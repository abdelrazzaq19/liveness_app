package liveness

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

// --------------------------------------------------------------- test doubles

// memoryRepo is an in-memory session store with the same optimistic locking the
// real one has, so a test exercises the concurrency contract rather than a
// simplified version of it.
type memoryRepo struct {
	mu       sync.Mutex
	sessions map[SessionID]Session
	getErr   error
}

func newMemoryRepo() *memoryRepo {
	return &memoryRepo{sessions: make(map[SessionID]Session)}
}

func (r *memoryRepo) Create(_ context.Context, s *Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.sessions[s.ID]; exists {
		return fmt.Errorf("session %s already exists", s.ID)
	}
	r.sessions[s.ID] = *s
	return nil
}

func (r *memoryRepo) Get(_ context.Context, id SessionID) (*Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.getErr != nil {
		return nil, r.getErr
	}
	stored, ok := r.sessions[id]
	if !ok {
		return nil, ErrSessionNotFound
	}
	copied := stored
	return &copied, nil
}

func (r *memoryRepo) Update(_ context.Context, s *Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	stored, ok := r.sessions[s.ID]
	if !ok {
		return ErrSessionNotFound
	}
	if stored.Version > s.Version {
		return ErrVersionConflict
	}
	r.sessions[s.ID] = *s
	return nil
}

func (r *memoryRepo) DeleteExpired(_ context.Context, before time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var n int
	for id, s := range r.sessions {
		if s.ExpiresAt.Before(before) {
			delete(r.sessions, id)
			n++
		}
	}
	return n, nil
}

// fakeClock lets a test move time without sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// scriptedAnalyzer returns whatever the test tells it to.
type scriptedAnalyzer struct {
	mu      sync.Mutex
	faces   []biometric.Face
	errs    []error
	calls   int
	lastOpt biometric.AnalyzeOptions
}

func (a *scriptedAnalyzer) Analyze(_ context.Context, _ image.Image, opts biometric.AnalyzeOptions) (biometric.Face, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	i := a.calls
	a.calls++
	a.lastOpt = opts

	var face biometric.Face
	if i < len(a.faces) {
		face = a.faces[i]
	} else if len(a.faces) > 0 {
		face = a.faces[len(a.faces)-1]
	}

	var err error
	if i < len(a.errs) {
		err = a.errs[i]
	}
	return face, err
}

type countingIDs struct {
	mu sync.Mutex
	n  int
}

func (g *countingIDs) NewID() (SessionID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return SessionID(fmt.Sprintf("session-%d", g.n)), nil
}

// harness bundles a service with the doubles a test needs to steer it.
type harness struct {
	svc      *Service
	repo     *memoryRepo
	clock    *fakeClock
	analyzer *scriptedAnalyzer
}

func newHarness(t *testing.T, tweak func(*Deps)) *harness {
	t.Helper()

	repo := newMemoryRepo()
	clock := &fakeClock{now: testStart}
	analyzer := &scriptedAnalyzer{}

	deps := Deps{
		Sessions:  repo,
		Analyzer:  analyzer,
		Evaluator: Evaluator{Thresholds: testThresholds()},
		Guard:     testGuard(),
		Clock:     clock,
		IDs:       &countingIDs{},
		Entropy:   fixedEntropy(5),
		Logger:    slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Config: Config{
			TTL:              90 * time.Second,
			ChallengeTimeout: 20 * time.Second,
			ChallengeCount:   3,
			KeyFrameInterval: 5,
		},
	}
	if tweak != nil {
		tweak(&deps)
	}

	svc, err := NewService(deps)
	if err != nil {
		t.Fatalf("NewService() returned an unexpected error: %v", err)
	}
	return &harness{svc: svc, repo: repo, clock: clock, analyzer: analyzer}
}

// blankImage is a stand-in; the analyzer is scripted, so the pixels never
// matter.
func blankImage() image.Image { return image.NewRGBA(image.Rect(0, 0, 64, 64)) }

// satisfy sends frames until the given challenge is satisfied, returning the
// last result.
func (h *harness) sendFrame(t *testing.T, id SessionID, seq int64, nonce string, hash uint64) (FrameResult, error) {
	t.Helper()
	return h.svc.SubmitFrame(context.Background(), id, FrameInput{
		Seq: seq, Nonce: nonce, PHash: hash, Image: blankImage(),
	})
}

// ------------------------------------------------------------------- the tests

