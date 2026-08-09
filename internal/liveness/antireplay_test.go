package liveness

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

func testGuard() Guard {
	return Guard{
		MinLivenessScore:   0.80,
		EnforceAntiSpoof:   true,
		IdentityCosineMin:  0.70,
		PHashMinDistance:   5,
		MaxDuplicateStreak: 8,
		MaxRecentHashes:    64,
	}
}

// guardedSession returns a session ready to accept frames.
func guardedSession(t *testing.T) *Session {
	t.Helper()

	s, err := NewSession(testStart, NewSessionParams{
		ID: "s", ChallengeCount: 3, TTL: time.Minute, ChallengeTimeout: 20 * time.Second,
		Entropy: fixedEntropy(11),
	})
	if err != nil {
		t.Fatalf("NewSession() returned an unexpected error: %v", err)
	}
	if err := s.Begin(testStart); err != nil {
		t.Fatalf("Begin() returned an unexpected error: %v", err)
	}
	return s
}

// liveFace is a frame that passes every check on its own.
func liveFace() biometric.Face {
	return biometric.Face{EAR: 0.35, LivenessScore: 0.95}
}

// embeddingAt builds a unit vector at a chosen angle from a reference, so a
// test can set the cosine similarity exactly.
func embeddingAt(cosine float64) biometric.Embedding {
	e := make(biometric.Embedding, biometric.EmbeddingDim)
	e[0] = float32(cosine)
	e[1] = float32(math.Sqrt(1 - cosine*cosine))
	return e
}

func referenceEmbedding() biometric.Embedding { return embeddingAt(1) }

func TestGuardValidation(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*Guard)
		wantHint string
	}{
		{"valid", func(*Guard) {}, ""},
		{"liveness out of range", func(g *Guard) { g.MinLivenessScore = 2 }, "MinLivenessScore"},
		{"cosine out of range", func(g *Guard) { g.IdentityCosineMin = 3 }, "IdentityCosineMin"},
		{"hash distance too large", func(g *Guard) { g.PHashMinDistance = 100 }, "PHashMinDistance"},
		{"zero duplicate streak", func(g *Guard) { g.MaxDuplicateStreak = 0 }, "MaxDuplicateStreak"},
		{"zero history", func(g *Guard) { g.MaxRecentHashes = 0 }, "MaxRecentHashes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := testGuard()
			tt.mutate(&g)

			err := g.Validate()
			if tt.wantHint == "" {
				if err != nil {
					t.Fatalf("Validate() rejected a valid guard: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() accepted %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.wantHint) {
				t.Errorf("error does not mention %s: %v", tt.wantHint, err)
			}
		})
	}
}

func TestAGoodFramePasses(t *testing.T) {
	g := testGuard()
	s := guardedSession(t)

	if err := g.Check(s, Frame{Seq: 1, PHash: 0xAAAA}, liveFace()); err != nil {
		t.Fatalf("Check() rejected a good frame: %v", err)
	}
}

// The nonce is checked by the service before any of this runs, as
// authorisation rather than as a replay defence — see
// TestAWrongNonceIsRefusedWithoutTouchingTheSession.

// --- defence 2: sequence numbers must strictly increase ---

func TestSequenceMustIncrease(t *testing.T) {
	g := testGuard()

	tests := []struct {
		name    string
		lastSeq int64
		seq     int64
		wantErr bool
	}{
		{"next in order", 4, 5, false},
		{"skipping ahead is allowed", 4, 40, false},
		{"repeating the last one", 4, 4, true},
		{"going backwards", 4, 1, true},
		{"zero after a real frame", 4, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := guardedSession(t)
			s.LastSeq = tt.lastSeq

			err := g.Check(s, Frame{Seq: tt.seq, PHash: 0x1234}, liveFace())
			if tt.wantErr {
				if !errors.Is(err, ErrSequenceReplay) {
					t.Errorf("Check() error = %v, want ErrSequenceReplay", err)
				}
				return
			}
			if err != nil {
				t.Errorf("Check() rejected an in-order frame: %v", err)
			}
		})
	}
}

// --- defence 4: identical frames ---

