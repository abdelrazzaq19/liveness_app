// Package attack is the regression suite for the defences.
//
// Every test here is an attack that must be refused, driven through the real
// HTTP router against the real service, guard, and evaluator. Only the
// biometrics are stubbed, and deliberately so: the stub derives its output from
// the pixels it is given, which makes an attack reproducible without a face.
//
// It runs in the ordinary suite rather than behind a build tag. A defence that
// is only checked when somebody remembers to pass a flag is a defence that
// stops being checked.
//
// What is NOT here, and cannot be until Open Question #3 is answered: a printed
// photograph and a screen replay of a real person. Those need labelled captures
// of real faces, which this repository is not allowed to hold. The passive
// anti-spoof model they would exercise is also switched off, for reasons in
// SPEC §5. Both gaps are named in tasks/todo.md rather than papered over with a
// synthetic image that would pass for the wrong reason.
package attack

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ziad/liveness-verifier/internal/biometric/stub"
	"github.com/ziad/liveness-verifier/internal/config"
	"github.com/ziad/liveness-verifier/internal/httpapi"
	"github.com/ziad/liveness-verifier/internal/liveness"
)

// ------------------------------------------------------------------- harness

// memoryRepo is the session store, with the same optimistic locking the real
// one has so a test exercises the concurrency contract rather than a simpler
// version of it.
type memoryRepo struct {
	mu       sync.Mutex
	sessions map[liveness.SessionID]liveness.Session
}

func newMemoryRepo() *memoryRepo {
	return &memoryRepo{sessions: map[liveness.SessionID]liveness.Session{}}
}

func (r *memoryRepo) Create(_ context.Context, s *liveness.Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[s.ID] = *s
	return nil
}

func (r *memoryRepo) Get(_ context.Context, id liveness.SessionID) (*liveness.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return nil, liveness.ErrSessionNotFound
	}
	copied := s
	return &copied, nil
}

func (r *memoryRepo) Update(_ context.Context, s *liveness.Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, ok := r.sessions[s.ID]
	if !ok {
		return liveness.ErrSessionNotFound
	}
	if stored.Version > s.Version {
		return liveness.ErrVersionConflict
	}
	r.sessions[s.ID] = *s
	return nil
}

func (r *memoryRepo) DeleteExpired(context.Context, time.Time) (int, error) { return 0, nil }

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type ids struct {
	mu sync.Mutex
	n  int
}

func (g *ids) NewID() (liveness.SessionID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return liveness.SessionID(fmt.Sprintf("session-%d", g.n)), nil
}

const apiKey = "operator-key"

type rig struct {
	router http.Handler
	repo   *memoryRepo
	clock  *clock
}

func newRig(t *testing.T, tweak func(*liveness.Deps)) *rig {
	t.Helper()

	repo := newMemoryRepo()
	clk := &clock{now: time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)}

	deps := liveness.Deps{
		Sessions: repo,
		Analyzer: &stub.Pipeline{},
		Evaluator: liveness.Evaluator{Thresholds: liveness.Thresholds{
			BlinkCloseRatio: 0.60,
			BlinkOpenRatio:  0.85,
			BlinkMinFrames:  2,
			YawTurnDeg:      15,
			PitchNodDeg:     15,
			MARMouthOpen:    0.55,
		}},
		Guard: liveness.Guard{
			MinLivenessScore:   0.0, // the stub is not an anti-spoof model
			EnforceAntiSpoof:   true,
			IdentityCosineMin:  0.70,
			PHashMinDistance:   5,
			MaxDuplicateStreak: 4,
			MaxRecentHashes:    64,
		},
		Clock: clk,
		IDs:   &ids{},
		// Varied rather than a constant byte: the challenge generator draws from
		// this, and a source that repeats one value cannot produce a permutation.
		Entropy: randomEntropy(1),
		Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Config: liveness.Config{
			TTL:                 90 * time.Second,
			ChallengeTimeout:    5 * time.Second,
			ChallengeCount:      3,
			MaxChallengeRetries: 0,
			KeyFrameInterval:    5,
		},
	}
	if tweak != nil {
		tweak(&deps)
	}

	svc, err := liveness.NewService(deps)
	if err != nil {
		t.Fatalf("NewService() returned an unexpected error: %v", err)
	}

	router, err := httpapi.NewRouter(httpapi.Deps{
		Config: &config.Config{
			Server: config.Server{
				Addr:            ":8080",
				RequestTimeout:  5 * time.Second,
				ShutdownTimeout: time.Second,
				APIKeys:         []config.Secret{apiKey},
				RateLimitPerMin: 100_000,
				MaxFrameBytes:   2 << 20,
			},
			Imaging: config.Imaging{MaxDecodedPixels: 16_000_000},
			Log:     config.Log{Level: slog.LevelError, Format: "json"},
		},
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Liveness: svc,
	})
	if err != nil {
		t.Fatalf("NewRouter() returned an unexpected error: %v", err)
	}

	return &rig{router: router, repo: repo, clock: clk}
}

