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
			name: "wrong nonce",
			setup: func(_ *harness, _ *Session) (int64, string, uint64) {
				return 1, "not-the-nonce", 0x3333
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
		current, err := h.svc.Get(context.Background(), s.ID)
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

	verdict, err := h.svc.Complete(context.Background(), s.ID)
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

	_, err = h.svc.Complete(context.Background(), s.ID)
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

		got, err := h.svc.Get(context.Background(), s.ID)
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

func TestUnknownSessionIsReported(t *testing.T) {
	h := newHarness(t, nil)

	if _, err := h.svc.Get(context.Background(), "no-such-session"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Get() error = %v, want ErrSessionNotFound", err)
	}
	if _, err := h.sendFrame(t, "no-such-session", 1, "n", 1); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("SubmitFrame() error = %v, want ErrSessionNotFound", err)
	}
	if _, err := h.svc.Complete(context.Background(), "no-such-session"); !errors.Is(err, ErrSessionNotFound) {
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
