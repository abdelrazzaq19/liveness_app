package liveness

import (
	"errors"
	"fmt"
	"math/bits"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

// Replay defences, each with its own error so a caller can tell a recoverable
// rejection from a fatal one.
var (
	// ErrSequenceReplay means a frame arrived out of order or twice.
	ErrSequenceReplay = errors.New("liveness: frame sequence went backwards or repeated")

	// ErrDuplicateFrame means this frame is visually identical to a recent one.
	//
	// Recoverable. A subject holding still produces near-identical frames too,
	// and failing them would reject honest people for sitting steady.
	ErrDuplicateFrame = errors.New("liveness: frame is identical to a recent one")

	// ErrStaticReplay means nothing has changed for long enough that the camera
	// is looking at a still image rather than a person.
	ErrStaticReplay = errors.New("liveness: the scene has not changed; a still image is being presented")

	// ErrSpoofDetected means the passive check rejected the frame.
	ErrSpoofDetected = errors.New("liveness: frame flagged as a spoof")

	// ErrIdentityChanged means the face is no longer the one the session
	// started with.
	ErrIdentityChanged = errors.New("liveness: the face changed during the session")
)

// Frame is what the client sends alongside the image.
//
// The nonce is not here. Holding it is what authorises the caller to touch the
// session at all, so it is checked before any of this runs — and checked as
// authorisation rather than as a replay defence. Treating a wrong nonce as a
// failed session, which is where it used to live, let anyone who learned a
// session id destroy somebody else's verification by sending one bad frame.
type Frame struct {
	// Seq must increase strictly across a session.
	Seq int64

	// PHash is the perceptual hash of the decoded frame.
	PHash uint64
}

// Guard applies the replay defences that look at a single frame.
//
// Three of the six defences in the design live elsewhere, because they are
// properties of the session rather than of a frame: the challenge order is
// drawn in challenge.go, and both deadlines are enforced by Session.Expired.
// The three here are the ones a frame can violate.
type Guard struct {
	// MinLivenessScore is the passive anti-spoof threshold.
	MinLivenessScore float64

	// IdentityCosineMin is the similarity a key frame must keep with the
	// session's first one.
	//
	// Embeddings are float32, so a similarity cannot land on this threshold
	// exactly; treat a value within about 1e-7 of it as either side of the
	// line. That matters when reading a rejection near the boundary, not when
	// choosing the number.
	IdentityCosineMin float64

	// PHashMinDistance is the Hamming distance below which two frames count as
	// the same picture.
	PHashMinDistance int

	// MaxDuplicateStreak is how many identical frames in a row are tolerated
	// before the session is failed as a still image.
	//
	// A live subject holding still trips the duplicate check too, so a single
	// duplicate cannot be fatal. What separates a person from a photograph is
	// that a person eventually moves — the challenges see to that.
	MaxDuplicateStreak int

	// MaxRecentHashes bounds the history kept per session.
	//
	// A replayed still image matches the immediately preceding frames, so a
	// short window catches it. An unbounded one would grow a database row by
	// eight bytes per frame for the whole session and buy nothing.
	MaxRecentHashes int
}

// Validate reports a guard that cannot work.
func (g Guard) Validate() error {
	var problems []error
	if g.MinLivenessScore < 0 || g.MinLivenessScore > 1 {
		problems = append(problems, fmt.Errorf("MinLivenessScore must be in [0,1], got %g", g.MinLivenessScore))
	}
	if g.IdentityCosineMin < -1 || g.IdentityCosineMin > 1 {
		problems = append(problems, fmt.Errorf("IdentityCosineMin must be in [-1,1], got %g", g.IdentityCosineMin))
	}
	if g.PHashMinDistance < 0 || g.PHashMinDistance > 64 {
		problems = append(problems, fmt.Errorf("PHashMinDistance must be in [0,64], got %d", g.PHashMinDistance))
	}
	if g.MaxDuplicateStreak < 1 {
		problems = append(problems, fmt.Errorf("MaxDuplicateStreak must be at least 1, got %d", g.MaxDuplicateStreak))
	}
	if g.MaxRecentHashes < 1 {
		problems = append(problems, fmt.Errorf("MaxRecentHashes must be at least 1, got %d", g.MaxRecentHashes))
	}
	return errors.Join(problems...)
}

// Fatal reports whether an error from Check ends the session.
//
// The distinction is the difference between asking the client for another
// frame and telling the subject they failed.
func Fatal(err error) bool {
	switch {
	case errors.Is(err, ErrDuplicateFrame):
		return false
	case err == nil:
		return false
	default:
		return true
	}
}

// Check applies every defence a single frame can violate.
//
// The order is cheapest and most certain first: a wrong nonce or a replayed
// sequence number is a fact about the request, while a spoof score is a
// judgement about the pixels.
func (g Guard) Check(s *Session, f Frame, face biometric.Face) error {
	if err := g.CheckRequest(s, f); err != nil {
		return err
	}
	return g.CheckAnalysis(s, f, face)
}

// CheckRequest applies the defences that need no image analysis.
//
// Separated so a replayed sequence number costs one comparison rather than four
// model inferences. Rejecting the cheap way round is the difference between an
// attacker wasting their own time and wasting the service's.
func (g Guard) CheckRequest(s *Session, f Frame) error {
	if f.Seq <= s.LastSeq {
		return fmt.Errorf("%w: got %d, last accepted %d", ErrSequenceReplay, f.Seq, s.LastSeq)
	}
	return nil
}

// CheckAnalysis applies the defences that need the frame to have been analysed.
func (g Guard) CheckAnalysis(s *Session, f Frame, face biometric.Face) error {
	if g.isDuplicate(s, f.PHash) {
		if s.DuplicateStreak+1 >= g.MaxDuplicateStreak {
			return fmt.Errorf("%w: %d identical frames in a row", ErrStaticReplay, s.DuplicateStreak+1)
		}
		return ErrDuplicateFrame
	}

	if face.LivenessScore < g.MinLivenessScore {
		// The score itself is not in the error: it goes to the audit trail, not
		// to a client who could use it to tune an attack.
		return ErrSpoofDetected
	}

	if err := g.checkIdentity(s, face); err != nil {
		return err
	}
	return nil
}

// checkIdentity compares a key frame against the session's reference.
//
// Only key frames carry an embedding, so most frames skip this. Without it, an
// attacker can satisfy the challenges with their own face and then present the
// victim's for the frame that matters.
func (g Guard) checkIdentity(s *Session, face biometric.Face) error {
	if len(face.Embedding) == 0 || len(s.ReferenceEmbedding) == 0 {
		return nil
	}
	if similarity := s.ReferenceEmbedding.Cosine(face.Embedding); similarity < g.IdentityCosineMin {
		return ErrIdentityChanged
	}
	return nil
}

// isDuplicate reports whether the hash is within the threshold of a recent one.
func (g Guard) isDuplicate(s *Session, hash uint64) bool {
	for _, seen := range s.RecentHashes {
		if bits.OnesCount64(seen^hash) < g.PHashMinDistance {
			return true
		}
	}
	return false
}

// Record folds an accepted frame into the session's replay state.
//
// Called only after Check returns nil or a recoverable error: a frame that
// failed fatally must not advance the sequence, or a rejected attempt would
// consume sequence numbers the honest client still needs.
func (g Guard) Record(s *Session, f Frame, face biometric.Face) {
	s.LastSeq = f.Seq

	if g.isDuplicate(s, f.PHash) {
		s.DuplicateStreak++
	} else {
		s.DuplicateStreak = 0
	}

	s.RecentHashes = append(s.RecentHashes, f.PHash)
	if overflow := len(s.RecentHashes) - g.MaxRecentHashes; overflow > 0 {
		s.RecentHashes = append(s.RecentHashes[:0], s.RecentHashes[overflow:]...)
	}

	// The first key frame becomes the identity every later one is measured
	// against.
	if len(s.ReferenceEmbedding) == 0 && len(face.Embedding) > 0 {
		s.ReferenceEmbedding = face.Embedding
	}
}
