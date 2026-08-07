package biometric

import (
	"errors"
	"math"
	"testing"
)

// projectPose renders the canonical face model at a known orientation, using
// the same weak-perspective model the estimator assumes.
//
// That makes the pair an exact round trip: any error the test finds is in the
// estimator, not in a mismatch between how the fixture and the code understand
// projection.
func projectPose(p Pose, scale float64, center Point) Landmarks106 {
	r := rotationMatrix(p)

	var l Landmarks106
	for _, c := range poseModel {
		x := c.model[0]
		y := c.model[1]
		z := c.model[2]

		camX := r.At(0, 0)*x + r.At(0, 1)*y + r.At(0, 2)*z
		camY := r.At(1, 0)*x + r.At(1, 1)*y + r.At(1, 2)*z

		l[c.landmark] = Point{
			X: center.X + scale*camX,
			Y: center.Y + scale*camY,
		}
	}
	return l
}

func assertPose(t *testing.T, got, want Pose, tolerance float64) {
	t.Helper()

	for _, c := range []struct {
		axis      string
		got, want float64
	}{
		{"yaw", got.Yaw, want.Yaw},
		{"pitch", got.Pitch, want.Pitch},
		{"roll", got.Roll, want.Roll},
	} {
		if math.Abs(c.got-c.want) > tolerance {
			t.Errorf("%s = %.4f deg, want %.4f deg (tolerance %.2f)", c.axis, c.got, c.want, tolerance)
		}
	}
}

// The projection model matches the estimator's exactly, so recovery should be
// near-perfect. A loose tolerance here would hide a real error and still pass.
const exactTolerance = 0.05

func TestEstimatePoseRecoversYaw(t *testing.T) {
	// The range the turn-your-head challenges use, plus margin.
	for yaw := -45.0; yaw <= 45.0; yaw += 5 {
		want := Pose{Yaw: yaw}
		got, err := projectPose(want, 0.4, Point{X: 320, Y: 240}).EstimatePose()
		if err != nil {
			t.Fatalf("yaw %.0f: EstimatePose() returned an unexpected error: %v", yaw, err)
		}
		assertPose(t, got, want, exactTolerance)
	}
}

func TestEstimatePoseRecoversPitch(t *testing.T) {
	for pitch := -30.0; pitch <= 30.0; pitch += 5 {
		want := Pose{Pitch: pitch}
		got, err := projectPose(want, 0.4, Point{X: 320, Y: 240}).EstimatePose()
		if err != nil {
			t.Fatalf("pitch %.0f: EstimatePose() returned an unexpected error: %v", pitch, err)
		}
		assertPose(t, got, want, exactTolerance)
	}
}

func TestEstimatePoseRecoversRoll(t *testing.T) {
	for roll := -40.0; roll <= 40.0; roll += 5 {
		want := Pose{Roll: roll}
		got, err := projectPose(want, 0.4, Point{X: 320, Y: 240}).EstimatePose()
		if err != nil {
			t.Fatalf("roll %.0f: EstimatePose() returned an unexpected error: %v", roll, err)
		}
		assertPose(t, got, want, exactTolerance)
	}
}

func TestEstimatePoseRecoversCombinedRotations(t *testing.T) {
	tests := []Pose{
		{},
		{Yaw: 30, Pitch: 10, Roll: -5},
		{Yaw: -25, Pitch: -15, Roll: 12},
		{Yaw: 40, Pitch: 20, Roll: 20},
		{Yaw: -40, Pitch: -20, Roll: -20},
	}

	for _, want := range tests {
		got, err := projectPose(want, 0.5, Point{X: 100, Y: 100}).EstimatePose()
		if err != nil {
			t.Fatalf("%+v: EstimatePose() returned an unexpected error: %v", want, err)
		}
		assertPose(t, got, want, exactTolerance)
	}
}

// Orientation must not depend on how close the subject is or where in the frame
// they are; only the challenge thresholds care about the angle itself.
func TestEstimatePoseIsInvariantToScaleAndPosition(t *testing.T) {
	want := Pose{Yaw: 28, Pitch: -12, Roll: 7}

	for _, tc := range []struct {
		name   string
		scale  float64
		center Point
	}{
		{"small and top left", 0.15, Point{X: 40, Y: 30}},
		{"medium and centred", 0.5, Point{X: 320, Y: 240}},
		{"large and bottom right", 1.8, Point{X: 1500, Y: 1100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := projectPose(want, tc.scale, tc.center).EstimatePose()
			if err != nil {
				t.Fatalf("EstimatePose() returned an unexpected error: %v", err)
			}
			assertPose(t, got, want, exactTolerance)
		})
	}
}