// A subject holding still produces near-identical frames, so one duplicate
// cannot be fatal. Failing them would reject honest people for sitting steady.
func TestASingleDuplicateIsRecoverable(t *testing.T) {
	g := testGuard()
	s := guardedSession(t)

	first := Frame{Seq: 1, PHash: 0x0F0F0F0F0F0F0F0F}
	if err := g.Check(s, first, liveFace()); err != nil {
		t.Fatalf("Check() rejected the first frame: %v", err)
	}
	g.Record(s, first, liveFace())

	// One bit different: well inside the duplicate threshold.
	repeat := Frame{Seq: 2, PHash: first.PHash ^ 1}

	err := g.Check(s, repeat, liveFace())
	if !errors.Is(err, ErrDuplicateFrame) {
		t.Fatalf("Check() error = %v, want ErrDuplicateFrame", err)
	}
	if Fatal(err) {
		t.Error("a single duplicate frame is fatal; a subject holding still would fail")
	}
}

// What separates a person from a photograph is that a person eventually moves.
func TestALongRunOfIdenticalFramesFailsTheSession(t *testing.T) {
	g := testGuard()
	s := guardedSession(t)

	still := uint64(0x0F0F0F0F0F0F0F0F)
	var lastErr error

	for seq := int64(1); seq <= int64(g.MaxDuplicateStreak)+2; seq++ {
		f := Frame{Seq: seq, PHash: still}

		lastErr = g.Check(s, f, liveFace())
		if errors.Is(lastErr, ErrStaticReplay) {
			break
		}
		g.Record(s, f, liveFace())
	}

	if !errors.Is(lastErr, ErrStaticReplay) {
		t.Fatalf("a still image survived %d frames; last error = %v", g.MaxDuplicateStreak+2, lastErr)
	}
	if !Fatal(lastErr) {
		t.Error("a static replay is not fatal; it should be")
	}
}

// A moving subject must never accumulate a duplicate streak.
func TestMovementClearsTheDuplicateStreak(t *testing.T) {
	g := testGuard()
	s := guardedSession(t)

	still := uint64(0x0F0F0F0F0F0F0F0F)
	for seq := int64(1); seq <= 3; seq++ {
		f := Frame{Seq: seq, PHash: still}
		_ = g.Check(s, f, liveFace())
		g.Record(s, f, liveFace())
	}
	if s.DuplicateStreak == 0 {
		t.Fatal("three identical frames did not build a streak")
	}

	moved := Frame{Seq: 10, PHash: ^still}
	if err := g.Check(s, moved, liveFace()); err != nil {
		t.Fatalf("Check() rejected a clearly different frame: %v", err)
	}
	g.Record(s, moved, liveFace())

	if s.DuplicateStreak != 0 {
		t.Errorf("duplicate streak = %d after the subject moved, want 0", s.DuplicateStreak)
	}
}

// --- defence 5: the passive anti-spoof score ---

func TestLowLivenessScoreFailsTheSession(t *testing.T) {
	g := testGuard()

	tests := []struct {
		name    string
		score   float64
		wantErr bool
	}{
		{"clearly live", 0.99, false},
		{"exactly at the threshold", 0.80, false},
		{"just below", 0.79, true},
		{"a printed photograph", 0.01, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := guardedSession(t)

			face := liveFace()
			face.LivenessScore = tt.score

			err := g.Check(s, Frame{Seq: 1, PHash: 0x99}, face)
			if tt.wantErr {
				if !errors.Is(err, ErrSpoofDetected) {
					t.Errorf("Check() error = %v, want ErrSpoofDetected", err)
				}
				if !Fatal(err) {
					t.Error("a spoof is not fatal; it should be")
				}
				return
			}
			if err != nil {
				t.Errorf("Check() rejected a live frame: %v", err)
			}
		})
	}
}

// The score must not travel back to the client, who could use it to tune an
// attack until it just clears the threshold.
func TestSpoofErrorDoesNotLeakTheScore(t *testing.T) {
	g := testGuard()
	s := guardedSession(t)

	face := liveFace()
	face.LivenessScore = 0.4213

	err := g.Check(s, Frame{Seq: 1, PHash: 1}, face)
	if err == nil {
		t.Fatal("Check() accepted a spoof")
	}
	if strings.Contains(err.Error(), "0.42") {
		t.Errorf("the error carries the score: %v", err)
	}
}

// --- defence 6: the face must not change mid-session ---