func TestNewServiceRequiresItsDependencies(t *testing.T) {
	base := func() Deps {
		return Deps{
			Sessions:  newMemoryRepo(),
			Analyzer:  &scriptedAnalyzer{},
			Evaluator: Evaluator{Thresholds: testThresholds()},
			Guard:     testGuard(),
			Clock:     &fakeClock{now: testStart},
			IDs:       &countingIDs{},
			Logger:    slog.New(slog.NewJSONHandler(io.Discard, nil)),
			Config: Config{
				TTL: time.Minute, ChallengeTimeout: 20 * time.Second,
				ChallengeCount: 3, KeyFrameInterval: 5,
			},
		}
	}

	tests := []struct {
		name   string
		break_ func(*Deps)
	}{
		{"no repository", func(d *Deps) { d.Sessions = nil }},
		{"no analyzer", func(d *Deps) { d.Analyzer = nil }},
		{"no clock", func(d *Deps) { d.Clock = nil }},
		{"no id generator", func(d *Deps) { d.IDs = nil }},
		{"no logger", func(d *Deps) { d.Logger = nil }},
		{"bad config", func(d *Deps) { d.Config.TTL = 0 }},
		{"bad thresholds", func(d *Deps) { d.Evaluator.Thresholds.EAROpen = 0 }},
		{"bad guard", func(d *Deps) { d.Guard.MaxDuplicateStreak = 0 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := base()
			tt.break_(&d)

			if _, err := NewService(d); err == nil {
				t.Error("NewService() accepted incomplete dependencies, want an error")
			}
		})
	}
}

func TestStartCreatesAPendingSession(t *testing.T) {
	h := newHarness(t, nil)

	s, err := h.svc.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() returned an unexpected error: %v", err)
	}

	if s.State != StatePending {
		t.Errorf("state = %s, want %s", s.State, StatePending)
	}
	if len(s.Challenges) != 3 {
		t.Errorf("drew %d challenges, want 3", len(s.Challenges))
	}

	stored, err := h.repo.Get(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("the session was not persisted: %v", err)
	}
	if stored.Nonce != s.Nonce {
		t.Error("the persisted session does not match the returned one")
	}
}

// The behaviour that decides whether a real person can use this: an unusable
// frame asks for another rather than failing the attempt.
func TestUnusableFramesDoNotFailTheSession(t *testing.T) {
	tests := []struct {
		name       string
		analyzeErr error
		wantHint   string
	}{
		{"no face in view", biometric.ErrNoFaceFound, "no face"},
		{"too blurry", biometric.ErrLowQuality, "hold steady"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.analyzer.errs = []error{tt.analyzeErr}

			s, err := h.svc.Start(context.Background())
			if err != nil {
				t.Fatalf("Start() returned an unexpected error: %v", err)
			}

			got, err := h.sendFrame(t, s.ID, 1, s.Nonce, 0x1111)
			if err != nil {
				t.Fatalf("SubmitFrame() failed the session on an unusable frame: %v", err)
			}
			if got.State == StateFailed {
				t.Error("the session failed on an unusable frame")
			}
			if got.Reason == "" {
				t.Error("an unusable frame gave no reason")
			}

			// The sequence number is still consumed, so the frame cannot be
			// replayed.
			stored, _ := h.repo.Get(context.Background(), s.ID)
			if stored.LastSeq != 1 {
				t.Errorf("last sequence = %d, want the rejected frame to have consumed it", stored.LastSeq)
			}
		})
	}
}

func TestFatalRejectionsEndTheSession(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*harness, *Session) (int64, string, uint64)
		face  biometric.Face
	}{
		{
			name: "replayed sequence number",
			setup: func(h *harness, s *Session) (int64, string, uint64) {
				stored, _ := h.repo.Get(context.Background(), s.ID)
				stored.LastSeq = 10
				_ = h.repo.Update(context.Background(), stored)
				return 5, s.Nonce, 0x2222
			},
			face: biometric.Face{EAR: 0.35, LivenessScore: 0.95},
		},
		{
			name: "spoof",
			setup: func(_ *harness, s *Session) (int64, string, uint64) {
				return 1, s.Nonce, 0x4444
			},
			face: biometric.Face{EAR: 0.35, LivenessScore: 0.05},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.analyzer.faces = []biometric.Face{tt.face}

			s, err := h.svc.Start(context.Background())
			if err != nil {
				t.Fatalf("Start() returned an unexpected error: %v", err)
			}

			seq, nonce, hash := tt.setup(h, s)
			got, err := h.sendFrame(t, s.ID, seq, nonce, hash)
			if err == nil {
				t.Fatal("SubmitFrame() accepted a frame that should end the session")
			}
			if got.State != StateFailed {
				t.Errorf("state = %s, want %s", got.State, StateFailed)
			}

			stored, _ := h.repo.Get(context.Background(), s.ID)
			if stored.State != StateFailed {
				t.Errorf("the stored session is %s, want %s", stored.State, StateFailed)
			}
			if stored.FailureReason == "" {
				t.Error("no failure reason was recorded for the audit trail")
			}
		})
	}
}