func (r *rig) call(t *testing.T, method, path string, body any, headers map[string]string) (int, []byte) {
	t.Helper()

	var reader io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("could not encode the request: %v", err)
		}
		reader = bytes.NewReader(raw)
	}

	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

type startedSession struct {
	ID         string   `json:"session_id"`
	Nonce      string   `json:"nonce"`
	Challenges []string `json:"challenges"`
}

func (r *rig) start(t *testing.T) startedSession {
	t.Helper()

	code, body := r.call(t, http.MethodPost, "/v1/liveness/sessions", nil, nil)
	if code != http.StatusCreated {
		t.Fatalf("starting a session: status %d: %s", code, body)
	}

	var s startedSession
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("session response is not valid JSON: %v", err)
	}
	return s
}

func (r *rig) frame(t *testing.T, s startedSession, seq int, img image.Image) (int, []byte) {
	t.Helper()

	return r.call(t, http.MethodPost, "/v1/liveness/sessions/"+s.ID+"/frames",
		map[string]any{"seq": seq, "nonce": s.Nonce, "frame": encodeJPEG(t, img)},
		map[string]string{"X-Session-Nonce": s.Nonce})
}

func encodeJPEG(t *testing.T, img image.Image) string {
	t.Helper()

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("could not encode a frame: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// scene builds a frame the stub can read. seed shifts the content, so two
// scenes are as different to the pipeline as two moments are.
func scene(seed float64) image.Image {
	const w, h = 320, 400

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		fy := float64(y) / h
		for x := 0; x < w; x++ {
			fx := float64(x) / w
			v := 0.5 + 0.25*math.Sin(2*math.Pi*(3*fx+seed)) + 0.2*math.Sin(2*math.Pi*(5*fy+seed))
			g := uint8(math.Max(0, math.Min(255, 30+v*180)))
			img.SetRGBA(x, y, color.RGBA{R: g, G: g, B: g, A: 255})
		}
	}
	return img
}

func (r *rig) state(t *testing.T, s startedSession) string {
	t.Helper()

	code, body := r.call(t, http.MethodGet, "/v1/liveness/sessions/"+s.ID, nil,
		map[string]string{"X-Session-Nonce": s.Nonce})
	if code != http.StatusOK {
		// A terminal session still reports its state through the repository.
		stored, err := r.repo.Get(context.Background(), liveness.SessionID(s.ID))
		if err != nil {
			t.Fatalf("could not read the session: %v", err)
		}
		return string(stored.State)
	}

	var got struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("status response is not valid JSON: %v", err)
	}
	return got.State
}

// --------------------------------------------------------------- the attacks

