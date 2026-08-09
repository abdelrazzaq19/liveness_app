package enrollment

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

type memoryFaces struct {
	mu     sync.Mutex
	stored []Face
	insErr error
}

func (m *memoryFaces) Insert(_ context.Context, f Face) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.insErr != nil {
		return m.insErr
	}
	if err := f.Validate(); err != nil {
		return err
	}
	m.stored = append(m.stored, f)
	return nil
}

func (m *memoryFaces) Search(_ context.Context, q biometric.Embedding, topK int) ([]Match, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []Match
	for _, f := range m.stored {
		out = append(out, Match{FaceID: f.ID, SubjectID: f.SubjectID, Score: f.Embedding.Cosine(q)})
	}
	// Descending, as the real repository promises.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Score > out[j-1].Score; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

func (m *memoryFaces) DeleteSubject(_ context.Context, subject string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var kept []Face
	var n int
	for _, f := range m.stored {
		if f.SubjectID == subject {
			n++
			continue
		}
		kept = append(kept, f)
	}
	m.stored = kept
	return n, nil
}

func (m *memoryFaces) Count(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.stored), nil
}

type memoryTokens struct {
	mu    sync.Mutex
	saved map[string]TokenRecord
	used  map[string]bool
}

func newMemoryTokens() *memoryTokens {
	return &memoryTokens{saved: map[string]TokenRecord{}, used: map[string]bool{}}
}

func (m *memoryTokens) Save(_ context.Context, rec TokenRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saved[string(rec.Hash)] = rec
	return nil
}

func (m *memoryTokens) Consume(_ context.Context, hash []byte, now time.Time) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := string(hash)
	rec, ok := m.saved[key]
	if !ok || m.used[key] || !rec.ExpiresAt.After(now) {
		return "", ErrTokenInvalid
	}
	m.used[key] = true
	return rec.SessionID, nil
}

func (m *memoryTokens) PurgeExpired(context.Context, time.Time) (int, error) { return 0, nil }

type fixedSessions struct {
	embeddings map[string]biometric.Embedding
	err        error
}

func (f fixedSessions) ReferenceEmbedding(_ context.Context, id string) (biometric.Embedding, error) {
	if f.err != nil {
		return nil, f.err
	}
	e, ok := f.embeddings[id]
	if !ok {
		return nil, fmt.Errorf("no session %s", id)
	}
	return e, nil
}

type scriptedAnalyzer struct {
	face biometric.Face
	err  error
}

func (a scriptedAnalyzer) Analyze(context.Context, image.Image, biometric.AnalyzeOptions) (biometric.Face, error) {
	return a.face, a.err
}

type countingIDs struct {
	mu sync.Mutex
	n  int
}