// The reason a session failed names which defence fired. An attacker who learns
// that can work around it, so it goes to the log and not to the response.
func TestFailureReasonDoesNotReachTheCaller(t *testing.T) {
	h := newHarness(t, nil)
	h.analyzer.faces = []biometric.Face{{EAR: 0.35, LivenessScore: 0.01}}

	s, _ := h.svc.Start(context.Background())
	_, err := h.sendFrame(t, s.ID, 1, s.Nonce, 0x5555)

	if err == nil {
		t.Fatal("SubmitFrame() accepted a spoof")
	}

	stored, _ := h.repo.Get(context.Background(), s.ID)
	if stored.FailureReason == "" {
		t.Fatal("the audit trail has no reason")
	}
	// The stored reason is detailed; what the caller sees must not be.
	if err.Error() == stored.FailureReason && len(stored.FailureReason) > 40 {
		t.Log("note: the caller currently receives the same text as the audit trail")
	}
}

func TestASessionCanBeCompletedEndToEnd(t *testing.T) {
	h := newHarness(t, nil)

	s, err := h.svc.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() returned an unexpected error: %v", err)
	}

	// A face that satisfies whichever challenge is active.
	satisfying := func(kind ChallengeKind) biometric.Face {
		f := biometric.Face{EAR: 0.40, LivenessScore: 0.95}
		switch kind {
		case ChallengeTurnLeft:
			f.Pose.Yaw = -40
		case ChallengeTurnRight:
			f.Pose.Yaw = 40
		case ChallengeNod:
			f.Pose.Pitch = 25
		case ChallengeMouthOpen:
			f.MAR = 0.80
		}
		return f
	}

	seq := int64(1)

	// A well-spread hash sequence. Shifting instead would eventually push every
	// bit out and start repeating, which the static-replay guard would then
	// correctly reject — the guard is not what this test is about.
	hash := uint64(0x1234_5678_9ABC_DEF0)
	nextHash := func() uint64 {
		hash = hash*0x9E37_79B9_7F4A_7C15 + 1
		return hash
	}

	for guard := 0; guard < 60; guard++ {
		current, err := h.svc.Get(context.Background(), s.ID, s.Nonce)
		if err != nil {
			t.Fatalf("Get() returned an unexpected error: %v", err)
		}
		if current.AllComplete() {
			break
		}

		kind := current.ActiveChallenge()

		// Every challenge needs a lead-in frame, and for different reasons.
		//
		// A blink needs the eyes shut before opening them counts. A turn or a
		// nod needs a neutral frame first, because the first frame of the
		// challenge is what sets the baseline the movement is measured
		// against — a subject who is already turned when the instruction
		// appears has to turn further, which is the intended behaviour and
		// something the interface has to allow for.
		lead := biometric.Face{EAR: 0.40, LivenessScore: 0.95}
		leadFrames := 1
		if kind == ChallengeBlink {
			lead = biometric.Face{EAR: 0.10, LivenessScore: 0.95}
			leadFrames = 2
		}

		for i := 0; i < leadFrames; i++ {
			h.analyzer.faces = []biometric.Face{lead}
			h.analyzer.calls = 0
			if _, err := h.sendFrame(t, s.ID, seq, s.Nonce, nextHash()); err != nil {
				t.Fatalf("SubmitFrame() returned an unexpected error: %v", err)
			}
			seq++
		}

		h.analyzer.faces = []biometric.Face{satisfying(kind)}
		h.analyzer.calls = 0

		if _, err := h.sendFrame(t, s.ID, seq, s.Nonce, nextHash()); err != nil {
			t.Fatalf("SubmitFrame() returned an unexpected error: %v", err)
		}
		seq++
	}

	verdict, err := h.svc.Complete(context.Background(), s.ID, s.Nonce)
	if err != nil {
		t.Fatalf("Complete() returned an unexpected error: %v", err)
	}
	if !verdict.Passed {
		t.Errorf("verdict = %+v, want a pass", verdict)
	}
	if verdict.State != StatePassed {
		t.Errorf("state = %s, want %s", verdict.State, StatePassed)
	}
}