// Replaying a captured request. The sequence number is what makes one frame
// distinguishable from the same frame sent again.
func TestReplayedSequenceNumberEndsTheSession(t *testing.T) {
	r := newRig(t, nil)
	s := r.start(t)

	if code, body := r.frame(t, s, 1, scene(0)); code != http.StatusOK {
		t.Fatalf("the first frame was refused: %d %s", code, body)
	}

	// The same sequence number again, which is what a captured request looks
	// like when it is sent twice.
	code, _ := r.frame(t, s, 1, scene(0.2))
	if code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d for a replayed sequence number", code, http.StatusUnprocessableEntity)
	}
	if got := r.state(t, s); got != string(liveness.StateFailed) {
		t.Errorf("session state = %s, want %s", got, liveness.StateFailed)
	}
}

func TestSequenceGoingBackwardsEndsTheSession(t *testing.T) {
	r := newRig(t, nil)
	s := r.start(t)

	for seq, seed := range []float64{0, 0.2, 0.4} {
		if code, body := r.frame(t, s, seq+1, scene(seed)); code != http.StatusOK {
			t.Fatalf("frame %d was refused: %d %s", seq+1, code, body)
		}
	}

	code, _ := r.frame(t, s, 2, scene(0.6))
	if code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d for a sequence number going backwards", code, http.StatusUnprocessableEntity)
	}
}

// Holding a photograph in front of the camera. What separates it from a person
// sitting still is that a person eventually moves; the streak is the measure.
func TestAStillImageHeldTooLongEndsTheSession(t *testing.T) {
	r := newRig(t, nil)
	s := r.start(t)

	still := scene(0.33)

	var lastCode int
	for seq := 1; seq <= 8; seq++ {
		code, _ := r.frame(t, s, seq, still)
		lastCode = code
		if code != http.StatusOK {
			break
		}
	}

	if lastCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d after eight identical frames, want %d", lastCode, http.StatusUnprocessableEntity)
	}
	if got := r.state(t, s); got != string(liveness.StateFailed) {
		t.Errorf("session state = %s, want %s", got, liveness.StateFailed)
	}
}

// A subject who holds still briefly is not an attacker. This is the other half
// of the previous test, and the one that decides whether real people can use
// the system.
func TestHoldingStillBrieflyIsNotAnAttack(t *testing.T) {
	r := newRig(t, func(d *liveness.Deps) { d.Guard.MaxDuplicateStreak = 12 })
	s := r.start(t)

	still := scene(0.33)
	for seq := 1; seq <= 6; seq++ {
		if code, body := r.frame(t, s, seq, still); code != http.StatusOK {
			t.Fatalf("frame %d refused an honest subject holding still: %d %s", seq, code, body)
		}
	}

	if got := r.state(t, s); got == string(liveness.StateFailed) {
		t.Error("a subject who held still for a second was failed")
	}
}

// Knowing a session id must not be enough to do anything with it.
func TestKnowingTheSessionIdIsNotEnough(t *testing.T) {
	r := newRig(t, nil)
	s := r.start(t)

	calls := []struct {
		name    string
		method  string
		path    string
		body    any
		headers map[string]string
	}{
		{
			"read status", http.MethodGet, "/v1/liveness/sessions/" + s.ID, nil,
			map[string]string{"X-Session-Nonce": "wrong-nonce"},
		},
		{
			"finish it", http.MethodPost, "/v1/liveness/sessions/" + s.ID + "/complete", nil,
			map[string]string{"X-Session-Nonce": "wrong-nonce"},
		},
		{
			"submit a frame", http.MethodPost, "/v1/liveness/sessions/" + s.ID + "/frames",
			map[string]any{"seq": 1, "nonce": "wrong-nonce", "frame": encodeJPEG(t, scene(0))},
			map[string]string{"X-Session-Nonce": "wrong-nonce"},
		},
	}

	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			code, _ := r.call(t, c.method, c.path, c.body, c.headers)
			if code != http.StatusForbidden {
				t.Errorf("status = %d, want %d", code, http.StatusForbidden)
			}
		})
	}

	// And none of it touched the session. This is the part that matters: a
	// wrong nonce used to fail the session, so anyone who learned an id could
	// destroy somebody else's verification with one bad request.
	if got := r.state(t, s); got == string(liveness.StateFailed) {
		t.Error("a stranger with the session id destroyed the session")
	}
}