func (g *countingIDs) NewID() (FaceID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return FaceID(fmt.Sprintf("face-%d", g.n)), nil
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

type recordingArtifacts struct {
	mu   sync.Mutex
	put  map[string][]byte
	fail error
}

func (r *recordingArtifacts) Put(_ context.Context, key string, data []byte, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	if r.put == nil {
		r.put = map[string][]byte{}
	}
	r.put[key] = data
	return nil
}

func (r *recordingArtifacts) Get(context.Context, string) ([]byte, error) {
	return nil, ErrArtifactNotFound
}
func (r *recordingArtifacts) Delete(context.Context, string) error { return nil }

// ------------------------------------------------------------------- fixtures

var testStart = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

// embeddingSeeded builds a deterministic normalised vector. Vectors from
// different seeds are near-orthogonal, which is what two different people look
// like to an embedder.
func embeddingSeeded(seed int) biometric.Embedding {
	e := make(biometric.Embedding, biometric.EmbeddingDim)
	x := uint64(seed)*2862933555777941757 + 3037000493
	for i := range e {
		x = x*6364136223846793005 + 1442695040888963407
		e[i] = float32(int64(x>>33)%2000-1000) / 1000
	}
	return e.Normalize()
}

// nearby returns a vector close to e: the same face captured again.
func nearby(e biometric.Embedding, drift float64) biometric.Embedding {
	other := embeddingSeeded(99)
	out := make(biometric.Embedding, len(e))
	for i := range e {
		out[i] = e[i] + float32(drift)*other[i]
	}
	return out.Normalize()
}

type harness struct {
	svc       *Service
	faces     *memoryFaces
	tokens    *TokenService
	store     *memoryTokens
	artifacts *recordingArtifacts
	sessions  fixedSessions
}

func newHarness(t *testing.T, tweak func(*Deps)) *harness {
	t.Helper()

	faces := &memoryFaces{}
	store := newMemoryTokens()
	artifacts := &recordingArtifacts{}

	tokens, err := NewTokenService(TokenDeps{Store: store, Secret: "test-secret", TTL: 5 * time.Minute})
	if err != nil {
		t.Fatalf("NewTokenService() returned an unexpected error: %v", err)
	}

	sessions := fixedSessions{embeddings: map[string]biometric.Embedding{
		"session-ok": embeddingSeeded(1),
	}}

	d := Deps{
		Faces:     faces,
		Tokens:    tokens,
		Sessions:  sessions,
		Analyzer:  scriptedAnalyzer{face: biometric.Face{Embedding: nearby(embeddingSeeded(1), 0.15)}},
		Artifacts: artifacts,
		EncodeArtifact: func(image.Image, biometric.Keypoints) ([]byte, string, error) {
			return []byte("aligned-crop"), "image/jpeg", nil
		},
		IDs:    &countingIDs{},
		Clock:  fixedClock{now: testStart},
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Config: Config{
			MatchCosineMin:    0.42,
			IdentityCosineMin: 0.70,
			SearchTopK:        5,
		},
	}
	if tweak != nil {
		tweak(&d)
	}

	svc, err := NewService(d)
	if err != nil {
		t.Fatalf("NewService() returned an unexpected error: %v", err)
	}
	return &harness{svc: svc, faces: faces, tokens: tokens, store: store, artifacts: artifacts, sessions: sessions}
}

func blankImage() image.Image { return image.NewRGBA(image.Rect(0, 0, 64, 64)) }

func (h *harness) token(t *testing.T, sessionID string) string {
	t.Helper()
	tok, err := h.tokens.Issue(context.Background(), sessionID, testStart)
	if err != nil {
		t.Fatalf("Issue() returned an unexpected error: %v", err)
	}
	return tok
}

// ------------------------------------------------------------------- the tests

func TestNewServiceRequiresItsDependencies(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(*Deps)
	}{
		{"no repository", func(d *Deps) { d.Faces = nil }},
		{"no tokens", func(d *Deps) { d.Tokens = nil }},
		{"no sessions", func(d *Deps) { d.Sessions = nil }},
		{"no analyzer", func(d *Deps) { d.Analyzer = nil }},
		{"no ids", func(d *Deps) { d.IDs = nil }},
		{"no clock", func(d *Deps) { d.Clock = nil }},
		{"no logger", func(d *Deps) { d.Logger = nil }},
		{"bad config", func(d *Deps) { d.Config.SearchTopK = 0 }},
		{"artifacts on, no store", func(d *Deps) { d.Config.StoreArtifacts = true; d.Artifacts = nil }},
		{"artifacts on, no encoder", func(d *Deps) { d.Config.StoreArtifacts = true; d.EncodeArtifact = nil }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := newHarness(t, nil)
			_ = base

			faces := &memoryFaces{}
			store := newMemoryTokens()
			tokens, _ := NewTokenService(TokenDeps{Store: store, Secret: "s", TTL: time.Minute})

			d := Deps{
				Faces:    faces,
				Tokens:   tokens,
				Sessions: fixedSessions{},
				Analyzer: scriptedAnalyzer{},
				IDs:      &countingIDs{},
				Clock:    fixedClock{now: testStart},
				Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
				Config:   Config{MatchCosineMin: 0.42, IdentityCosineMin: 0.7, SearchTopK: 5},
			}
			tt.break_(&d)

			if _, err := NewService(d); err == nil {
				t.Error("NewService() accepted incomplete dependencies, want an error")
			}
		})
	}
}

