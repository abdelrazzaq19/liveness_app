package liveness

import (
	"strings"
	"testing"
	"time"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

func testThresholds() Thresholds {
	return Thresholds{
		BlinkCloseRatio: 0.60,
		BlinkOpenRatio:  0.85,
		BlinkMinFrames:  2,
		YawTurnDeg:      25,
		PitchNodDeg:     15,
		MARMouthOpen:    0.55,
	}
}

// sessionFor builds a session whose only challenge is the given one, so a test
// can drive it without caring what the generator drew.
func sessionFor(t *testing.T, kind ChallengeKind) *Session {
	t.Helper()

	s, err := NewSession(testStart, NewSessionParams{
		ID: "s", ChallengeCount: 1, TTL: time.Minute, ChallengeTimeout: 20 * time.Second,
		Entropy: fixedEntropy(3),
	})
	if err != nil {
		t.Fatalf("NewSession() returned an unexpected error: %v", err)
	}
	s.Challenges = []ChallengeKind{kind}
	if err := s.Begin(testStart); err != nil {
		t.Fatalf("Begin() returned an unexpected error: %v", err)
	}
	return s
}

// frame is shorthand for the fields the evaluator reads.
func frame(ear, mar, yaw, pitch float64) biometric.Face {
	return biometric.Face{
		EAR:  ear,
		MAR:  mar,
		Pose: biometric.Pose{Yaw: yaw, Pitch: pitch},
	}
}

// play runs a sequence of frames and returns the index at which the challenge
// was satisfied, or -1.
func play(e Evaluator, s *Session, frames []biometric.Face) int {
	for i, f := range frames {
		if e.Evaluate(s, f).Satisfied {
			return i
		}
	}
	return -1
}

func TestThresholdsValidation(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*Thresholds)
		wantHint string
	}{
		{"valid", func(*Thresholds) {}, ""},
		{"close ratio out of range", func(t *Thresholds) { t.BlinkCloseRatio = 1.5 }, "BlinkCloseRatio"},
		{"open ratio below close", func(t *Thresholds) { t.BlinkOpenRatio = 0.1 }, "BlinkOpenRatio"},
		{"open ratio equal to close", func(t *Thresholds) { t.BlinkOpenRatio = t.BlinkCloseRatio }, "BlinkOpenRatio"},
		{"zero blink frames", func(t *Thresholds) { t.BlinkMinFrames = 0 }, "BlinkMinFrames"},
		{"negative yaw", func(t *Thresholds) { t.YawTurnDeg = -5 }, "YawTurnDeg"},
		{"zero pitch", func(t *Thresholds) { t.PitchNodDeg = 0 }, "PitchNodDeg"},
		{"zero mouth", func(t *Thresholds) { t.MARMouthOpen = 0 }, "MARMouthOpen"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			th := testThresholds()
			tt.mutate(&th)

			err := th.Validate()
			if tt.wantHint == "" {
				if err != nil {
					t.Fatalf("Validate() rejected valid thresholds: %v", err)
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

// Requiring the eyes to reopen is what separates a blink from a photograph of
// someone with their eyes shut.
func TestBlinkRequiresCloseThenOpen(t *testing.T) {
	e := Evaluator{Thresholds: testThresholds()}

	tests := []struct {
		name    string
		frames  []biometric.Face
		wantIdx int
	}{
		{
			name: "closed for long enough, then open",
			frames: []biometric.Face{
				frame(0.35, 0, 0, 0), // open
				frame(0.15, 0, 0, 0), // shut 1
				frame(0.14, 0, 0, 0), // shut 2, reaches the minimum
				frame(0.36, 0, 0, 0), // open again
			},
			wantIdx: 3,
		},
		{
			name: "eyes stay shut",
			frames: []biometric.Face{
				frame(0.12, 0, 0, 0), frame(0.11, 0, 0, 0),
				frame(0.10, 0, 0, 0), frame(0.09, 0, 0, 0),
			},
			wantIdx: -1,
		},
		{
			name: "eyes never shut",
			frames: []biometric.Face{
				frame(0.40, 0, 0, 0), frame(0.38, 0, 0, 0), frame(0.41, 0, 0, 0),
			},
			wantIdx: -1,
		},
		{
			name: "one shut frame is not enough",
			frames: []biometric.Face{
				frame(0.15, 0, 0, 0), // shut 1, below the minimum of 2
				frame(0.40, 0, 0, 0), // open again
				frame(0.40, 0, 0, 0),
			},
			wantIdx: -1,
		},
		{
			name: "hovering in the hysteresis band does nothing",
			frames: []biometric.Face{
				frame(0.25, 0, 0, 0), frame(0.26, 0, 0, 0),
				frame(0.24, 0, 0, 0), frame(0.27, 0, 0, 0),
			},
			wantIdx: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := sessionFor(t, ChallengeBlink)
			if got := play(e, s, tt.frames); got != tt.wantIdx {
				t.Errorf("satisfied at frame %d, want %d", got, tt.wantIdx)
			}
		})
	}
}

// A run of shut frames interrupted by an open one must not accumulate towards
// the count, or noise on either side of the threshold would add up to a blink.
func TestBlinkCounterResetsWhenTheEyesOpen(t *testing.T) {
	e := Evaluator{Thresholds: testThresholds()}
	s := sessionFor(t, ChallengeBlink)

	// The eyes have to be seen open before a blink means anything: the widest
	// opening observed is what the drop is measured against. Values are the ones
	// this pipeline actually produces, not the published 68-point figures.
	e.Evaluate(s, frame(0.28, 0, 0, 0)) // open, establishes the reference

	e.Evaluate(s, frame(0.08, 0, 0, 0)) // shut 1
	if s.Progress.ClosedFrames != 1 {
		t.Fatalf("closed frames = %d, want 1", s.Progress.ClosedFrames)
	}

	e.Evaluate(s, frame(0.27, 0, 0, 0)) // open again, below the minimum
	if s.Progress.ClosedFrames != 0 {
		t.Errorf("closed frames = %d after the eyes reopened, want 0", s.Progress.ClosedFrames)
	}
	if s.Progress.SawClose {
		t.Error("a single shut frame set SawClose")
	}
}

// The values here are the ones this pipeline produced on a real session: eye
// aspect ratios between 0.030 and 0.300, mean 0.124, against published 68-point
// figures of 0.21 shut and 0.30 open.
//
// Under those absolute thresholds not one of 34 frames could register as open,
// so the challenge could never be completed however hard the subject blinked.
// This is the test that would have caught it.
func TestABlinkIsMeasuredAgainstTheSubjectsOwnEyes(t *testing.T) {
	e := Evaluator{Thresholds: testThresholds()}

	// A subject whose eyes never come near the published "open" figure.
	t.Run("narrow eyes still complete a blink", func(t *testing.T) {
		s := sessionFor(t, ChallengeBlink)

		for _, ear := range []float64{0.26, 0.28, 0.27} { // open, well below 0.30
			if out := e.Evaluate(s, frame(ear, 0, 0, 0)); out.Satisfied {
				t.Fatalf("EAR %.2f satisfied the blink before the eyes ever shut", ear)
			}
		}
		for _, ear := range []float64{0.09, 0.06} { // shut
			e.Evaluate(s, frame(ear, 0, 0, 0))
		}
		if !s.Progress.SawClose {
			t.Fatalf("eyes at 0.06 against a peak of 0.28 did not register as shut")
		}

		if out := e.Evaluate(s, frame(0.27, 0, 0, 0)); !out.Satisfied {
			t.Errorf("reopening to 0.27 did not complete the blink: %+v", out)
		}
	})

	// The same movement on a subject whose eyes read wider, to show the
	// reference travels with the person rather than with the scale.
	t.Run("wide eyes need a proportionally larger drop", func(t *testing.T) {
		s := sessionFor(t, ChallengeBlink)

		e.Evaluate(s, frame(0.60, 0, 0, 0)) // open
		// 0.40 would be shut for the narrow-eyed subject above; here it is not
		// even below the open ratio.
		e.Evaluate(s, frame(0.55, 0, 0, 0))
		if s.Progress.ClosedFrames != 0 {
			t.Errorf("0.55 against a peak of 0.60 counted as shut")
		}

		for _, ear := range []float64{0.20, 0.15} {
			e.Evaluate(s, frame(ear, 0, 0, 0))
		}
		if out := e.Evaluate(s, frame(0.58, 0, 0, 0)); !out.Satisfied {
			t.Errorf("a proportionally equal blink did not complete: %+v", out)
		}
	})

	// A subject who arrives mid-blink must not anchor the whole challenge to
	// their shut eyes, which a first-frame baseline would do.
	t.Run("arriving with the eyes already shut", func(t *testing.T) {
		s := sessionFor(t, ChallengeBlink)

		e.Evaluate(s, frame(0.05, 0, 0, 0)) // shut on arrival
		e.Evaluate(s, frame(0.28, 0, 0, 0)) // opens; the reference moves up
		for _, ear := range []float64{0.08, 0.07} {
			e.Evaluate(s, frame(ear, 0, 0, 0))
		}

		if out := e.Evaluate(s, frame(0.27, 0, 0, 0)); !out.Satisfied {
			t.Errorf("a subject who arrived mid-blink could not complete one: %+v", out)
		}
	})
}

// A turn is a movement, not a pose: a subject already sitting at an angle must
// still turn, and one facing slightly the wrong way must not be refused for it.
func TestTurnIsMeasuredFromTheStartingPose(t *testing.T) {
	e := Evaluator{Thresholds: testThresholds()}

	tests := []struct {
		name      string
		challenge ChallengeKind
		frames    []biometric.Face
		wantIdx   int
	}{
		{
			name:      "turn right from centre",
			challenge: ChallengeTurnRight,
			frames: []biometric.Face{
				frame(0.3, 0, 0, 0), frame(0.3, 0, 10, 0), frame(0.3, 26, 0, 0),
				frame(0.3, 0, 27, 0),
			},
			wantIdx: 3,
		},
		{
			name:      "turn left from centre",
			challenge: ChallengeTurnLeft,
			frames: []biometric.Face{
				frame(0.3, 0, 0, 0), frame(0.3, 0, -10, 0), frame(0.3, 0, -30, 0),
			},
			wantIdx: 2,
		},
		{
			name:      "already sitting at an angle still has to turn",
			challenge: ChallengeTurnRight,
			frames: []biometric.Face{
				frame(0.3, 0, 20, 0), // baseline is 20, not 0
				frame(0.3, 0, 30, 0), // only 10 degrees of movement
				frame(0.3, 0, 46, 0), // 26 degrees, enough
			},
			wantIdx: 2,
		},
		{
			name:      "turning the wrong way never satisfies",
			challenge: ChallengeTurnRight,
			frames: []biometric.Face{
				frame(0.3, 0, 0, 0), frame(0.3, 0, -30, 0), frame(0.3, 0, -44, 0),
			},
			wantIdx: -1,
		},
		{
			name:      "no pose at all never satisfies",
			challenge: ChallengeTurnLeft,
			frames: []biometric.Face{
				frame(0.3, 0, 0, 0), frame(0.3, 0, 0, 0), frame(0.3, 0, 0, 0),
			},
			wantIdx: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := sessionFor(t, tt.challenge)
			if got := play(e, s, tt.frames); got != tt.wantIdx {
				t.Errorf("satisfied at frame %d, want %d", got, tt.wantIdx)
			}
		})
	}
}

func TestBaselineIsCapturedOnce(t *testing.T) {
	e := Evaluator{Thresholds: testThresholds()}
	s := sessionFor(t, ChallengeTurnRight)

	e.Evaluate(s, frame(0.3, 0, 12, 4))
	if !s.Progress.HaveBaseline {
		t.Fatal("the first frame did not set a baseline")
	}
	if s.Progress.BaselineYaw != 12 {
		t.Errorf("baseline yaw = %v, want 12", s.Progress.BaselineYaw)
	}

	e.Evaluate(s, frame(0.3, 0, 20, 9))
	if s.Progress.BaselineYaw != 12 {
		t.Errorf("baseline yaw moved to %v; it must be captured once", s.Progress.BaselineYaw)
	}
}

// Pitch is the noisiest axis of the pose estimate, so a nod is accepted in
// either direction rather than as a down-then-up sequence.
func TestNodAcceptsMovementInEitherDirection(t *testing.T) {
	e := Evaluator{Thresholds: testThresholds()}

	tests := []struct {
		name    string
		frames  []biometric.Face
		wantIdx int
	}{
		{"downwards", []biometric.Face{frame(0.3, 0, 0, 0), frame(0.3, 0, 0, 18)}, 1},
		{"upwards", []biometric.Face{frame(0.3, 0, 0, 0), frame(0.3, 0, 0, -20)}, 1},
		{"too small", []biometric.Face{frame(0.3, 0, 0, 0), frame(0.3, 0, 0, 8)}, -1},
		{"from an offset baseline", []biometric.Face{
			frame(0.3, 0, 0, 10), frame(0.3, 0, 0, 20), frame(0.3, 0, 0, 26),
		}, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := sessionFor(t, ChallengeNod)
			if got := play(e, s, tt.frames); got != tt.wantIdx {
				t.Errorf("satisfied at frame %d, want %d", got, tt.wantIdx)
			}
		})
	}
}

func TestMouthOpen(t *testing.T) {
	e := Evaluator{Thresholds: testThresholds()}

	tests := []struct {
		name    string
		mar     float64
		wantSat bool
	}{
		{"shut", 0.10, false},
		{"just below the threshold", 0.54, false},
		{"exactly at the threshold", 0.55, true},
		{"wide", 0.90, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := sessionFor(t, ChallengeMouthOpen)
			if got := e.Evaluate(s, frame(0.3, tt.mar, 0, 0)).Satisfied; got != tt.wantSat {
				t.Errorf("satisfied = %v, want %v", got, tt.wantSat)
			}
		})
	}
}

