package liveness

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"time"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

// ErrSessionNotFound means no session exists with that id.
var ErrSessionNotFound = errors.New("liveness: session not found")

// ErrVersionConflict means the session changed underneath an update.
//
// Frames from one session can arrive concurrently; the loser retries rather
// than silently discarding the winner's progress.
var ErrVersionConflict = errors.New("liveness: session was modified concurrently")

// Repository persists sessions.
type Repository interface {
	Create(ctx context.Context, s *Session) error
	Get(ctx context.Context, id SessionID) (*Session, error)

	// Update must fail with ErrVersionConflict when the stored version no
	// longer matches the one the caller read.
	Update(ctx context.Context, s *Session) error

	DeleteExpired(ctx context.Context, before time.Time) (int, error)
}

// IDGenerator produces session identifiers.
type IDGenerator interface {
	NewID() (SessionID, error)
}

// Config is what the service needs from configuration.
type Config struct {
	TTL              time.Duration
	ChallengeTimeout time.Duration
	ChallengeCount   int

	// KeyFrameInterval is how often a frame gets an embedding.
	//
	// Embedding is by far the heaviest stage — measured at 71% of the whole
	// pipeline — and identity only has to be re-checked periodically, not on
	// every frame. Setting this to 1 makes the service unable to keep up with
	// a camera.
	KeyFrameInterval int
}

func (c Config) validate() error {
	var problems []error
	if c.TTL <= 0 {
		problems = append(problems, fmt.Errorf("TTL must be positive, got %s", c.TTL))
	}
	if c.ChallengeTimeout <= 0 {
		problems = append(problems, fmt.Errorf("ChallengeTimeout must be positive, got %s", c.ChallengeTimeout))
	}
	if c.ChallengeCount < 1 {
		problems = append(problems, fmt.Errorf("ChallengeCount must be at least 1, got %d", c.ChallengeCount))
	}
	if c.KeyFrameInterval < 1 {
		problems = append(problems, fmt.Errorf("KeyFrameInterval must be at least 1, got %d", c.KeyFrameInterval))
	}
	return errors.Join(problems...)
}

// Deps is everything the service needs built for it.
type Deps struct {
	Sessions  Repository
	Analyzer  biometric.Analyzer
	Evaluator Evaluator
	Guard     Guard
	Clock     Clock
	IDs       IDGenerator
	Entropy   io.Reader
	Logger    *slog.Logger
	Config    Config
}

// Service runs verification sessions.
type Service struct {
	deps Deps
}

// NewService validates the wiring and the thresholds.
func NewService(d Deps) (*Service, error) {
	var problems []error

	if d.Sessions == nil {
		problems = append(problems, errors.New("Sessions repository is required"))
	}
	if d.Analyzer == nil {
		problems = append(problems, errors.New("Analyzer is required"))
	}
	if d.Clock == nil {
		problems = append(problems, errors.New("Clock is required"))
	}
	if d.IDs == nil {
		problems = append(problems, errors.New("IDs generator is required"))
	}
	if d.Logger == nil {
		problems = append(problems, errors.New("Logger is required"))
	}
	if err := d.Config.validate(); err != nil {
		problems = append(problems, err)
	}
	if err := d.Evaluator.Thresholds.Validate(); err != nil {
		problems = append(problems, err)
	}
	if err := d.Guard.Validate(); err != nil {
		problems = append(problems, err)
	}

	if err := errors.Join(problems...); err != nil {
		return nil, fmt.Errorf("liveness: service: %w", err)
	}
	return &Service{deps: d}, nil
}

// Start creates a session and returns it with its challenges.
func (s *Service) Start(ctx context.Context) (*Session, error) {
	id, err := s.deps.IDs.NewID()
	if err != nil {
		return nil, fmt.Errorf("liveness: new session id: %w", err)
	}

	session, err := NewSession(s.deps.Clock.Now(), NewSessionParams{
		ID:               id,
		ChallengeCount:   s.deps.Config.ChallengeCount,
		TTL:              s.deps.Config.TTL,
		ChallengeTimeout: s.deps.Config.ChallengeTimeout,
		Entropy:          s.deps.Entropy,
	})
	if err != nil {
		return nil, err
	}

	if err := s.deps.Sessions.Create(ctx, session); err != nil {
		return nil, fmt.Errorf("liveness: persist session: %w", err)
	}

	s.deps.Logger.InfoContext(ctx, "liveness session started",
		slog.String("session_id", id.String()),
		slog.Int("challenges", len(session.Challenges)),
	)
	return session, nil
}