// A session cannot be finished before its challenges are.
func TestASessionCannotBeCompletedEarly(t *testing.T) {
	r := newRig(t, nil)
	s := r.start(t)

	code, _ := r.call(t, http.MethodPost, "/v1/liveness/sessions/"+s.ID+"/complete", nil,
		map[string]string{"X-Session-Nonce": s.Nonce})

	if code != http.StatusConflict {
		t.Errorf("status = %d, want %d for a session with challenges outstanding", code, http.StatusConflict)
	}
	if got := r.state(t, s); got == string(liveness.StatePassed) {
		t.Error("an unfinished session was marked as passed")
	}
}

// Waiting out the clock is not a way through.
func TestAnExpiredSessionCannotBeUsed(t *testing.T) {
	r := newRig(t, nil)
	s := r.start(t)

	r.clock.advance(91 * time.Second)

	code, _ := r.frame(t, s, 1, scene(0))
	if code != http.StatusGone {
		t.Errorf("status = %d, want %d for an expired session", code, http.StatusGone)
	}
}

// The challenge order is drawn per session, so a recording of one session is
// useless against the next.
func TestChallengeOrderIsNotFixedAcrossSessions(t *testing.T) {
	seen := map[string]int{}

	// A fresh rig each time, because the entropy source is per service.
	for i := 0; i < 24; i++ {
		r := newRig(t, func(d *liveness.Deps) {
			d.Entropy = randomEntropy(int64(i))
		})
		s := r.start(t)
		seen[fmt.Sprint(s.Challenges)]++
	}

	if len(seen) < 2 {
		t.Errorf("24 sessions produced %d distinct challenge orders: a recording of one would fit them all", len(seen))
	}
}

// randomEntropy returns a deterministic but varying source, so the test is
// reproducible while the orders it produces still differ.
func randomEntropy(seed int64) io.Reader {
	b := make([]byte, 4096)
	x := uint64(seed)*6364136223846793005 + 1442695040888963407
	for i := range b {
		x = x*6364136223846793005 + 1442695040888963407
		b[i] = byte(x >> 33)
	}
	return bytes.NewReader(b)
}

// Session creation is an operator call. Without a key it must not allocate a
// database row or a slot of inference work.
func TestSessionCreationNeedsTheOperatorKey(t *testing.T) {
	r := newRig(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/liveness/sessions", http.NoBody)
	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d without an API key", rec.Code, http.StatusUnauthorized)
	}
}

// Every refusal on the verification path looks the same from outside. A client
// that could tell which defence fired learns which one to work around next.
func TestRefusalsDoNotNameTheDefenceThatFired(t *testing.T) {
	messages := map[string]bool{}

	for _, attack := range []func(t *testing.T) (int, []byte){
		func(t *testing.T) (int, []byte) {
			r := newRig(t, nil)
			s := r.start(t)
			r.frame(t, s, 1, scene(0))
			return r.frame(t, s, 1, scene(0.2)) // replayed sequence
		},
		func(t *testing.T) (int, []byte) {
			r := newRig(t, nil)
			s := r.start(t)
			still := scene(0.33)
			var code int
			var body []byte
			for seq := 1; seq <= 8; seq++ {
				code, body = r.frame(t, s, seq, still)
				if code != http.StatusOK {
					break
				}
			}
			return code, body // static replay
		},
	} {
		code, body := attack(t)
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("an attack answered %d rather than %d", code, http.StatusUnprocessableEntity)
		}

		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("refusal is not valid JSON: %v", err)
		}
		messages[env.Error.Code+"|"+env.Error.Message] = true
	}

	if len(messages) != 1 {
		t.Errorf("two different attacks produced %d distinct refusals; the response distinguishes them: %v",
			len(messages), messages)
	}
}
