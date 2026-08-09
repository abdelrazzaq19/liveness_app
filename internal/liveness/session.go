package liveness

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

// Sentinel errors callers are expected to branch on.
var (
	// ErrSessionExpired means the session ran out of time.
	ErrSessionExpired = errors.New("liveness: session expired")

	// ErrSessionFinished means the session already reached a terminal state.
	ErrSessionFinished = errors.New("liveness: session already finished")

	// ErrIllegalTransition means a state change was requested that the
	// lifecycle does not allow.
	ErrIllegalTransition = errors.New("liveness: illegal state transition")

	// ErrChallengesIncomplete means Complete was called before every challenge
	// was satisfied.
	ErrChallengesIncomplete = errors.New("liveness: not every challenge was completed")
)

// State is where a session sits in its lifecycle.
type State string

const (
	StatePending    State = "PENDING"
	StateInProgress State = "IN_PROGRESS"
	StatePassed     State = "PASSED"
	StateFailed     State = "FAILED"
	StateExpired    State = "EXPIRED"
)

// Terminal reports whether no further transition is possible.
func (s State) Terminal() bool {
	return s == StatePassed || s == StateFailed || s == StateExpired
}

// legalTransitions is the entire lifecycle, written out.
//
// A state machine that is a table can be read and tested; one scattered through
// if statements can only be traced. Every edge a session may take is here, and
// anything absent is refused rather than silently allowed.
var legalTransitions = map[State][]State{
	StatePending:    {StateInProgress, StateFailed, StateExpired},
	StateInProgress: {StatePassed, StateFailed, StateExpired},
	StatePassed:     {},
	StateFailed:     {},
	StateExpired:    {},
}

// SessionID identifies one verification attempt.
type SessionID string

func (id SessionID) String() string { return string(id) }

// Clock reports the current time.
//
// The domain never calls time.Now directly: expiry is most of what a session
// does, and a test that had to sleep to exercise it would be slow and flaky in
// equal measure.
type Clock interface {
	Now() time.Time
}

// SystemClock is the real clock.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time { return time.Now() }

// Progress is the per-challenge state carried between frames.
//
// It lives on the session because the challenges need memory: a blink is not
// visible in any single frame, only in a sequence of them.
type Progress struct {
	// ClosedFrames counts consecutive frames with the eyes below the blink
	// threshold.
	ClosedFrames int `json:"closed_frames"`

	// SawClose records that the eyes have been shut during this challenge, so
	// that reopening them completes the blink rather than starting one.
	SawClose bool `json:"saw_close"`

	// BaselineYaw and BaselinePitch are the head angles when the challenge
	// began, so a turn is measured as a movement rather than as an absolute
	// pose. A subject who is simply sitting at an angle should still have to
	// turn.
	BaselineYaw   float64 `json:"baseline_yaw"`
	BaselinePitch float64 `json:"baseline_pitch"`
	HaveBaseline  bool    `json:"have_baseline"`
}

// Session is one verification attempt.
type Session struct {
	ID    SessionID `json:"id"`
	Nonce string    `json:"nonce"`
	State State     `json:"state"`

	Challenges []ChallengeKind `json:"challenges"`

	// Current is the index of the challenge being attempted.
	Current int `json:"current"`

	CreatedAt         time.Time `json:"created_at"`
	ExpiresAt         time.Time `json:"expires_at"`
	ChallengeDeadline time.Time `json:"challenge_deadline"`

	// LastSeq is the highest frame sequence number accepted so far. Frames must
	// arrive strictly increasing.
	LastSeq int64 `json:"last_seq"`

	// RecentHashes holds the perceptual hashes of accepted frames, so a
	// repeated still image can be spotted.
	RecentHashes []uint64 `json:"recent_hashes"`

	// DuplicateStreak counts consecutive visually identical frames. One
	// duplicate means the subject held still; a long run means the camera is
	// looking at a photograph.
	DuplicateStreak int `json:"duplicate_streak"`

	// ReferenceEmbedding is the descriptor of the first key frame. Every later
	// key frame is compared against it, which is what stops the face being
	// swapped part-way through an otherwise valid session.
	ReferenceEmbedding biometric.Embedding `json:"-"`

	Progress Progress `json:"progress"`

	// Retries counts challenge attempts restarted after their deadline ran out.
	//
	// Counted for the whole session rather than per challenge, and bounded. A
	// retry hands an attacker another attempt at the same instruction, so the
	// budget is what keeps a challenge sequence from degrading into "keep
	// trying until something works". The session TTL is the second bound, and
	// neither can be spent past the other.
	Retries int `json:"retries"`

	// FailureReason is set when the session fails, for the audit trail. It is
	// never sent to the client, which learns only that verification failed.
	FailureReason string `json:"failure_reason,omitempty"`

	// Version is the optimistic lock. Frames from one session can arrive
	// concurrently, and without this the later write silently discards the
	// earlier one's progress.
	Version int `json:"version"`
}

// NewSessionParams configures a new session.
type NewSessionParams struct {
	ID               SessionID
	ChallengeCount   int
	TTL              time.Duration
	ChallengeTimeout time.Duration
	Entropy          io.Reader
}

