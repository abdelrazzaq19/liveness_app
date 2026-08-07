package liveness

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

var testStart = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

// fixedEntropy returns a reader that yields a deterministic byte stream, so a
// test can assert on the exact challenge order the generator produces.
func fixedEntropy(seed byte) *bytes.Reader {
	b := make([]byte, 4096)
	for i := range b {
		b[i] = seed + byte(i*31)
	}
	return bytes.NewReader(b)
}

func newTestSession(t *testing.T, count int) *Session {
	t.Helper()

	s, err := NewSession(testStart, NewSessionParams{
		ID:               "sess-1",
		ChallengeCount:   count,
		TTL:              90 * time.Second,
		ChallengeTimeout: 20 * time.Second,
		Entropy:          fixedEntropy(7),
	})
	if err != nil {
		t.Fatalf("NewSession() returned an unexpected error: %v", err)
	}
	return s
}

func TestNewSessionStartsPending(t *testing.T) {
	s := newTestSession(t, 3)

	if s.State != StatePending {
		t.Errorf("state = %s, want %s", s.State, StatePending)
	}
	if len(s.Challenges) != 3 {
		t.Errorf("drew %d challenges, want 3", len(s.Challenges))
	}
	if s.Nonce == "" {
		t.Error("session has no nonce")
	}
	if !s.ExpiresAt.After(s.CreatedAt) {
		t.Errorf("expires at %s, created at %s", s.ExpiresAt, s.CreatedAt)
	}
	if got := s.ActiveChallenge(); got != s.Challenges[0] {
		t.Errorf("active challenge = %s, want the first drawn %s", got, s.Challenges[0])
	}
	if got := s.Remaining(); got != 3 {
		t.Errorf("Remaining() = %d, want 3", got)
	}
}

func TestNewSessionRejectsBadParameters(t *testing.T) {
	tests := []struct {
		name   string
		params NewSessionParams
	}{
		{"no id", NewSessionParams{ChallengeCount: 3, TTL: time.Minute, ChallengeTimeout: time.Second}},
		{"zero TTL", NewSessionParams{ID: "s", ChallengeCount: 3, ChallengeTimeout: time.Second}},
		{"zero challenge timeout", NewSessionParams{ID: "s", ChallengeCount: 3, TTL: time.Minute}},
		{"no challenges", NewSessionParams{ID: "s", TTL: time.Minute, ChallengeTimeout: time.Second}},
		{"more challenges than exist", NewSessionParams{
			ID: "s", ChallengeCount: len(AllChallenges) + 1, TTL: time.Minute, ChallengeTimeout: time.Second,
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.params.Entropy = fixedEntropy(1)
			if _, err := NewSession(testStart, tt.params); err == nil {
				t.Error("NewSession() accepted invalid parameters, want an error")
			}
		})
	}
}

// Every edge the lifecycle allows, and a representative set it does not. A
// state machine that is not exhaustively tested is a state machine with an
// unreachable branch nobody has noticed.
func TestStateTransitions(t *testing.T) {
	legal := []struct {
		from, to State
	}{
		{StatePending, StateInProgress},
		{StatePending, StateFailed},
		{StatePending, StateExpired},
		{StateInProgress, StatePassed},
		{StateInProgress, StateFailed},
		{StateInProgress, StateExpired},
	}

	for _, tt := range legal {
		t.Run("legal "+string(tt.from)+" to "+string(tt.to), func(t *testing.T) {
			s := newTestSession(t, 3)
			s.State = tt.from

			if err := s.transition(tt.to, testStart); err != nil {
				t.Fatalf("transition() refused a legal edge: %v", err)
			}
			if s.State != tt.to {
				t.Errorf("state = %s, want %s", s.State, tt.to)
			}
		})
	}

	illegal := []struct {
		from, to State
	}{
		{StatePending, StatePassed},     // cannot pass without attempting
		{StatePassed, StateFailed},      // terminal
		{StateFailed, StateInProgress},  // terminal
		{StateExpired, StatePassed},     // terminal
		{StateInProgress, StatePending}, // no going back
		{StatePassed, StatePassed},      // terminal, even to itself
	}

	for _, tt := range illegal {
		t.Run("illegal "+string(tt.from)+" to "+string(tt.to), func(t *testing.T) {
			s := newTestSession(t, 3)
			s.State = tt.from

			err := s.transition(tt.to, testStart)
			if !errors.Is(err, ErrIllegalTransition) {
				t.Fatalf("transition() error = %v, want ErrIllegalTransition", err)
			}
			// An illegal edge must not be applied.
			if s.State != tt.from {
				t.Errorf("state changed to %s despite the refusal", s.State)
			}
		})
	}
}

