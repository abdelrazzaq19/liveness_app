package biometric

import (
	"errors"
	"fmt"
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

// The correspondence set must not be nearly planar.
//
// This is the invariant that was actually broken, and it broke silently: the
// solve still returned an answer, it was just wrong. Six of the seven points
// then sat within 10 mm of one depth, and a weak perspective solve from a
// nearly planar set is ill conditioned — pitch read 40 to 72 degrees for a
// subject square to a laptop, and yaw never passed 22 when they turned.
//
// Checking the spread rather than the presence of any one landmark, because
// what matters is the geometry, not which points happen to supply it.
func TestPoseModelIsNotNearlyPlanar(t *testing.T) {
	if len(poseModel) < 4 {
		t.Fatalf("the pose model has %d points; a solve needs at least 4", len(poseModel))
	}

	minZ, maxZ := math.Inf(1), math.Inf(-1)
	minX, maxX := math.Inf(1), math.Inf(-1)
	minY, maxY := math.Inf(1), math.Inf(-1)
	for _, c := range poseModel {
		minX, maxX = math.Min(minX, c.model[0]), math.Max(maxX, c.model[0])
		minY, maxY = math.Min(minY, c.model[1]), math.Max(maxY, c.model[1])
		minZ, maxZ = math.Min(minZ, c.model[2]), math.Max(maxZ, c.model[2])
	}

	depth := maxZ - minZ
	lateral := math.Max(maxX-minX, maxY-minY)
	t.Logf("model extent: x %.0f  y %.0f  depth %.0f  ratio %.3f",
		maxX-minX, maxY-minY, depth, depth/lateral)

	// A tenth of the lateral extent is already generous; the classic model this
	// derives from sits at 0.27.
	if ratio := depth / lateral; ratio < 0.15 {
		t.Errorf("depth is %.0f against a lateral extent of %.0f (ratio %.3f): "+
			"the points are nearly planar and the solve is ill conditioned", depth, lateral, ratio)
	}

	// And the depth must not come from a single outlier: drop any one point and
	// the set must still span.
	for i := range poseModel {
		lo, hi := math.Inf(1), math.Inf(-1)
		for j, c := range poseModel {
			if i == j {
				continue
			}
			lo, hi = math.Min(lo, c.model[2]), math.Max(hi, c.model[2])
		}
		if hi-lo < depth*0.4 {
			t.Errorf("dropping point %d collapses the depth from %.0f to %.0f; "+
				"the conditioning rests on one landmark", i, depth, hi-lo)
		}
	}
}

// A real rotation must survive the round trip through the projection.
//
// The previous model passed every algebraic test in this file — the Euler
// decomposition and its inverse agreed perfectly — while still producing
// nonsense on real faces, because those tests never projected anything. This
// one does: it takes the model points, rotates them, projects them the way a
// camera would, and asks whether the solver recovers the angle it started from.
func TestPoseSurvivesProjectionRoundTrip(t *testing.T) {
	for _, want := range []Pose{
		{Yaw: 0, Pitch: 0, Roll: 0},
		{Yaw: 20, Pitch: 0, Roll: 0},
		{Yaw: -25, Pitch: 0, Roll: 0},
		{Yaw: 0, Pitch: 15, Roll: 0},
		{Yaw: 0, Pitch: -12, Roll: 0},
		{Yaw: 18, Pitch: -10, Roll: 6},
	} {
		t.Run(fmt.Sprintf("yaw %.0f pitch %.0f roll %.0f", want.Yaw, want.Pitch, want.Roll), func(t *testing.T) {
			var pts Landmarks106
			r := rotationMatrix(want)

			// Scaled orthographic projection, which is the model the solver
			// assumes: rotate, drop z, scale, and place in the frame.
			const scale = 0.5
			for _, c := range poseModel {
				x := r.At(0, 0)*c.model[0] + r.At(0, 1)*c.model[1] + r.At(0, 2)*c.model[2]
				y := r.At(1, 0)*c.model[0] + r.At(1, 1)*c.model[1] + r.At(1, 2)*c.model[2]
				pts[c.landmark] = Point{X: 320 + scale*x, Y: 400 + scale*y}
			}

			got, err := pts.EstimatePose()
			if err != nil {
				t.Fatalf("EstimatePose() returned an unexpected error: %v", err)
			}

			for _, axis := range []struct {
				name       string
				got, want_ float64
			}{
				{"yaw", got.Yaw, want.Yaw},
				{"pitch", got.Pitch, want.Pitch},
				{"roll", got.Roll, want.Roll},
			} {
				if math.Abs(axis.got-axis.want_) > 1.0 {
					t.Errorf("%s = %.2f, want %.2f", axis.name, axis.got, axis.want_)
				}
			}
		})
	}
}

// Conditioning, which is what actually broke.
//
// TestPoseSurvivesProjectionRoundTrip passes even with a nearly planar model,
// because a noise-free projection solves exactly however badly conditioned it
// is. That is precisely why the missing nose tip survived every algebraic test
// in this file while producing 40 to 72 degrees of pitch on real faces.
//
// Real landmarks are not noise-free. A regression that places each point within
// a pixel or two of the truth is a good one, and the estimate has to stay
// usable under that.
func TestPoseIsStableUnderLandmarkNoise(t *testing.T) {
	// Deterministic jitter: a fixed pattern rather than a random one, so a
	// failure is reproducible and not a flake somebody reruns until it passes.
	jitter := []Point{
		{X: 1.5, Y: -1.0},
		{X: -1.2, Y: 1.4},
		{X: 0.8, Y: 1.1},
		{X: -1.6, Y: -0.7},
		{X: 1.1, Y: 1.6},
		{X: -0.9, Y: -1.3},
		{X: 1.4, Y: 0.6},
		{X: -1.3, Y: 1.2},
	}

	for _, want := range []Pose{
		{Yaw: 0, Pitch: 0, Roll: 0},
		{Yaw: 20, Pitch: -5, Roll: 0},
		{Yaw: -20, Pitch: 5, Roll: 3},
	} {
		t.Run(fmt.Sprintf("yaw %.0f pitch %.0f", want.Yaw, want.Pitch), func(t *testing.T) {
			var pts Landmarks106
			r := rotationMatrix(want)

			const scale = 0.5
			for i, c := range poseModel {
				x := r.At(0, 0)*c.model[0] + r.At(0, 1)*c.model[1] + r.At(0, 2)*c.model[2]
				y := r.At(1, 0)*c.model[0] + r.At(1, 1)*c.model[1] + r.At(1, 2)*c.model[2]

				j := jitter[i%len(jitter)]
				pts[c.landmark] = Point{X: 320 + scale*x + j.X, Y: 400 + scale*y + j.Y}
			}

			got, err := pts.EstimatePose()
			if err != nil {
				t.Fatalf("EstimatePose() returned an unexpected error: %v", err)
			}
			t.Logf("want %+v  got yaw %.1f pitch %.1f roll %.1f", want, got.Yaw, got.Pitch, got.Roll)

			// A couple of pixels of landmark noise must not move the answer by
			// more than a few degrees. The broken model moved it by tens.
			const tolerance = 8.0
			if math.Abs(got.Yaw-want.Yaw) > tolerance {
				t.Errorf("yaw = %.1f, want %.1f within %.0f degrees", got.Yaw, want.Yaw, tolerance)
			}
			if math.Abs(got.Pitch-want.Pitch) > tolerance {
				t.Errorf("pitch = %.1f, want %.1f within %.0f degrees", got.Pitch, want.Pitch, tolerance)
			}
		})
	}
}