// NewSession creates a session in the pending state.
func NewSession(now time.Time, p NewSessionParams) (*Session, error) {
	if p.ID == "" {
		return nil, errors.New("liveness: session needs an id")
	}
	if p.TTL <= 0 {
		return nil, fmt.Errorf("liveness: session TTL must be positive, got %s", p.TTL)
	}
	if p.ChallengeTimeout <= 0 {
		return nil, fmt.Errorf("liveness: challenge timeout must be positive, got %s", p.ChallengeTimeout)
	}

	challenges, err := NewChallengeSet(p.Entropy, p.ChallengeCount)
	if err != nil {
		return nil, err
	}
	nonce, err := NewNonce(p.Entropy)
	if err != nil {
		return nil, err
	}

	return &Session{
		ID:                p.ID,
		Nonce:             nonce,
		State:             StatePending,
		Challenges:        challenges,
		CreatedAt:         now,
		ExpiresAt:         now.Add(p.TTL),
		ChallengeDeadline: now.Add(p.ChallengeTimeout),
	}, nil
}

// ActiveChallenge returns the challenge the subject is being asked for.
//
// It returns the empty kind once every challenge is done, which is what
// Complete checks.
func (s *Session) ActiveChallenge() ChallengeKind {
	if s.Current < 0 || s.Current >= len(s.Challenges) {
		return ""
	}
	return s.Challenges[s.Current]
}

// Remaining is how many challenges are still to be satisfied.
func (s *Session) Remaining() int {
	if s.Current >= len(s.Challenges) {
		return 0
	}
	return len(s.Challenges) - s.Current
}

// AllComplete reports whether every challenge has been satisfied.
func (s *Session) AllComplete() bool { return s.Current >= len(s.Challenges) }

// SecondsRemaining is how long the subject has left on the active challenge.
//
// It is the earlier of the two deadlines, because either one ending stops the
// session: a countdown that showed the challenge's twelve seconds while the
// session had three left would be a lie the subject only discovers at zero.
//
// Never negative, and zero once the session is over.
func (s *Session) SecondsRemaining(now time.Time) float64 {
	if s.State.Terminal() || s.AllComplete() {
		return 0
	}

	deadline := s.ChallengeDeadline
	if s.ExpiresAt.Before(deadline) {
		deadline = s.ExpiresAt
	}

	left := deadline.Sub(now).Seconds()
	if left < 0 {
		return 0
	}
	return left
}

// SessionExpired reports whether the session's own lifetime has run out.
//
// This is the hard ceiling. Nothing recovers from it: it bounds the total time
// an attacker has to work with, so no amount of retry budget may extend it.
func (s *Session) SessionExpired(now time.Time) bool {
	return now.After(s.ExpiresAt)
}

// ChallengeExpired reports whether the active challenge ran out of time.
//
// Kept separate from SessionExpired because they mean different things: this
// one is recoverable while retries remain, and the other never is. Collapsing
// them, which is how this started, made every missed challenge fatal.
func (s *Session) ChallengeExpired(now time.Time) bool {
	// A challenge deadline only applies while one is being attempted.
	return !s.State.Terminal() && !s.AllComplete() && now.After(s.ChallengeDeadline)
}

// Expired reports whether the session or its current challenge has run out of
// time.
func (s *Session) Expired(now time.Time) bool {
	return s.SessionExpired(now) || s.ChallengeExpired(now)
}

// RetryChallenge restarts the active challenge on a fresh deadline.
//
// Progress is cleared, and that is the point rather than tidiness: carrying it
// over would let a subject shut their eyes in one attempt and open them in the
// next, satisfying a blink that never happened in either. The replay state —
// sequence number, frame hashes, reference embedding — is deliberately kept,
// because none of it belongs to the challenge.
func (s *Session) RetryChallenge(now time.Time, challengeTimeout time.Duration) error {
	if s.State.Terminal() {
		return fmt.Errorf("%w: state %s", ErrSessionFinished, s.State)
	}
	if s.AllComplete() {
		return nil
	}

	s.Retries++
	s.Progress = Progress{}
	s.ChallengeDeadline = now.Add(challengeTimeout)
	s.Version++
	return nil
}

// Begin moves a pending session into progress.
func (s *Session) Begin(now time.Time) error {
	if s.State == StateInProgress {
		return nil
	}
	return s.transition(StateInProgress, now)
}

// Advance records that the active challenge was satisfied.
//
// The session passes once the last one is done; otherwise the next challenge
// starts with a fresh deadline and fresh progress.
func (s *Session) Advance(now time.Time, challengeTimeout time.Duration) error {
	if s.State.Terminal() {
		return fmt.Errorf("%w: state %s", ErrSessionFinished, s.State)
	}
	if s.AllComplete() {
		return nil
	}

	s.Current++
	s.Progress = Progress{}

	if s.AllComplete() {
		return s.transition(StatePassed, now)
	}
	s.ChallengeDeadline = now.Add(challengeTimeout)
	return nil
}

// Fail ends the session, recording why for the audit trail.
func (s *Session) Fail(now time.Time, reason string) error {
	if s.State.Terminal() {
		return fmt.Errorf("%w: state %s", ErrSessionFinished, s.State)
	}
	if err := s.transition(StateFailed, now); err != nil {
		return err
	}
	s.FailureReason = reason
	return nil
}

// Expire ends the session because time ran out.
func (s *Session) Expire(now time.Time) error {
	if s.State.Terminal() {
		return fmt.Errorf("%w: state %s", ErrSessionFinished, s.State)
	}
	if err := s.transition(StateExpired, now); err != nil {
		return err
	}
	s.FailureReason = "session or challenge deadline elapsed"
	return nil
}

// transition applies a state change if the lifecycle allows it.
func (s *Session) transition(to State, _ time.Time) error {
	allowed, known := legalTransitions[s.State]
	if !known {
		return fmt.Errorf("%w: %s is not a state", ErrIllegalTransition, s.State)
	}

	for _, candidate := range allowed {
		if candidate == to {
			s.State = to
			s.Version++
			return nil
		}
	}
	return fmt.Errorf("%w: %s to %s", ErrIllegalTransition, s.State, to)
}
