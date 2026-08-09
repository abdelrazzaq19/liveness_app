package liveness

import (
	"context"
	"crypto/subtle"
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

// ErrWrongNonce means the caller does not hold the session's credentials.
//
// The nonce is what authorises a subject's browser to operate a session it was
// handed. It has to be checked on every session-scoped call, not only on
// frames, or the id alone would be enough to read or finish somebody else's
// verification.
var ErrWrongNonce = errors.New("liveness: session nonce does not match")

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

	// MaxChallengeRetries is how many times a challenge whose deadline elapsed
	// may be restarted before the session is given up on.
	//
	// Zero restores the original behaviour, where missing one challenge ends
	// everything. It is bounded because each retry is another attempt at the
	// same instruction, and an unbounded budget turns a challenge-response
	// protocol into a guessing game with no cost for guessing wrong.
	MaxChallengeRetries int

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
	if c.MaxChallengeRetries < 0 {
		problems = append(problems, fmt.Errorf("MaxChallengeRetries cannot be negative, got %d", c.MaxChallengeRetries))
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

	// Retried reports that the challenge ran out of time and was restarted on a
	// fresh deadline rather than ending the session.
	Retried bool `json:"retried"`

	// RetriesLeft is how many further restarts the session may still spend.
	RetriesLeft int `json:"retries_left"`

	// SecondsRemaining is how long is left on the current challenge.
	//
	// Sent as a duration rather than as a deadline on purpose. A client with a
	// skewed clock would render an absolute deadline wrongly and either rush
	// the subject or let them run out without warning; a countdown it merely
	// interpolates between responses cannot drift far, because another one
	// arrives several times a second.
	SecondsRemaining float64 `json:"seconds_remaining"`

	// Reason tells the subject what the frame was missing. It is empty when the
	// frame advanced the session.
	Reason string `json:"reason,omitempty"`
}

// progressResult builds the part of a result that describes where the session
// stands, so every return path reports the same thing.
func (s *Service) progressResult(session *Session, now time.Time) FrameResult {
	left := s.deps.Config.MaxChallengeRetries - session.Retries
	if left < 0 {
		left = 0
	}

	return FrameResult{
		State:            session.State,
		Challenge:        session.ActiveChallenge(),
		Remaining:        session.Remaining(),
		RetriesLeft:      left,
		SecondsRemaining: session.SecondsRemaining(now),
	}
}

// retryChallenge restarts the active challenge after its deadline elapsed.
//
// The frame that triggered this is deliberately not evaluated. It was captured
// during the attempt that just ended, so the subject may be halfway through a
// movement — and for the turn and nod challenges the first frame of an attempt
// becomes the baseline everything is measured against. Letting it through would
// hand the new attempt a baseline taken mid-turn.
func (s *Service) retryChallenge(ctx context.Context, session *Session, now time.Time) (FrameResult, error) {
	kind := session.ActiveChallenge()

	if err := session.RetryChallenge(now, s.deps.Config.ChallengeTimeout); err != nil {
		return FrameResult{}, err
	}

	s.deps.Logger.InfoContext(ctx, "liveness challenge retried",
		slog.String("session_id", session.ID.String()),
		slog.String("challenge", string(kind)),
		slog.Int("retries_used", session.Retries),
		slog.Int("retries_allowed", s.deps.Config.MaxChallengeRetries),
	)

	result := s.progressResult(session, now)
	result.Retried = true
	result.Reason = "time ran out for that step; try it once more"
	return s.save(ctx, session, result)
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

	// Authorisation first, and before any mutation. A caller who does not hold
	// the nonce must not be able to change the session in any way — including
	// ending it, which is what a wrong nonce used to do.
	if err := authorise(session, in.Nonce); err != nil {
		return FrameResult{}, err
	}

	now := s.deps.Clock.Now()

	if session.State.Terminal() {
		return FrameResult{State: session.State}, fmt.Errorf("%w: state %s", ErrSessionFinished, session.State)
	}
	// The session's own lifetime is the hard ceiling and nothing recovers from
	// it, so it is checked before the retry budget is consulted at all.
	if session.SessionExpired(now) {
		return s.endSession(ctx, session, now, expireSession, "")
	}
	if session.ChallengeExpired(now) {
		if session.Retries >= s.deps.Config.MaxChallengeRetries {
			return s.endSession(ctx, session, now, expireSession, "")
		}
		return s.retryChallenge(ctx, session, now)
	}
	if err := session.Begin(now); err != nil {
		return FrameResult{}, err
	}

	frame := Frame{Seq: in.Seq, PHash: in.PHash}

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
		// Logged here as well as after a successful analysis, because this is
		// the path a frame takes when the pipeline never produced a
		// measurement — and a frame that stops here leaves no trace in the
		// metrics log at all. Instrumenting only the happy path is how a
		// session where every single frame was rejected outright looked
		// identical to one where the thresholds were merely too tight.
		//
		// The error text carries the detail: which gate, and by how much.
		s.deps.Logger.DebugContext(ctx, "frame rejected before measurement",
			slog.String("session_id", session.ID.String()),
			slog.Int64("seq", in.Seq),
			slog.String("challenge", string(session.ActiveChallenge())),
			slog.Bool("recoverable", recoverableAnalysis(analyzeErr)),
			slog.String("error", analyzeErr.Error()),
		)

		if !recoverableAnalysis(analyzeErr) {
			return FrameResult{State: session.State}, fmt.Errorf("liveness: analyse frame: %w", analyzeErr)
		}
		// Unusable frame: record it so the sequence number is consumed, then
		// ask for another.
		s.deps.Guard.Record(session, frame, biometric.Face{})

		result := s.progressResult(session, now)
		result.Reason = analysisReason(analyzeErr)
		return s.save(ctx, session, result)
	}

	// Logged here, before anything branches on these numbers.
	//
	// Twice now this sat further down and hid the very failure being chased:
	// once after the analysis error check, so frames rejected for quality left
	// no trace, and once after the guard, so frames failed as spoofs left none
	// either. A measurement log that only runs when nothing went wrong is a
	// measurement log for the case nobody needs it.
	//
	// Scalars only. The rule against logging biometric data is not relaxed for
	// debugging: no frames, no embeddings, no landmark coordinates. These
	// numbers describe a moment, not a person, and none identifies anyone.
	if s.deps.Logger.Enabled(ctx, slog.LevelDebug) {
		s.deps.Logger.DebugContext(ctx, "frame measured",
			slog.String("session_id", session.ID.String()),
			slog.Int64("seq", in.Seq),
			slog.String("challenge", string(session.ActiveChallenge())),
			slog.Float64("ear", face.EAR),
			slog.Float64("mar", face.MAR),
			slog.Float64("yaw", face.Pose.Yaw),
			slog.Float64("pitch", face.Pose.Pitch),
			slog.Float64("face_width", face.Box.Width()),
			slog.Float64("detect_score", face.Score),

			// Alongside the threshold it is about to be compared against, so a
			// rejection can be read without going to look up the setting.
			slog.Float64("liveness", face.LivenessScore),
			slog.Float64("liveness_min", s.deps.Guard.MinLivenessScore),
		)
	}

	// A suspicion that is not being acted on still has to be visible, and at a
	// level someone reads. Otherwise the only record that the passive defence
	// is switched off lives in a configuration file nobody opens until after
	// the incident.
	if !s.deps.Guard.EnforceAntiSpoof && s.deps.Guard.SpoofSuspected(face) {
		s.deps.Logger.WarnContext(ctx, "passive anti-spoof would have rejected this frame, but enforcement is off",
			slog.String("session_id", session.ID.String()),
			slog.Int64("seq", in.Seq),
			slog.Float64("liveness", face.LivenessScore),
			slog.Float64("liveness_min", s.deps.Guard.MinLivenessScore),
		)
	}

	// A recoverable rejection must not stop the frame being evaluated.
	//
	// Every challenge asks the subject to hold a pose — eyes shut, mouth open,
	// head turned — and holding a pose is exactly what produces near-identical
	// frames. Returning early here threw the satisfying frame away and left a
	// subject doing precisely what was asked of them waiting out the clock.
	//
	// The still-image defence is untouched. What separates a photograph from a
	// person holding a pose is how long the stillness lasts, and a long streak
	// is fatal.
	if guardErr := s.deps.Guard.CheckAnalysis(session, frame, face); Fatal(guardErr) {
		return s.endSession(ctx, session, now, failSession, guardErr.Error())
	}

	outcome := s.deps.Evaluator.Evaluate(session, face)
	s.deps.Guard.Record(session, frame, face)

	if s.deps.Logger.Enabled(ctx, slog.LevelDebug) {
		s.deps.Logger.DebugContext(ctx, "challenge evaluated",
			slog.String("session_id", session.ID.String()),
			slog.Int64("seq", in.Seq),
			slog.String("challenge", string(session.ActiveChallenge())),
			slog.Bool("satisfied", outcome.Satisfied),
			slog.String("reason", outcome.Reason),
			slog.Float64("yaw_delta", face.Pose.Yaw-session.Progress.BaselineYaw),
			slog.Float64("pitch_delta", face.Pose.Pitch-session.Progress.BaselinePitch),
			slog.Int("closed_frames", session.Progress.ClosedFrames),
		)
	}

	if outcome.Satisfied {
		if err := session.Advance(now, s.deps.Config.ChallengeTimeout); err != nil {
			return FrameResult{}, err
		}

		// Rebuilt after advancing, so the countdown the client shows is the new
		// challenge's rather than the one just finished.
		result := s.progressResult(session, now)
		result.Advanced = true
		result.Completed = session.AllComplete()
		return s.save(ctx, session, result)
	}

	result := s.progressResult(session, now)
	result.Reason = outcome.Reason
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
func (s *Service) Complete(ctx context.Context, id SessionID, nonce string) (Verdict, error) {
	session, err := s.deps.Sessions.Get(ctx, id)
	if err != nil {
		return Verdict{}, err
	}
	if err := authorise(session, nonce); err != nil {
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
func (s *Service) Get(ctx context.Context, id SessionID, nonce string) (*Session, error) {
	session, err := s.deps.Sessions.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := authorise(session, nonce); err != nil {
		return nil, err
	}

	// An expired session that nobody has touched still reads as in progress
	// until someone looks at it.
	//
	// Only the session's own lifetime ends it here. A challenge deadline that
	// elapsed may still be recoverable, and deciding that is the frame path's
	// job — reading the status must not be what burns a retry.
	if !session.State.Terminal() && session.SessionExpired(s.deps.Clock.Now()) {
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

// authorise checks that the caller holds the session's nonce.
//
// Compared in constant time. The comparison is cheap either way, and a timing
// side channel on a 128-bit capability is exactly the sort of thing that is
// obvious in hindsight.
func authorise(session *Session, nonce string) error {
	if subtle.ConstantTimeCompare([]byte(nonce), []byte(session.Nonce)) != 1 {
		return ErrWrongNonce
	}
	return nil
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

	// Checked before the general quality case, which it is a kind of. A subject
	// whose light is fine and whose face is merely small was being told to hold
	// steady in good light — advice they could follow perfectly and still be
	// refused by every frame.
	case errors.Is(err, biometric.ErrFaceTooSmall):
		return "move closer to the camera"

	case errors.Is(err, biometric.ErrLowQuality):
		return "hold steady and make sure your face is well lit"
	default:
		return "frame could not be used"
	}
}