// An unknown state is a corrupted row, not a bug in the caller. Refusing is
// what keeps it from becoming a session in a state nothing understands.
func TestTransitionRefusesAnUnknownState(t *testing.T) {
	s := newTestSession(t, 3)
	s.State = State("SOMETHING_ELSE")

	if err := s.transition(StateFailed, testStart); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("transition() error = %v, want ErrIllegalTransition", err)
	}
}

func TestAdvanceWalksThroughEveryChallenge(t *testing.T) {
	s := newTestSession(t, 3)
	if err := s.Begin(testStart); err != nil {
		t.Fatalf("Begin() returned an unexpected error: %v", err)
	}

	now := testStart
	for i := 0; i < 3; i++ {
		if got, want := s.ActiveChallenge(), s.Challenges[i]; got != want {
			t.Fatalf("challenge %d is %s, want %s", i, got, want)
		}
		if got, want := s.Remaining(), 3-i; got != want {
			t.Errorf("Remaining() = %d, want %d", got, want)
		}

		now = now.Add(2 * time.Second)
		if err := s.Advance(now, 20*time.Second); err != nil {
			t.Fatalf("Advance() returned an unexpected error: %v", err)
		}
	}

	if !s.AllComplete() {
		t.Error("AllComplete() is false after advancing past every challenge")
	}
	if s.State != StatePassed {
		t.Errorf("state = %s, want %s once every challenge is done", s.State, StatePassed)
	}
	if got := s.ActiveChallenge(); got != "" {
		t.Errorf("ActiveChallenge() = %s, want empty once complete", got)
	}
}

// Progress is per challenge. Carrying a half-finished blink into the next one
// would let a single motion satisfy two challenges.
func TestAdvanceResetsProgress(t *testing.T) {
	s := newTestSession(t, 3)
	_ = s.Begin(testStart)

	s.Progress = Progress{ClosedFrames: 4, SawClose: true, HaveBaseline: true, BaselineYaw: 12}

	if err := s.Advance(testStart, 20*time.Second); err != nil {
		t.Fatalf("Advance() returned an unexpected error: %v", err)
	}
	if s.Progress != (Progress{}) {
		t.Errorf("progress = %+v, want it reset", s.Progress)
	}
}

func TestAdvanceGivesEachChallengeAFreshDeadline(t *testing.T) {
	s := newTestSession(t, 3)
	_ = s.Begin(testStart)

	at := testStart.Add(15 * time.Second)
	if err := s.Advance(at, 20*time.Second); err != nil {
		t.Fatalf("Advance() returned an unexpected error: %v", err)
	}

	if want := at.Add(20 * time.Second); !s.ChallengeDeadline.Equal(want) {
		t.Errorf("challenge deadline = %s, want %s", s.ChallengeDeadline, want)
	}
}

func TestExpiry(t *testing.T) {
	tests := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"fresh", testStart, false},
		{"within the challenge window", testStart.Add(19 * time.Second), false},
		{"past the challenge deadline", testStart.Add(21 * time.Second), true},
		{"past the session TTL", testStart.Add(91 * time.Second), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestSession(t, 3)
			_ = s.Begin(testStart)

			if got := s.Expired(tt.at); got != tt.want {
				t.Errorf("Expired(%s) = %v, want %v", tt.at.Sub(testStart), got, tt.want)
			}
		})
	}
}

// The countdown the subject watches. It has to be the earlier of the two
// deadlines: showing a challenge's ten seconds while the session had three left
// would be a lie they only discover at zero.
func TestSecondsRemaining(t *testing.T) {
	tests := []struct {
		name string
		at   time.Time
		want float64
	}{
		{"at the start", testStart, 20},
		{"part way through", testStart.Add(7 * time.Second), 13},
		{"exactly at the deadline", testStart.Add(20 * time.Second), 0},
		{"past it, never negative", testStart.Add(45 * time.Second), 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestSession(t, 3)
			_ = s.Begin(testStart)

			if got := s.SecondsRemaining(tt.at); got != tt.want {
				t.Errorf("SecondsRemaining() = %g, want %g", got, tt.want)
			}
		})
	}
}

func TestSecondsRemainingUsesTheEarlierDeadline(t *testing.T) {
	s := newTestSession(t, 3)
	_ = s.Begin(testStart)

	// A session about to end while the challenge still has plenty of time.
	s.ExpiresAt = testStart.Add(4 * time.Second)

	if got := s.SecondsRemaining(testStart); got != 4 {
		t.Errorf("SecondsRemaining() = %g, want the session's 4 rather than the challenge's 20", got)
	}
}

