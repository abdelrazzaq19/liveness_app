package liveness

import (
	"errors"
	"fmt"
	"math"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

// Thresholds are the numbers a challenge is judged against.
//
// ⚠ Every value here arrives from configuration and every default is a
// literature figure for a different landmark scheme. They have been shown not
// to transfer: on a mean face with a closed mouth the measured MAR is 0.520
// against a 0.55 "open" threshold. Until the calibration harness produces
// measured values, a challenge decision built on these is provisional.
type Thresholds struct {
	// BlinkCloseRatio and BlinkOpenRatio are fractions of the subject's own
	// widest observed eye opening, not absolute ratios.
	//
	// Absolute thresholds were tried first and were provably unsatisfiable. On
	// a real session this pipeline produced eye aspect ratios between 0.030 and
	// 0.300 with a mean of 0.124, against literature figures of 0.21 for shut
	// and 0.30 for open — 0 frames out of 34 could ever register as open, so no
	// amount of blinking could complete the challenge.
	//
	// The literature values are for a 68-point landmark scheme; this model
	// emits 106 and puts its eye points elsewhere, so the ratio it produces is
	// on a different scale entirely. Measuring against the subject's own
	// baseline sidesteps both that and the fact that eyes differ between
	// people. It is the same reasoning already applied to head turns, which are
	// measured as movement rather than as an absolute angle.
	BlinkCloseRatio float64
	BlinkOpenRatio  float64

	// BlinkMinFrames is how many consecutive shut frames a blink needs, which
	// is what separates a blink from one noisy landmark reading.
	BlinkMinFrames int

	// YawTurnDeg and PitchNodDeg are movements away from the pose the subject
	// started the challenge in, not absolute angles.
	YawTurnDeg  float64
	PitchNodDeg float64

	MARMouthOpen float64
}

// Validate reports thresholds that cannot work.
func (t Thresholds) Validate() error {
	var problems []error
	if t.BlinkCloseRatio <= 0 || t.BlinkCloseRatio >= 1 {
		problems = append(problems, fmt.Errorf("BlinkCloseRatio must be in (0,1), got %g", t.BlinkCloseRatio))
	}
	if t.BlinkOpenRatio <= t.BlinkCloseRatio || t.BlinkOpenRatio > 1 {
		problems = append(problems, fmt.Errorf(
			"BlinkOpenRatio (%g) must be above BlinkCloseRatio (%g) and at most 1, or blink detection flaps",
			t.BlinkOpenRatio, t.BlinkCloseRatio))
	}
	if t.BlinkMinFrames < 1 {
		problems = append(problems, fmt.Errorf("BlinkMinFrames must be at least 1, got %d", t.BlinkMinFrames))
	}
	if t.YawTurnDeg <= 0 {
		problems = append(problems, fmt.Errorf("YawTurnDeg must be positive, got %g", t.YawTurnDeg))
	}
	if t.PitchNodDeg <= 0 {
		problems = append(problems, fmt.Errorf("PitchNodDeg must be positive, got %g", t.PitchNodDeg))
	}
	if t.MARMouthOpen <= 0 {
		problems = append(problems, fmt.Errorf("MARMouthOpen must be positive, got %g", t.MARMouthOpen))
	}
	return errors.Join(problems...)
}

// Evaluator decides whether a frame satisfies the challenge in progress.
//
// It is a pure function of the session's progress and the frame: no clock, no
// storage, no randomness. That is what lets a whole session be replayed in a
// table-driven test.
type Evaluator struct {
	Thresholds Thresholds
}

// Outcome is what a single frame did for the active challenge.
type Outcome struct {
	// Satisfied reports that the challenge is now complete.
	Satisfied bool

	// Reason explains what the frame was missing, for the client to show the
	// subject. It is empty when the challenge is satisfied.
	Reason string
}

// Evaluate updates the session's progress and reports whether the active
// challenge is now satisfied.
//
// It mutates only Session.Progress. Advancing, failing, and expiring are the
// service's decisions, not this one's.
func (e Evaluator) Evaluate(s *Session, face biometric.Face) Outcome {
	switch s.ActiveChallenge() {
	case ChallengeBlink:
		return e.blink(s, face)
	case ChallengeTurnLeft:
		return e.turn(s, face, -1)
	case ChallengeTurnRight:
		return e.turn(s, face, +1)
	case ChallengeNod:
		return e.nod(s, face)
	case ChallengeMouthOpen:
		return e.mouthOpen(face)
	case "":
		return Outcome{Satisfied: true}
	default:
		return Outcome{Reason: fmt.Sprintf("unknown challenge %q", s.ActiveChallenge())}
	}
}

// blink requires the eyes to close and then open again.
//
// Requiring the reopening matters: without it a subject who simply keeps their
// eyes shut satisfies the challenge, and so does a photograph of someone with
// their eyes closed.
func (e Evaluator) blink(s *Session, face biometric.Face) Outcome {
	// The widest the eyes have been seen is the reference. A running maximum
	// rather than the first frame's value, because a subject who arrives
	// mid-blink would otherwise anchor the whole challenge to their shut eyes
	// and never be able to satisfy it.
	if face.EAR > s.Progress.PeakEAR {
		s.Progress.PeakEAR = face.EAR
	}
	if s.Progress.PeakEAR <= 0 {
		return Outcome{Reason: "blink"}
	}

	shut := s.Progress.PeakEAR * e.Thresholds.BlinkCloseRatio
	open := s.Progress.PeakEAR * e.Thresholds.BlinkOpenRatio

	switch {
	case face.EAR < shut:
		s.Progress.ClosedFrames++
		if s.Progress.ClosedFrames >= e.Thresholds.BlinkMinFrames {
			s.Progress.SawClose = true
		}
		return Outcome{Reason: "keep going, now open your eyes"}

	case face.EAR > open:
		if s.Progress.SawClose {
			return Outcome{Satisfied: true}
		}
		// Eyes open and never shut: start over rather than accumulating
		// unrelated frames towards the count.
		s.Progress.ClosedFrames = 0
		return Outcome{Reason: "blink"}

	default:
		// Inside the hysteresis band. Neither shut nor open, so nothing
		// changes; this is the gap that stops a wavering ratio from
		// registering a blink on its own.
		return Outcome{Reason: "blink"}
	}
}

// turn requires the head to rotate away from where it started.
//
// Measured as a movement rather than an absolute angle: a subject who happens
// to be sitting at an angle to the camera must still turn, and one facing
// slightly the wrong way must not be refused for it.
//
// direction is -1 for a turn towards the left of the image and +1 for the
// right, matching biometric.Pose. The instruction the subject reads is the
// interface's problem: a mirrored camera preview reverses it.
func (e Evaluator) turn(s *Session, face biometric.Face, direction float64) Outcome {
	if !s.Progress.HaveBaseline {
		s.Progress.BaselineYaw = face.Pose.Yaw
		s.Progress.BaselinePitch = face.Pose.Pitch
		s.Progress.HaveBaseline = true
	}

	delta := face.Pose.Yaw - s.Progress.BaselineYaw
	if delta*direction >= e.Thresholds.YawTurnDeg {
		return Outcome{Satisfied: true}
	}

	if direction < 0 {
		return Outcome{Reason: "turn further"}
	}
	return Outcome{Reason: "turn further"}
}

// nod accepts movement in either direction.
//
// Pitch is the weakest axis of the pose estimate — it is read from how much the
// face's depth foreshortens, and a face is nearly flat from the camera's point
// of view. Insisting on a down-then-up sequence would fail honest subjects on
// noise, so a single clear movement is enough.
func (e Evaluator) nod(s *Session, face biometric.Face) Outcome {
	if !s.Progress.HaveBaseline {
		s.Progress.BaselineYaw = face.Pose.Yaw
		s.Progress.BaselinePitch = face.Pose.Pitch
		s.Progress.HaveBaseline = true
	}

	if math.Abs(face.Pose.Pitch-s.Progress.BaselinePitch) >= e.Thresholds.PitchNodDeg {
		return Outcome{Satisfied: true}
	}
	return Outcome{Reason: "nod further"}
}

func (e Evaluator) mouthOpen(face biometric.Face) Outcome {
	if face.MAR >= e.Thresholds.MARMouthOpen {
		return Outcome{Satisfied: true}
	}
	return Outcome{Reason: "open your mouth wider"}
}