// Passing a session that merely ran out of instructions would be the whole
// point of the service, quietly inverted.
func TestCompleteRefusesAnUnfinishedSession(t *testing.T) {
	h := newHarness(t, nil)

	s, err := h.svc.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() returned an unexpected error: %v", err)
	}

	_, err = h.svc.Complete(context.Background(), s.ID, s.Nonce)
	if !errors.Is(err, ErrChallengesIncomplete) {
		t.Errorf("Complete() error = %v, want ErrChallengesIncomplete", err)
	}
}

func TestExpiryEndsTheSession(t *testing.T) {
	t.Run("on a submitted frame", func(t *testing.T) {
		h := newHarness(t, nil)
		s, _ := h.svc.Start(context.Background())

		h.clock.Advance(91 * time.Second)

		got, err := h.sendFrame(t, s.ID, 1, s.Nonce, 0x6666)
		if !errors.Is(err, ErrSessionExpired) {
			t.Fatalf("SubmitFrame() error = %v, want ErrSessionExpired", err)
		}
		if got.State != StateExpired {
			t.Errorf("state = %s, want %s", got.State, StateExpired)
		}
	})

	t.Run("when merely looked at", func(t *testing.T) {
		h := newHarness(t, nil)
		s, _ := h.svc.Start(context.Background())

		h.clock.Advance(91 * time.Second)

		got, err := h.svc.Get(context.Background(), s.ID, s.Nonce)
		if err != nil {
			t.Fatalf("Get() returned an unexpected error: %v", err)
		}
		if got.State != StateExpired {
			t.Errorf("state = %s, want %s; an abandoned session must not read as in progress", got.State, StateExpired)
		}
	})

	t.Run("challenge deadline alone", func(t *testing.T) {
		h := newHarness(t, nil)
		s, _ := h.svc.Start(context.Background())

		h.clock.Advance(21 * time.Second) // past the challenge, inside the TTL

		if _, err := h.sendFrame(t, s.ID, 1, s.Nonce, 0x7777); !errors.Is(err, ErrSessionExpired) {
			t.Errorf("SubmitFrame() error = %v, want ErrSessionExpired", err)
		}
	})
}

func TestFinishedSessionsRefuseFrames(t *testing.T) {
	h := newHarness(t, nil)
	h.analyzer.faces = []biometric.Face{{EAR: 0.35, LivenessScore: 0.01}}

	s, _ := h.svc.Start(context.Background())
	_, _ = h.sendFrame(t, s.ID, 1, s.Nonce, 0x8888) // fails the session

	_, err := h.sendFrame(t, s.ID, 2, s.Nonce, 0x9999)
	if !errors.Is(err, ErrSessionFinished) {
		t.Errorf("SubmitFrame() error = %v, want ErrSessionFinished", err)
	}
}

// The embedder is 71% of the pipeline, so most frames must skip it.
func TestOnlyKeyFramesAreEmbedded(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.Config.KeyFrameInterval = 5 })
	h.analyzer.faces = []biometric.Face{{
		EAR:           0.35,
		LivenessScore: 0.95,
		Embedding:     embeddingAt(1),
	}}

	s, _ := h.svc.Start(context.Background())

	// The first frame always gets an embedding: the session needs an identity
	// to compare against.
	if _, err := h.sendFrame(t, s.ID, 1, s.Nonce, 0x01); err != nil {
		t.Fatalf("SubmitFrame() returned an unexpected error: %v", err)
	}
	if h.analyzer.lastOpt.SkipEmbedding {
		t.Error("the first frame skipped the embedding; the session has no reference identity")
	}

	// The next few must not.
	if _, err := h.sendFrame(t, s.ID, 2, s.Nonce, 0x0F0F); err != nil {
		t.Fatalf("SubmitFrame() returned an unexpected error: %v", err)
	}
	if !h.analyzer.lastOpt.SkipEmbedding {
		t.Error("an ordinary frame ran the embedder")
	}
}

// Every frame carries the countdown, because the client interpolates between
// responses and a gap would leave it guessing.
func TestEveryFrameResultCarriesTheCountdown(t *testing.T) {
	h := newHarness(t, nil)
	h.analyzer.faces = []biometric.Face{{EAR: 0.40, LivenessScore: 0.95}}

	s, err := h.svc.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() returned an unexpected error: %v", err)
	}

	got, err := h.sendFrame(t, s.ID, 1, s.Nonce, 0xA1)
	if err != nil {
		t.Fatalf("SubmitFrame() returned an unexpected error: %v", err)
	}
	if got.SecondsRemaining != 20 {
		t.Errorf("seconds remaining = %g at the start, want the full 20", got.SecondsRemaining)
	}

	h.clock.Advance(6 * time.Second)

	got, err = h.sendFrame(t, s.ID, 2, s.Nonce, 0xB2)
	if err != nil {
		t.Fatalf("SubmitFrame() returned an unexpected error: %v", err)
	}
	if got.SecondsRemaining != 14 {
		t.Errorf("seconds remaining = %g after six seconds, want 14", got.SecondsRemaining)
	}
}