// A finished session has no countdown, and a client that kept showing one would
// be inviting the subject to keep trying.
func TestSecondsRemainingIsZeroOnceTheSessionEnds(t *testing.T) {
	for _, state := range []State{StatePassed, StateFailed, StateExpired} {
		t.Run(string(state), func(t *testing.T) {
			s := newTestSession(t, 3)
			s.State = state

			if got := s.SecondsRemaining(testStart); got != 0 {
				t.Errorf("SecondsRemaining() = %g on a %s session, want 0", got, state)
			}
		})
	}
}

func TestAdvanceRestartsTheCountdown(t *testing.T) {
	s := newTestSession(t, 3)
	_ = s.Begin(testStart)

	at := testStart.Add(8 * time.Second)
	if got := s.SecondsRemaining(at); got != 12 {
		t.Fatalf("SecondsRemaining() = %g before advancing, want 12", got)
	}

	if err := s.Advance(at, 20*time.Second); err != nil {
		t.Fatalf("Advance() returned an unexpected error: %v", err)
	}
	if got := s.SecondsRemaining(at); got != 20 {
		t.Errorf("SecondsRemaining() = %g after advancing, want a full 20", got)
	}
}

// A finished session must not keep expiring: the challenge deadline is long
// past by the time anyone looks at a completed record.
func TestCompletedSessionDoesNotExpire(t *testing.T) {
	s := newTestSession(t, 1)
	_ = s.Begin(testStart)
	_ = s.Advance(testStart, 20*time.Second)

	if s.State != StatePassed {
		t.Fatalf("state = %s, want %s", s.State, StatePassed)
	}
	if s.Expired(testStart.Add(30 * time.Second)) {
		t.Error("a passed session reports itself expired past the challenge deadline")
	}
	// The session TTL still applies, so an old record can be recognised.
	if !s.Expired(testStart.Add(200 * time.Second)) {
		t.Error("a session past its TTL does not report as expired")
	}
}

func TestTerminalSessionsRefuseFurtherWork(t *testing.T) {
	for _, state := range []State{StatePassed, StateFailed, StateExpired} {
		t.Run(string(state), func(t *testing.T) {
			s := newTestSession(t, 3)
			s.State = state

			if err := s.Advance(testStart, time.Second); !errors.Is(err, ErrSessionFinished) {
				t.Errorf("Advance() error = %v, want ErrSessionFinished", err)
			}
			if err := s.Fail(testStart, "late"); !errors.Is(err, ErrSessionFinished) {
				t.Errorf("Fail() error = %v, want ErrSessionFinished", err)
			}
			if err := s.Expire(testStart); !errors.Is(err, ErrSessionFinished) {
				t.Errorf("Expire() error = %v, want ErrSessionFinished", err)
			}
		})
	}
}

func TestFailRecordsTheReason(t *testing.T) {
	s := newTestSession(t, 3)
	_ = s.Begin(testStart)

	if err := s.Fail(testStart, "frame flagged as spoof"); err != nil {
		t.Fatalf("Fail() returned an unexpected error: %v", err)
	}
	if s.State != StateFailed {
		t.Errorf("state = %s, want %s", s.State, StateFailed)
	}
	if s.FailureReason != "frame flagged as spoof" {
		t.Errorf("reason = %q, want the one given", s.FailureReason)
	}
}

// Frames from one session can arrive concurrently. Without a version the later
// write silently discards the earlier one's progress.
func TestEveryTransitionBumpsTheVersion(t *testing.T) {
	s := newTestSession(t, 2)
	start := s.Version

	_ = s.Begin(testStart)
	if s.Version == start {
		t.Fatal("Begin() did not change the version")
	}

	before := s.Version
	_ = s.Advance(testStart, 20*time.Second) // not terminal, no transition
	_ = s.Advance(testStart, 20*time.Second) // completes, transitions to passed

	if s.Version <= before {
		t.Errorf("version = %d, want it above %d after passing", s.Version, before)
	}
}

func TestBeginIsIdempotent(t *testing.T) {
	s := newTestSession(t, 3)

	if err := s.Begin(testStart); err != nil {
		t.Fatalf("Begin() returned an unexpected error: %v", err)
	}
	if err := s.Begin(testStart); err != nil {
		t.Errorf("a second Begin() returned an error: %v", err)
	}
	if s.State != StateInProgress {
		t.Errorf("state = %s, want %s", s.State, StateInProgress)
	}
}