func TestEnrollStoresTheFace(t *testing.T) {
	h := newHarness(t, nil)

	got, err := h.svc.Enroll(context.Background(), EnrollInput{
		Token:     h.token(t, "session-ok"),
		SubjectID: "subject-1",
		Image:     blankImage(),
	})
	if err != nil {
		t.Fatalf("Enroll() returned an unexpected error: %v", err)
	}
	if got.SubjectID != "subject-1" || got.SessionID != "session-ok" {
		t.Errorf("result = %+v, want subject-1 from session-ok", got)
	}
	if len(h.faces.stored) != 1 {
		t.Fatalf("stored %d faces, want 1", len(h.faces.stored))
	}
	if h.faces.stored[0].SessionID != "session-ok" {
		t.Error("the stored face does not record the session that authorised it")
	}
}

// The attack this whole path exists to stop: pass liveness with your own face,
// then enrol somebody else's photograph.
//
// A token proves that some session passed. On its own it says nothing about
// whose face arrives with it.
func TestADifferentFaceCannotRideAValidToken(t *testing.T) {
	h := newHarness(t, func(d *Deps) {
		// Verified session is person 1; the image submitted is person 2.
		d.Analyzer = scriptedAnalyzer{face: biometric.Face{Embedding: embeddingSeeded(2)}}
	})

	_, err := h.svc.Enroll(context.Background(), EnrollInput{
		Token:     h.token(t, "session-ok"),
		SubjectID: "victim",
		Image:     blankImage(),
	})
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("error = %v, want ErrIdentityMismatch", err)
	}
	if len(h.faces.stored) != 0 {
		t.Error("a face that failed the identity check reached the gallery")
	}
}

// Nothing is enrolled without a token, whatever else the request carries.
func TestEnrollNeedsAValidToken(t *testing.T) {
	tests := []struct {
		name  string
		token func(h *harness, t *testing.T) string
	}{
		{"no token", func(*harness, *testing.T) string { return "" }},
		{"unknown token", func(*harness, *testing.T) string { return "not-a-real-token" }},
		{"already spent", func(h *harness, t *testing.T) string {
			tok := h.token(t, "session-ok")
			if _, err := h.svc.Enroll(context.Background(), EnrollInput{
				Token: tok, SubjectID: "first", Image: blankImage(),
			}); err != nil {
				t.Fatalf("the first enrollment failed: %v", err)
			}
			return tok
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			before := len(h.faces.stored)

			_, err := h.svc.Enroll(context.Background(), EnrollInput{
				Token: tt.token(h, t), SubjectID: "subject-x", Image: blankImage(),
			})
			if !errors.Is(err, ErrTokenInvalid) {
				t.Errorf("error = %v, want ErrTokenInvalid", err)
			}
			if len(h.faces.stored) > before+1 {
				t.Error("a face was enrolled without a valid token")
			}
		})
	}
}

// A token is spent even when the enrollment then fails. Otherwise a token
// survives every failed attempt and can be replayed until something gets
// through — unlimited attempts for an attacker against one wasted verification
// for an honest subject.
func TestAFailedEnrollmentStillSpendsTheToken(t *testing.T) {
	h := newHarness(t, func(d *Deps) {
		d.Analyzer = scriptedAnalyzer{face: biometric.Face{Embedding: embeddingSeeded(2)}}
	})

	tok := h.token(t, "session-ok")

	if _, err := h.svc.Enroll(context.Background(), EnrollInput{
		Token: tok, SubjectID: "s", Image: blankImage(),
	}); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("first attempt error = %v, want ErrIdentityMismatch", err)
	}

	// Second attempt with the same token, now with the right face.
	h2 := h
	if _, err := h2.svc.Enroll(context.Background(), EnrollInput{
		Token: tok, SubjectID: "s", Image: blankImage(),
	}); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("the token survived a failed enrollment: %v", err)
	}
}

func TestEnrollRefusesWhenTheVerifiedCaptureIsGone(t *testing.T) {
	h := newHarness(t, func(d *Deps) {
		d.Sessions = fixedSessions{err: errors.New("session purged")}
	})

	_, err := h.svc.Enroll(context.Background(), EnrollInput{
		Token: h.token(t, "session-ok"), SubjectID: "s", Image: blankImage(),
	})
	if !errors.Is(err, ErrSessionUnavailable) {
		t.Errorf("error = %v, want ErrSessionUnavailable", err)
	}
	if len(h.faces.stored) != 0 {
		t.Error("a face was enrolled with no verified capture to bind it to")
	}
}