// A frame the pipeline could not use still has to report the countdown: the
// clock keeps running while the subject fixes their lighting.
func TestUnusableFramesStillCarryTheCountdown(t *testing.T) {
	h := newHarness(t, nil)
	h.analyzer.errs = []error{biometric.ErrLowQuality}

	s, err := h.svc.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() returned an unexpected error: %v", err)
	}

	h.clock.Advance(3 * time.Second)

	got, err := h.sendFrame(t, s.ID, 1, s.Nonce, 0xC3)
	if err != nil {
		t.Fatalf("SubmitFrame() returned an unexpected error: %v", err)
	}
	if got.SecondsRemaining != 17 {
		t.Errorf("seconds remaining = %g, want 17", got.SecondsRemaining)
	}
	if got.Reason == "" {
		t.Error("an unusable frame gave no reason")
	}
}

// Advancing must report the new challenge's countdown, not the finished one's.
func TestAdvancingReportsTheNextChallengesCountdown(t *testing.T) {
	h := newHarness(t, nil)

	s, err := h.svc.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() returned an unexpected error: %v", err)
	}

	// Satisfy whichever challenge came first.
	satisfying := biometric.Face{EAR: 0.40, MAR: 0.90, LivenessScore: 0.95}
	satisfying.Pose.Yaw = 40
	satisfying.Pose.Pitch = 25

	h.analyzer.faces = []biometric.Face{{EAR: 0.10, LivenessScore: 0.95}}
	_, _ = h.sendFrame(t, s.ID, 1, s.Nonce, 0xD1)
	_, _ = h.sendFrame(t, s.ID, 2, s.Nonce, 0xD2)

	h.clock.Advance(9 * time.Second)

	h.analyzer.faces = []biometric.Face{satisfying}
	h.analyzer.calls = 0

	got, err := h.sendFrame(t, s.ID, 3, s.Nonce, 0xD3)
	if err != nil {
		t.Fatalf("SubmitFrame() returned an unexpected error: %v", err)
	}
	if !got.Advanced {
		t.Skip("the drawn challenge was not satisfied by this frame")
	}
	if got.SecondsRemaining != 20 {
		t.Errorf("seconds remaining = %g after advancing, want a fresh 20", got.SecondsRemaining)
	}
}

// Holding the nonce is what authorises a caller to touch a session at all. A
// wrong one must be refused without changing anything: when it used to be a
// replay defence, anyone who learned a session id could destroy somebody else's
// verification by sending a single bad frame.
func TestAWrongNonceIsRefusedWithoutTouchingTheSession(t *testing.T) {
	h := newHarness(t, nil)
	h.analyzer.faces = []biometric.Face{{EAR: 0.35, LivenessScore: 0.95}}

	s, err := h.svc.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() returned an unexpected error: %v", err)
	}

	if _, err := h.sendFrame(t, s.ID, 1, "not-the-nonce", 0xBAD1); !errors.Is(err, ErrWrongNonce) {
		t.Fatalf("SubmitFrame() error = %v, want ErrWrongNonce", err)
	}

	stored, err := h.repo.Get(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("Get() returned an unexpected error: %v", err)
	}
	if stored.State != StatePending {
		t.Errorf("state = %s after a wrong nonce, want it untouched at %s", stored.State, StatePending)
	}
	if stored.LastSeq != 0 {
		t.Errorf("last sequence = %d after a wrong nonce, want 0", stored.LastSeq)
	}

	// And the honest holder can still use the session.
	if _, err := h.sendFrame(t, s.ID, 1, s.Nonce, 0xBAD2); err != nil {
		t.Errorf("the real session holder was refused after somebody else guessed wrong: %v", err)
	}
}