// The sign convention is documented on Pose, and every challenge depends on it.
// Getting it backwards would accept "turn left" for a turn to the right.
func TestPoseSignConventions(t *testing.T) {
	center := Point{X: 500, Y: 500}
	frontal := projectPose(Pose{}, 0.5, center)

	t.Run("positive yaw swings the nose towards image right", func(t *testing.T) {
		turned := projectPose(Pose{Yaw: 30}, 0.5, center)

		// With the head turned, the far side of the face compresses towards the
		// centre. Compare the eye-corner spans on each side.
		frontalLeft := frontal[LeftEyeFirst+eyeCornerLeft].X
		frontalRight := frontal[RightEyeFirst+eyeCornerRight].X
		turnedLeft := turned[LeftEyeFirst+eyeCornerLeft].X
		turnedRight := turned[RightEyeFirst+eyeCornerRight].X

		frontalMid := (frontalLeft + frontalRight) / 2
		turnedMid := (turnedLeft + turnedRight) / 2

		if turnedMid <= frontalMid {
			t.Errorf("positive yaw moved the face midpoint from %.1f to %.1f, want it to move right",
				frontalMid, turnedMid)
		}
	})

	t.Run("positive roll tilts the head so its top leans right", func(t *testing.T) {
		tilted := projectPose(Pose{Roll: 25}, 0.5, center)

		// Tilting the crown towards image right swings the whole face
		// clockwise on screen: the left eye rises and the right one drops,
		// the same way your right eye falls towards your right shoulder.
		leftDelta := tilted[LeftEyeFirst+eyeCornerLeft].Y - frontal[LeftEyeFirst+eyeCornerLeft].Y
		rightDelta := tilted[RightEyeFirst+eyeCornerRight].Y - frontal[RightEyeFirst+eyeCornerRight].Y

		if !(leftDelta < 0 && rightDelta > 0) {
			t.Errorf("positive roll moved the left eye by %.1f and the right by %.1f; want the left up and the right down",
				leftDelta, rightDelta)
		}

		// And the crown itself must move right.
		crownShift := tilted[ContourFirst].X - frontal[ContourFirst].X
		if crownShift >= 0 {
			t.Errorf("the chin moved %.1f px right under a positive roll; want it left as the crown goes right", crownShift)
		}
	})

	t.Run("positive pitch lifts the chin", func(t *testing.T) {
		lifted := projectPose(Pose{Pitch: 25}, 0.5, center)

		chinRise := frontal[ContourFirst].Y - lifted[ContourFirst].Y
		if chinRise <= 0 {
			t.Errorf("positive pitch moved the chin by %.1f px downwards, want it to rise", -chinRise)
		}
	})
}

// A pose the estimator cannot determine must be an error rather than a
// plausible-looking angle that a challenge would then act on.
func TestEstimatePoseRejectsDegenerateLandmarks(t *testing.T) {
	tests := []struct {
		name      string
		landmarks func() Landmarks106
	}{
		{
			name:      "all points at the origin",
			landmarks: func() Landmarks106 { return Landmarks106{} },
		},
		{
			name: "all model points coincident",
			landmarks: func() Landmarks106 {
				var l Landmarks106
				for _, c := range poseModel {
					l[c.landmark] = Point{X: 200, Y: 200}
				}
				return l
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.landmarks().EstimatePose()
			if !errors.Is(err, ErrPoseUnavailable) {
				t.Errorf("EstimatePose() error = %v, want ErrPoseUnavailable", err)
			}
		})
	}
}

// The estimator must degrade gracefully rather than collapse when the landmarks
// carry the jitter a real detector produces.
func TestEstimatePoseToleratesLandmarkNoise(t *testing.T) {
	want := Pose{Yaw: 25, Pitch: -8, Roll: 4}
	l := projectPose(want, 0.6, Point{X: 400, Y: 400})

	// A deterministic perturbation of a couple of pixels on each point, in
	// different directions, standing in for detector jitter.
	offsets := []Point{{X: 2, Y: -1}, {X: -2, Y: 2}, {X: 1, Y: 2}, {X: -1, Y: -2}, {X: 2, Y: 1}, {X: -2, Y: -1}, {X: 0, Y: 2}}
	for i, c := range poseModel {
		p := l[c.landmark]
		p.X += offsets[i%len(offsets)].X
		p.Y += offsets[i%len(offsets)].Y
		l[c.landmark] = p
	}

	got, err := l.EstimatePose()
	if err != nil {
		t.Fatalf("EstimatePose() returned an unexpected error: %v", err)
	}

	// Pitch tolerates less than the other two axes, and that is inherent
	// rather than a shortcoming to tune away. Yaw and roll are read from the
	// face's width and tilt, which span hundreds of pixels; pitch is read from
	// how the depth differences foreshorten, and those are tens of millimetres
	// on a face that is nearly flat from the camera's point of view. The same
	// landmark jitter therefore buys several times more error in pitch.
	for _, c := range []struct {
		axis      string
		got, want float64
		tolerance float64
	}{
		{"yaw", got.Yaw, want.Yaw, 3},
		{"roll", got.Roll, want.Roll, 3},
		{"pitch", got.Pitch, want.Pitch, 8},
	} {
		if math.Abs(c.got-c.want) > c.tolerance {
			t.Errorf("with 2 px of landmark noise, %s = %.2f deg, want %.2f +/- %.0f",
				c.axis, c.got, c.want, c.tolerance)
		}
	}
}

// The composition and the decomposition must be exact inverses, since every
// synthetic test above rests on that.
func TestRotationMatrixAndEulerRoundTrip(t *testing.T) {
	poses := []Pose{
		{},
		{Yaw: 15},
		{Pitch: 15},
		{Roll: 15},
		{Yaw: -33, Pitch: 21, Roll: -9},
		{Yaw: 44, Pitch: -29, Roll: 38},
	}

	for _, want := range poses {
		got := eulerFromRotation(rotationMatrix(want))
		assertPose(t, got, want, 1e-9)
	}
}