// The artifact is written before the row, so a crash between the two leaves an
// orphaned object rather than a row pointing at an image that is not there.
func TestArtifactFailureStopsTheEnrollment(t *testing.T) {
	h := newHarness(t, func(d *Deps) {
		d.Config.StoreArtifacts = true
		d.Artifacts = &recordingArtifacts{fail: errors.New("bucket is full")}
	})

	if _, err := h.svc.Enroll(context.Background(), EnrollInput{
		Token: h.token(t, "session-ok"), SubjectID: "s", Image: blankImage(),
	}); err == nil {
		t.Fatal("Enroll() succeeded despite the artifact write failing")
	}
	if len(h.faces.stored) != 0 {
		t.Error("a row was written pointing at an artifact that does not exist")
	}
}

func TestArtifactsAreNotStoredByDefault(t *testing.T) {
	h := newHarness(t, nil)

	if _, err := h.svc.Enroll(context.Background(), EnrollInput{
		Token: h.token(t, "session-ok"), SubjectID: "s", Image: blankImage(),
	}); err != nil {
		t.Fatalf("Enroll() returned an unexpected error: %v", err)
	}

	if len(h.artifacts.put) != 0 {
		t.Errorf("an image was retained without being asked for: %v", h.artifacts.put)
	}
	if h.faces.stored[0].ArtifactKey != "" {
		t.Error("the stored row claims an artifact that was never written")
	}
}

func TestSearchAppliesTheThreshold(t *testing.T) {
	me := embeddingSeeded(1)

	tests := []struct {
		name        string
		query       biometric.Embedding
		wantMatched bool
	}{
		{"the same person", nearby(me, 0.15), true},
		{"somebody else", embeddingSeeded(500), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			if _, err := h.svc.Enroll(context.Background(), EnrollInput{
				Token: h.token(t, "session-ok"), SubjectID: "subject-1", Image: blankImage(),
			}); err != nil {
				t.Fatalf("Enroll() returned an unexpected error: %v", err)
			}

			h2 := newHarness(t, func(d *Deps) {
				d.Faces = h.faces
				d.Analyzer = scriptedAnalyzer{face: biometric.Face{Embedding: tt.query}}
			})

			got, err := h2.svc.Search(context.Background(), blankImage())
			if err != nil {
				t.Fatalf("Search() returned an unexpected error: %v", err)
			}
			if got.Matched != tt.wantMatched {
				t.Errorf("matched = %v, want %v (best score %.4f)", got.Matched, tt.wantMatched, got.Candidates[0].Score)
			}
		})
	}
}

// An empty gallery and a gallery where nobody matched are different problems. A
// caller that cannot tell them apart diagnoses an empty database as a
// recognition failure.
func TestAnEmptyGalleryIsNotAFailedMatch(t *testing.T) {
	h := newHarness(t, nil)

	_, err := h.svc.Search(context.Background(), blankImage())
	if !errors.Is(err, ErrNoMatch) {
		t.Errorf("error = %v, want ErrNoMatch for an empty gallery", err)
	}
}

func TestDeleteSubjectReportsWhatItRemoved(t *testing.T) {
	h := newHarness(t, nil)

	for i := 0; i < 2; i++ {
		if _, err := h.svc.Enroll(context.Background(), EnrollInput{
			Token: h.token(t, "session-ok"), SubjectID: "subject-gone", Image: blankImage(),
		}); err != nil {
			t.Fatalf("Enroll() returned an unexpected error: %v", err)
		}
	}

	n, err := h.svc.DeleteSubject(context.Background(), "subject-gone")
	if err != nil {
		t.Fatalf("DeleteSubject() returned an unexpected error: %v", err)
	}
	if n != 2 {
		t.Errorf("removed %d faces, want 2", n)
	}

	if _, err := h.svc.DeleteSubject(context.Background(), "subject-gone"); !errors.Is(err, ErrSubjectNotFound) {
		t.Errorf("deleting an unknown subject returned %v, want ErrSubjectNotFound", err)
	}
}