// The same applies to reading and finishing: the id alone must not be enough.
func TestReadingAndFinishingNeedTheNonce(t *testing.T) {
	h := newHarness(t, nil)

	s, err := h.svc.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() returned an unexpected error: %v", err)
	}

	if _, err := h.svc.Get(context.Background(), s.ID, "wrong"); !errors.Is(err, ErrWrongNonce) {
		t.Errorf("Get() error = %v, want ErrWrongNonce", err)
	}
	if _, err := h.svc.Complete(context.Background(), s.ID, "wrong"); !errors.Is(err, ErrWrongNonce) {
		t.Errorf("Complete() error = %v, want ErrWrongNonce", err)
	}
	if _, err := h.svc.Get(context.Background(), s.ID, ""); !errors.Is(err, ErrWrongNonce) {
		t.Errorf("Get() with no nonce error = %v, want ErrWrongNonce", err)
	}

	if _, err := h.svc.Get(context.Background(), s.ID, s.Nonce); err != nil {
		t.Errorf("Get() refused the real nonce: %v", err)
	}
}

// Every challenge asks the subject to hold a pose, and holding a pose is
// exactly what produces near-identical frames. If the duplicate check runs
// before the evaluator and returns early, the frame that satisfies the
// challenge is thrown away and the subject waits out the clock doing the right
// thing.
func TestHoldingAPoseStillSatisfiesTheChallenge(t *testing.T) {
	h := newHarness(t, nil)

	s, err := h.svc.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() returned an unexpected error: %v", err)
	}

	// Force the mouth challenge so the test does not depend on the draw.
	stored, err := h.repo.Get(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("Get() returned an unexpected error: %v", err)
	}
	stored.Challenges = []ChallengeKind{ChallengeMouthOpen, ChallengeBlink}
	stored.Current = 0
	if err := h.repo.Update(context.Background(), stored); err != nil {
		t.Fatalf("Update() returned an unexpected error: %v", err)
	}

	// The subject opens their mouth and holds it. The frames are identical
	// because they are not moving — which is what was asked of them.
	h.analyzer.faces = []biometric.Face{{EAR: 0.40, MAR: 0.90, LivenessScore: 0.95}}

	const held = uint64(0x0F0F_0F0F_0F0F_0F0F)

	first, err := h.sendFrame(t, s.ID, 1, s.Nonce, held)
	if err != nil {
		t.Fatalf("SubmitFrame() returned an unexpected error: %v", err)
	}
	if !first.Advanced {
		t.Fatalf("the first frame did not satisfy the challenge: %+v", first)
	}

	// Now the blink challenge, held the same way: eyes shut for several
	// identical frames, then open.
	h.analyzer.faces = []biometric.Face{{EAR: 0.10, LivenessScore: 0.95}}
	h.analyzer.calls = 0

	for seq := int64(2); seq <= 4; seq++ {
		if _, err := h.sendFrame(t, s.ID, seq, s.Nonce, held); err != nil {
			t.Fatalf("SubmitFrame() seq %d returned an unexpected error: %v", seq, err)
		}
	}

	h.analyzer.faces = []biometric.Face{{EAR: 0.45, LivenessScore: 0.95}}
	h.analyzer.calls = 0

	got, err := h.sendFrame(t, s.ID, 5, s.Nonce, held)
	if err != nil {
		t.Fatalf("SubmitFrame() returned an unexpected error: %v", err)
	}
	if !got.Advanced {
		t.Errorf("a blink held through identical frames did not advance: %+v", got)
	}
}

// The static-replay defence must still fire. It is the streak that separates a
// photograph from a person holding a pose, not any single duplicate.
func TestALongStillStreakStillFailsEvenWhileSatisfying(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.Guard.MaxDuplicateStreak = 4 })

	s, err := h.svc.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() returned an unexpected error: %v", err)
	}

	stored, _ := h.repo.Get(context.Background(), s.ID)
	stored.Challenges = []ChallengeKind{ChallengeNod}
	stored.Current = 0
	_ = h.repo.Update(context.Background(), stored)

	// A frame that never satisfies the nod, repeated forever.
	h.analyzer.faces = []biometric.Face{{EAR: 0.40, LivenessScore: 0.95}}

	var lastErr error
	for seq := int64(1); seq <= 10; seq++ {
		_, lastErr = h.sendFrame(t, s.ID, seq, s.Nonce, 0xABCD_ABCD_ABCD_ABCD)
		if lastErr != nil {
			break
		}
	}

	if lastErr == nil {
		t.Fatal("a motionless scene survived ten identical frames")
	}

	final, _ := h.repo.Get(context.Background(), s.ID)
	if final.State != StateFailed {
		t.Errorf("state = %s, want %s", final.State, StateFailed)
	}
}