// FrameInput is one frame submitted against a session.
type FrameInput struct {
	Seq   int64
	Nonce string
	PHash uint64
	Image image.Image
}

// FrameResult is what a submitted frame did.
type FrameResult struct {
	State     State         `json:"state"`
	Challenge ChallengeKind `json:"challenge,omitempty"`
	Advanced  bool          `json:"advanced"`
	Completed bool          `json:"completed"`
	Remaining int           `json:"remaining"`

	// Reason tells the subject what the frame was missing. It is empty when the
	// frame advanced the session.
	Reason string `json:"reason,omitempty"`
}

// SubmitFrame evaluates one frame against the active challenge.
//
// A frame rejected for quality, for having no face, or for being identical to
// the last one does NOT fail the session: the caller is expected to send
// another. Only a replay, a spoof, or a change of face ends it. That split is
// the difference between a service a real person can use and one that fails
// them for blinking at the wrong moment.
func (s *Service) SubmitFrame(ctx context.Context, id SessionID, in FrameInput) (FrameResult, error) {
	session, err := s.deps.Sessions.Get(ctx, id)
	if err != nil {
		return FrameResult{}, err
	}

	now := s.deps.Clock.Now()

	if session.State.Terminal() {
		return FrameResult{State: session.State}, fmt.Errorf("%w: state %s", ErrSessionFinished, session.State)
	}
	if session.Expired(now) {
		return s.endSession(ctx, session, now, expireSession, "")
	}
	if err := session.Begin(now); err != nil {
		return FrameResult{}, err
	}

	frame := Frame{Seq: in.Seq, Nonce: in.Nonce, PHash: in.PHash}

	// The cheap defences first: a replayed sequence number must not cost four
	// model inferences.
	if err := s.deps.Guard.CheckRequest(session, frame); err != nil {
		return s.endSession(ctx, session, now, failSession, err.Error())
	}

	keyFrame := s.isKeyFrame(session)
	face, analyzeErr := s.deps.Analyzer.Analyze(ctx, in.Image, biometric.AnalyzeOptions{
		SkipEmbedding: !keyFrame,
	})

	if analyzeErr != nil {
		if !recoverableAnalysis(analyzeErr) {
			return FrameResult{State: session.State}, fmt.Errorf("liveness: analyse frame: %w", analyzeErr)
		}
		// Unusable frame: record it so the sequence number is consumed, then
		// ask for another.
		s.deps.Guard.Record(session, frame, biometric.Face{})
		return s.save(ctx, session, FrameResult{
			State:     session.State,
			Challenge: session.ActiveChallenge(),
			Remaining: session.Remaining(),
			Reason:    analysisReason(analyzeErr),
		})
	}

	if err := s.deps.Guard.CheckAnalysis(session, frame, face); err != nil {
		if Fatal(err) {
			return s.endSession(ctx, session, now, failSession, err.Error())
		}
		s.deps.Guard.Record(session, frame, face)
		return s.save(ctx, session, FrameResult{
			State:     session.State,
			Challenge: session.ActiveChallenge(),
			Remaining: session.Remaining(),
			Reason:    "hold still for a moment, then follow the instruction",
		})
	}

	outcome := s.deps.Evaluator.Evaluate(session, face)
	s.deps.Guard.Record(session, frame, face)

	result := FrameResult{
		State:     session.State,
		Challenge: session.ActiveChallenge(),
		Remaining: session.Remaining(),
		Reason:    outcome.Reason,
	}

	if outcome.Satisfied {
		if err := session.Advance(now, s.deps.Config.ChallengeTimeout); err != nil {
			return FrameResult{}, err
		}
		result.Advanced = true
		result.Reason = ""
		result.Challenge = session.ActiveChallenge()
		result.Remaining = session.Remaining()
		result.Completed = session.AllComplete()
		result.State = session.State
	}

	return s.save(ctx, session, result)
}

// Verdict is the outcome of a completed session.
type Verdict struct {
	SessionID SessionID `json:"session_id"`
	State     State     `json:"state"`
	Passed    bool      `json:"passed"`
}