// An unsatisfied frame must say what is missing: the subject is being asked to
// do something and needs to know they have not yet.
func TestUnsatisfiedFramesExplainThemselves(t *testing.T) {
	e := Evaluator{Thresholds: testThresholds()}

	for _, kind := range AllChallenges {
		t.Run(string(kind), func(t *testing.T) {
			s := sessionFor(t, kind)

			out := e.Evaluate(s, frame(0.35, 0.1, 0, 0))
			if out.Satisfied {
				return // nothing to explain
			}
			if out.Reason == "" {
				t.Error("an unsatisfied frame gave no reason")
			}
		})
	}
}

func TestEvaluateHandlesAFinishedSession(t *testing.T) {
	e := Evaluator{Thresholds: testThresholds()}
	s := sessionFor(t, ChallengeBlink)
	s.Current = len(s.Challenges) // every challenge done

	if !e.Evaluate(s, frame(0.3, 0, 0, 0)).Satisfied {
		t.Error("a session with no active challenge reported unsatisfied")
	}
}

func TestEvaluateRejectsAnUnknownChallenge(t *testing.T) {
	e := Evaluator{Thresholds: testThresholds()}
	s := sessionFor(t, ChallengeBlink)
	s.Challenges = []ChallengeKind{"SMILE"}

	out := e.Evaluate(s, frame(0.3, 0, 0, 0))
	if out.Satisfied {
		t.Error("an unknown challenge was satisfied")
	}
	if !strings.Contains(out.Reason, "SMILE") {
		t.Errorf("reason %q does not name the unknown challenge", out.Reason)
	}
}