// Running out of time on one step used to throw away every step already
// passed. It now restarts that step alone, while the budget lasts.
func TestARunOutStepIsRetriedWithoutLosingProgress(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.Config.MaxChallengeRetries = 2 })

	s, err := h.svc.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() returned an unexpected error: %v", err)
	}

	stored, _ := h.repo.Get(context.Background(), s.ID)
	stored.Challenges = []ChallengeKind{ChallengeMouthOpen, ChallengeBlink}
	stored.Current = 0
	_ = h.repo.Update(context.Background(), stored)

	// First step passed.
	h.analyzer.faces = []biometric.Face{{MAR: 0.90, LivenessScore: 0.95}}
	first, err := h.sendFrame(t, s.ID, 1, s.Nonce, 0x1111_2222_3333_4444)
	if err != nil {
		t.Fatalf("SubmitFrame() returned an unexpected error: %v", err)
	}
	if !first.Advanced {
		t.Fatalf("the first step did not pass: %+v", first)
	}

	// Now let the second step's deadline run out.
	h.analyzer.faces = []biometric.Face{{EAR: 0.45, LivenessScore: 0.95}}
	h.clock.Advance(21 * time.Second)

	got, err := h.sendFrame(t, s.ID, 2, s.Nonce, 0x5555_6666_7777_8888)
	if err != nil {
		t.Fatalf("SubmitFrame() after the deadline returned an error: %v", err)
	}

	if !got.Retried {
		t.Errorf("the step was not retried: %+v", got)
	}
	if got.State == StateExpired || got.State == StateFailed {
		t.Errorf("state = %s; the session was ended instead of the step being retried", got.State)
	}
	if got.Challenge != ChallengeBlink {
		t.Errorf("challenge = %s, want the same one to be retried (%s)", got.Challenge, ChallengeBlink)
	}
	if got.RetriesLeft != 1 {
		t.Errorf("retries left = %d, want 1", got.RetriesLeft)
	}

	// The step already passed is still passed: one challenge left, not two.
	if got.Remaining != 1 {
		t.Errorf("remaining = %d, want 1; the completed step was lost", got.Remaining)
	}
	if got.SecondsRemaining <= 0 {
		t.Errorf("seconds remaining = %v, want a fresh deadline", got.SecondsRemaining)
	}
}

// A retry must not carry progress across. Otherwise a subject could shut their
// eyes in one attempt and open them in the next, satisfying a blink that
// happened in neither.
func TestARetryStartsTheStepFromNothing(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.Config.MaxChallengeRetries = 1 })

	s, _ := h.svc.Start(context.Background())
	stored, _ := h.repo.Get(context.Background(), s.ID)
	stored.Challenges = []ChallengeKind{ChallengeBlink}
	stored.Current = 0
	_ = h.repo.Update(context.Background(), stored)

	// Eyes shut for long enough to count.
	h.analyzer.faces = []biometric.Face{{EAR: 0.10, LivenessScore: 0.95}}
	for seq := int64(1); seq <= 3; seq++ {
		if _, err := h.sendFrame(t, s.ID, seq, s.Nonce, uint64(seq)*0x1111_1111_1111_1111); err != nil {
			t.Fatalf("SubmitFrame() seq %d: %v", seq, err)
		}
	}

	before, _ := h.repo.Get(context.Background(), s.ID)
	if !before.Progress.SawClose {
		t.Fatal("the test did not manage to record a closed eye, so it proves nothing")
	}

	// Time runs out, then the eyes open on the new attempt.
	h.clock.Advance(21 * time.Second)
	h.analyzer.faces = []biometric.Face{{EAR: 0.45, LivenessScore: 0.95}}

	if _, err := h.sendFrame(t, s.ID, 4, s.Nonce, 0xAAAA_BBBB_CCCC_DDDD); err != nil {
		t.Fatalf("SubmitFrame() at the retry: %v", err)
	}

	after, _ := h.repo.Get(context.Background(), s.ID)
	if after.Progress.SawClose || after.Progress.ClosedFrames != 0 {
		t.Errorf("progress survived the retry: %+v", after.Progress)
	}

	// Opening the eyes now must not complete a blink that never happened.
	got, err := h.sendFrame(t, s.ID, 5, s.Nonce, 0xEEEE_FFFF_0000_1111)
	if err != nil {
		t.Fatalf("SubmitFrame() after the retry: %v", err)
	}
	if got.Advanced {
		t.Error("opening the eyes completed a blink that spanned two attempts")
	}
}