// Complete finalises a session.
//
// It refuses a session that has not satisfied every challenge, rather than
// quietly passing one that ran out of instructions to follow.
func (s *Service) Complete(ctx context.Context, id SessionID) (Verdict, error) {
	session, err := s.deps.Sessions.Get(ctx, id)
	if err != nil {
		return Verdict{}, err
	}

	now := s.deps.Clock.Now()

	if session.State.Terminal() {
		return Verdict{SessionID: id, State: session.State, Passed: session.State == StatePassed}, nil
	}
	if session.Expired(now) {
		if _, err := s.endSession(ctx, session, now, expireSession, ""); err != nil && !errors.Is(err, ErrSessionExpired) {
			return Verdict{}, err
		}
		return Verdict{SessionID: id, State: session.State}, ErrSessionExpired
	}
	if !session.AllComplete() {
		return Verdict{SessionID: id, State: session.State},
			fmt.Errorf("%w: %d of %d remaining", ErrChallengesIncomplete, session.Remaining(), len(session.Challenges))
	}

	// AllComplete without a terminal state should be unreachable, since Advance
	// passes the session on the last challenge. Handle it rather than assume.
	if err := session.Advance(now, s.deps.Config.ChallengeTimeout); err != nil {
		return Verdict{}, err
	}
	if _, err := s.save(ctx, session, FrameResult{}); err != nil {
		return Verdict{}, err
	}

	return Verdict{SessionID: id, State: session.State, Passed: session.State == StatePassed}, nil
}

// Get returns a session.
func (s *Service) Get(ctx context.Context, id SessionID) (*Session, error) {
	session, err := s.deps.Sessions.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	// An expired session that nobody has touched still reads as in progress
	// until someone looks at it.
	if !session.State.Terminal() && session.Expired(s.deps.Clock.Now()) {
		if _, err := s.endSession(ctx, session, s.deps.Clock.Now(), expireSession, ""); err != nil &&
			!errors.Is(err, ErrSessionExpired) {
			return nil, err
		}
	}
	return session, nil
}

// PurgeExpired removes sessions that ended before the cutoff.
func (s *Service) PurgeExpired(ctx context.Context, before time.Time) (int, error) {
	return s.deps.Sessions.DeleteExpired(ctx, before)
}

// isKeyFrame decides whether this frame gets an embedding.
//
// The first frame always does, so the session has an identity to compare
// against from the start.
func (s *Service) isKeyFrame(session *Session) bool {
	if len(session.ReferenceEmbedding) == 0 {
		return true
	}
	return session.LastSeq%int64(s.deps.Config.KeyFrameInterval) == 0
}

type endKind int

const (
	failSession endKind = iota
	expireSession
)

// endSession moves a session to a terminal state and persists it.
func (s *Service) endSession(ctx context.Context, session *Session, now time.Time, kind endKind, reason string) (FrameResult, error) {
	var endErr error
	switch kind {
	case expireSession:
		endErr = ErrSessionExpired
		if err := session.Expire(now); err != nil {
			return FrameResult{}, err
		}
	default:
		endErr = errors.New(reason)
		if err := session.Fail(now, reason); err != nil {
			return FrameResult{}, err
		}
	}

	// The reason is logged, never returned to the client: it names which
	// defence fired, and an attacker who learns that can work around it.
	s.deps.Logger.WarnContext(ctx, "liveness session ended",
		slog.String("session_id", session.ID.String()),
		slog.String("state", string(session.State)),
		slog.String("reason", session.FailureReason),
	)

	if _, err := s.save(ctx, session, FrameResult{}); err != nil {
		return FrameResult{}, err
	}
	return FrameResult{State: session.State}, endErr
}

func (s *Service) save(ctx context.Context, session *Session, result FrameResult) (FrameResult, error) {
	if err := s.deps.Sessions.Update(ctx, session); err != nil {
		return FrameResult{}, err
	}
	return result, nil
}

// recoverableAnalysis reports whether a pipeline error means "send another
// frame" rather than "something is broken".
func recoverableAnalysis(err error) bool {
	return errors.Is(err, biometric.ErrNoFaceFound) || errors.Is(err, biometric.ErrLowQuality)
}

func analysisReason(err error) string {
	switch {
	case errors.Is(err, biometric.ErrNoFaceFound):
		return "no face in view"
	case errors.Is(err, biometric.ErrLowQuality):
		return "hold steady and make sure your face is well lit"
	default:
		return "frame could not be used"
	}
}