// Without this an attacker satisfies the challenges with their own face and
// then presents the victim's for the frame that matters.
func TestIdentityMustHoldAcrossKeyFrames(t *testing.T) {
	g := testGuard()

	tests := []struct {
		name    string
		cosine  float64
		wantErr bool
	}{
		{"the same person", 1.0, false},
		{"the same person, different lighting", 0.85, false},

		// Not "exactly at the threshold": embeddings are float32, so a cosine
		// cannot land on a float64 threshold precisely. An assertion at the
		// boundary would be testing rounding, not behaviour.
		{"just above the threshold", 0.705, false},
		{"just below", 0.69, true},
		{"somebody else", 0.10, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := guardedSession(t)
			s.ReferenceEmbedding = referenceEmbedding()

			face := liveFace()
			face.Embedding = embeddingAt(tt.cosine)

			err := g.Check(s, Frame{Seq: 1, PHash: 0x77}, face)
			if tt.wantErr {
				if !errors.Is(err, ErrIdentityChanged) {
					t.Errorf("Check() error = %v, want ErrIdentityChanged", err)
				}
				if !Fatal(err) {
					t.Error("an identity change is not fatal; it should be")
				}
				return
			}
			if err != nil {
				t.Errorf("Check() rejected the same person: %v", err)
			}
		})
	}
}

// Most frames carry no embedding, and those must skip the check rather than
// being treated as a mismatch against an absent reference.
func TestFramesWithoutAnEmbeddingSkipTheIdentityCheck(t *testing.T) {
	g := testGuard()
	s := guardedSession(t)
	s.ReferenceEmbedding = referenceEmbedding()

	face := liveFace() // no embedding
	if err := g.Check(s, Frame{Seq: 1, PHash: 0x55}, face); err != nil {
		t.Errorf("Check() rejected an ordinary frame: %v", err)
	}
}

func TestTheFirstKeyFrameBecomesTheReference(t *testing.T) {
	g := testGuard()
	s := guardedSession(t)

	face := liveFace()
	face.Embedding = embeddingAt(1)

	f := Frame{Seq: 1, PHash: 0x11}
	if err := g.Check(s, f, face); err != nil {
		t.Fatalf("Check() rejected the first key frame: %v", err)
	}
	g.Record(s, f, face)

	if len(s.ReferenceEmbedding) == 0 {
		t.Fatal("the first key frame did not become the reference")
	}

	// A later key frame must not replace it, or an attacker could drift the
	// identity a little at a time until it is someone else entirely.
	second := liveFace()
	second.Embedding = embeddingAt(0.9)

	f2 := Frame{Seq: 2, PHash: 0x22}
	g.Record(s, f2, second)

	if c := s.ReferenceEmbedding.Cosine(embeddingAt(1)); math.Abs(c-1) > 1e-6 {
		t.Error("the reference embedding was replaced by a later key frame")
	}
}

// --- bookkeeping ---

func TestRecordBoundsTheHashHistory(t *testing.T) {
	g := testGuard()
	g.MaxRecentHashes = 4
	s := guardedSession(t)

	for seq := int64(1); seq <= 20; seq++ {
		// Hashes far apart so none counts as a duplicate.
		f := Frame{Seq: seq, PHash: uint64(seq) * 0x1111111111111111}
		g.Record(s, f, liveFace())
	}

	if len(s.RecentHashes) > g.MaxRecentHashes {
		t.Errorf("kept %d hashes, want at most %d", len(s.RecentHashes), g.MaxRecentHashes)
	}
	if s.LastSeq != 20 {
		t.Errorf("last sequence = %d, want 20", s.LastSeq)
	}
}

func TestFatalClassification(t *testing.T) {
	tests := []struct {
		err   error
		fatal bool
	}{
		{nil, false},
		{ErrDuplicateFrame, false},
		{ErrSequenceReplay, true},
		{ErrStaticReplay, true},
		{ErrSpoofDetected, true},
		{ErrIdentityChanged, true},
	}

	for _, tt := range tests {
		if got := Fatal(tt.err); got != tt.fatal {
			t.Errorf("Fatal(%v) = %v, want %v", tt.err, got, tt.fatal)
		}
	}

	// Wrapped errors must classify the same way.
	if Fatal(errors.Join(ErrDuplicateFrame)) {
		t.Error("a wrapped duplicate is fatal")
	}
}