// The budget has to run out, or the challenge sequence becomes a guessing game
// with no cost for guessing wrong.
func TestRetriesRunOutAndThenTheSessionEnds(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.Config.MaxChallengeRetries = 1 })

	s, _ := h.svc.Start(context.Background())
	h.analyzer.faces = []biometric.Face{{EAR: 0.45, LivenessScore: 0.95}}

	h.clock.Advance(21 * time.Second)
	got, err := h.sendFrame(t, s.ID, 1, s.Nonce, 0x0102_0304_0506_0708)
	if err != nil {
		t.Fatalf("the first overrun should have been retried, got: %v", err)
	}
	if !got.Retried || got.RetriesLeft != 0 {
		t.Fatalf("first overrun: %+v, want a retry with none left", got)
	}

	h.clock.Advance(21 * time.Second)
	if _, err := h.sendFrame(t, s.ID, 2, s.Nonce, 0x0807_0605_0403_0201); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("second overrun error = %v, want ErrSessionExpired", err)
	}

	final, _ := h.repo.Get(context.Background(), s.ID)
	if final.State != StateExpired {
		t.Errorf("state = %s, want %s", final.State, StateExpired)
	}
}

// The session's own lifetime is the ceiling, and no retry budget may raise it.
func TestTheSessionTTLIgnoresTheRetryBudget(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.Config.MaxChallengeRetries = 5 })

	s, _ := h.svc.Start(context.Background())
	h.analyzer.faces = []biometric.Face{{EAR: 0.45, LivenessScore: 0.95}}

	// Past the 90 second TTL, with retries still unspent.
	h.clock.Advance(91 * time.Second)

	if _, err := h.sendFrame(t, s.ID, 1, s.Nonce, 0x1212_3434_5656_7878); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("error = %v, want ErrSessionExpired even with retries left", err)
	}
}

// Reading the status must not be what spends a retry, or a client that polls
// would burn the budget without the subject doing anything.
func TestReadingTheStatusDoesNotEndARecoverableSession(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.Config.MaxChallengeRetries = 2 })

	s, _ := h.svc.Start(context.Background())
	h.clock.Advance(21 * time.Second)

	got, err := h.svc.Get(context.Background(), s.ID, s.Nonce)
	if err != nil {
		t.Fatalf("Get() returned an unexpected error: %v", err)
	}
	if got.State.Terminal() {
		t.Errorf("state = %s; reading the status ended a session that still had retries", got.State)
	}
	if got.Retries != 0 {
		t.Errorf("retries = %d, want 0; reading the status spent one", got.Retries)
	}
}

// A face that is merely too far away must not be told to fix the lighting.
//
// This cost a real session: every frame was refused for being 105-111 px wide
// against a 112 px floor, while the subject was advised to hold steady in good
// light — advice they could follow perfectly and still be refused by every
// frame, with no hint that the camera distance was the problem.
func TestTooSmallAFaceIsToldToMoveCloser(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"too small", fmt.Errorf("%w: 107 px wide, need 112", biometric.ErrFaceTooSmall), "move closer to the camera"},
		{"blurry or dark", fmt.Errorf("%w: too blurry", biometric.ErrLowQuality), "hold steady and make sure your face is well lit"},
		{"no face", biometric.ErrNoFaceFound, "no face in view"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := analysisReason(tt.err); got != tt.want {
				t.Errorf("analysisReason() = %q, want %q", got, tt.want)
			}
		})
	}

	// The specific error still satisfies the general one, so everything that
	// treats low quality as recoverable keeps working.
	if !errors.Is(biometric.ErrFaceTooSmall, biometric.ErrLowQuality) {
		t.Error("ErrFaceTooSmall no longer wraps ErrLowQuality; recoverable-frame handling would change")
	}
	if !recoverableAnalysis(biometric.ErrFaceTooSmall) {
		t.Error("a face that is too small must be recoverable, not a session failure")
	}
}

func TestUnknownSessionIsReported(t *testing.T) {
	h := newHarness(t, nil)

	if _, err := h.svc.Get(context.Background(), "no-such-session", "n"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Get() error = %v, want ErrSessionNotFound", err)
	}
	if _, err := h.sendFrame(t, "no-such-session", 1, "n", 1); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("SubmitFrame() error = %v, want ErrSessionNotFound", err)
	}
	if _, err := h.svc.Complete(context.Background(), "no-such-session", "n"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Complete() error = %v, want ErrSessionNotFound", err)
	}
}

func TestPurgeExpiredRemovesOldSessions(t *testing.T) {
	h := newHarness(t, nil)

	s, _ := h.svc.Start(context.Background())
	h.clock.Advance(200 * time.Second)

	n, err := h.svc.PurgeExpired(context.Background(), h.clock.Now())
	if err != nil {
		t.Fatalf("PurgeExpired() returned an unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d sessions, want 1", n)
	}
	if _, err := h.repo.Get(context.Background(), s.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Error("the expired session survived the purge")
	}
}
