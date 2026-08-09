//go:build models

package onnx

import (
	"context"
	"math"
	"testing"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

const landmarkerModel = "2d106det.onnx"

func newLandmarker(t *testing.T) *Landmarker2d106 {
	t.Helper()

	rt, path := newRealRuntime(t, landmarkerModel)

	pool, err := rt.LoadModel(ModelSpec{Name: "landmarker", Path: path, PoolSize: 1})
	if err != nil {
		t.Fatalf("LoadModel() returned an unexpected error: %v", err)
	}

	l, err := NewLandmarker2d106(pool)
	if err != nil {
		t.Fatalf("NewLandmarker2d106() returned an unexpected error: %v", err)
	}
	return l
}

func TestLandmarkerGraphShape(t *testing.T) {
	rt, path := newRealRuntime(t, landmarkerModel)

	pool, err := rt.LoadModel(ModelSpec{Name: "landmarker", Path: path, PoolSize: 1})
	if err != nil {
		t.Fatalf("LoadModel() returned an unexpected error: %v", err)
	}

	inputs, outputs := pool.Signature()
	if len(inputs) != 1 || len(outputs) != 1 {
		t.Fatalf("graph has %d inputs and %d outputs, want 1 and 1", len(inputs), len(outputs))
	}

	t.Logf("input  %q %v", inputs[0].Name, inputs[0].Dimensions)
	t.Logf("output %q %v", outputs[0].Name, outputs[0].Dimensions)

	// The crop size is fixed by the graph, so a model that wants something
	// other than 192x192 would silently receive the wrong pixels.
	if dims := inputs[0].Dimensions; len(dims) != 4 || dims[2] != landmarkInputSize || dims[3] != landmarkInputSize {
		t.Errorf("input shape %v, want [_ 3 %d %d]", dims, landmarkInputSize, landmarkInputSize)
	}
	if dims := outputs[0].Dimensions; dims[len(dims)-1] != landmarkOutputValues {
		t.Errorf("output shape %v, want %d values", dims, landmarkOutputValues)
	}
}

// The acceptance criterion this task exists for: coordinates must come back in
// the source image's space, not the 192x192 crop's.
//
// The check works without a real face because a landmark network regresses
// towards a mean face on any input, so the points still land where the crop
// says a face would be.
func TestLandmarksAreInSourceImageCoordinates(t *testing.T) {
	l := newLandmarker(t)

	// A box deliberately far from the origin: landmarks left in crop
	// coordinates would all sit inside [0,192] and fail every assertion below.
	box := biometric.BBox{MinX: 300, MinY: 400, MaxX: 420, MaxY: 560}
	img := syntheticScene(800, 1000)

	pts, err := l.Landmarks(context.Background(), img, box)
	if err != nil {
		t.Fatalf("Landmarks() returned an unexpected error: %v", err)
	}

	bounds := pts.Bounds()
	t.Logf("box    %+v", box)
	t.Logf("bounds %+v", bounds)

	if bounds.MaxX <= float64(landmarkInputSize) && bounds.MaxY <= float64(landmarkInputSize) {
		t.Fatalf("every landmark falls within the crop size; they were not mapped back to the image: %+v", bounds)
	}

	// The crop is centred on the box, so the landmarks should be too.
	boxCenter := box.Center()
	lmkCenter := bounds.Center()
	tolerance := math.Max(box.Width(), box.Height())

	if math.Abs(lmkCenter.X-boxCenter.X) > tolerance || math.Abs(lmkCenter.Y-boxCenter.Y) > tolerance {
		t.Errorf("landmark centre %+v is more than %.0f px from the box centre %+v",
			lmkCenter, tolerance, boxCenter)
	}

	// The crop is the longest box side widened by the margin, so the landmarks
	// cannot legitimately spread wider than that.
	maxSpread := math.Max(box.Width(), box.Height()) * landmarkCropMargin
	if bounds.Width() > maxSpread || bounds.Height() > maxSpread {
		t.Errorf("landmarks span %.0fx%.0f, wider than the %.0f px crop", bounds.Width(), bounds.Height(), maxSpread)
	}
}

// Structural checks on the index map. A mean face is still a face: the eyes sit
// above the mouth, and the left eye sits left of the right one.
func TestLandmarkIndexMapMatchesFaceGeometry(t *testing.T) {
	l := newLandmarker(t)

	box := biometric.BBox{MinX: 200, MinY: 200, MaxX: 400, MaxY: 460}
	pts, err := l.Landmarks(context.Background(), syntheticScene(640, 800), box)
	if err != nil {
		t.Fatalf("Landmarks() returned an unexpected error: %v", err)
	}

	leftEye := pts.EyeCenter(biometric.LeftEye)
	rightEye := pts.EyeCenter(biometric.RightEye)
	mouth := pts.MouthCenter()

	t.Logf("left eye  %+v", leftEye)
	t.Logf("right eye %+v", rightEye)
	t.Logf("mouth     %+v", mouth)
	t.Logf("EAR left %.3f  right %.3f  mean %.3f",
		pts.EyeAspectRatio(biometric.LeftEye),
		pts.EyeAspectRatio(biometric.RightEye),
		pts.MeanEyeAspectRatio())
	t.Logf("MAR %.3f", pts.MouthAspectRatio())

	if leftEye.X >= rightEye.X {
		t.Errorf("left eye at x=%.1f is not left of the right eye at x=%.1f", leftEye.X, rightEye.X)
	}
	if mouth.Y <= leftEye.Y || mouth.Y <= rightEye.Y {
		t.Errorf("mouth at y=%.1f is not below the eyes at y=%.1f and %.1f", mouth.Y, leftEye.Y, rightEye.Y)
	}

	// The two eyes should be roughly level; a large difference means the blocks
	// are not the two eyes at all.
	eyeGap := math.Abs(leftEye.Y - rightEye.Y)
	eyeSpan := math.Abs(rightEye.X - leftEye.X)
	if eyeSpan <= 0 {
		t.Fatal("the eye centres coincide")
	}
	if eyeGap > eyeSpan {
		t.Errorf("eye centres differ by %.1f px vertically but only %.1f px horizontally", eyeGap, eyeSpan)
	}

	// Ratios must be finite and in a sane range, whatever the input.
	for _, r := range []struct {
		name  string
		value float64
	}{
		{"mean EAR", pts.MeanEyeAspectRatio()},
		{"MAR", pts.MouthAspectRatio()},
	} {
		if math.IsNaN(r.value) || math.IsInf(r.value, 0) {
			t.Errorf("%s is %v", r.name, r.value)
		}
		if r.value < 0 || r.value > 3 {
			t.Errorf("%s = %.3f, outside any plausible range", r.name, r.value)
		}
	}
}

func TestLandmarkerRejectsBadArguments(t *testing.T) {
	l := newLandmarker(t)
	img := syntheticScene(320, 320)

	boxes := []struct {
		name string
		box  biometric.BBox
	}{
		{"empty box", biometric.BBox{}},
		{"inverted box", biometric.BBox{MinX: 100, MinY: 100, MaxX: 50, MaxY: 50}},
		{"zero height", biometric.BBox{MinX: 10, MinY: 10, MaxX: 100, MaxY: 10}},
	}

	for _, tt := range boxes {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := l.Landmarks(context.Background(), img, tt.box); err == nil {
				t.Error("Landmarks() accepted a degenerate box, want an error")
			}
		})
	}

	t.Run("nil image", func(t *testing.T) {
		box := biometric.BBox{MinX: 10, MinY: 10, MaxX: 100, MaxY: 100}
		if _, err := l.Landmarks(context.Background(), nil, box); err == nil {
			t.Error("Landmarks() accepted a nil image, want an error")
		}
	})
}

// NewLandmarker2d106 must refuse a graph that is not the landmarker, so that
// pointing it at the detector fails at wiring time.
func TestNewLandmarkerRejectsWrongGraph(t *testing.T) {
	rt, path := newRealRuntime(t, detectorModel)

	pool, err := rt.LoadModel(ModelSpec{Name: "detector", Path: path, PoolSize: 1})
	if err != nil {
		t.Fatalf("LoadModel() returned an unexpected error: %v", err)
	}

	if _, err := NewLandmarker2d106(pool); err == nil {
		t.Error("NewLandmarker2d106() accepted the detector graph, want an error")
	}
}

// TestNoseBlockGeometry finds the nose tip by asking the model rather than by
// trusting a remembered index.
//
// The pose solver needs it. Without it the seven correspondences it uses sit
// within 10 mm of one depth — a nearly planar set, which makes a weak
// perspective solve ill-conditioned: on a real session pitch read 40 to 72
// degrees for a subject facing a laptop, and yaw never exceeded 22 even when
// the subject plainly turned. The nose is the one landmark that carries real
// depth, so which index it is decides whether the estimate is conditioned at
// all.
//
// On an input with no face the regression collapses to its mean shape, and that
// canonical face is exactly what is wanted here.
func TestNoseBlockGeometry(t *testing.T) {
	l := newLandmarker(t)

	box := biometric.BBox{MinX: 200, MinY: 200, MaxX: 400, MaxY: 460}
	pts, err := l.Landmarks(context.Background(), syntheticScene(640, 800), box)
	if err != nil {
		t.Fatalf("Landmarks() returned an unexpected error: %v", err)
	}

	eyes := biometric.Point{
		X: (pts.EyeCenter(biometric.LeftEye).X + pts.EyeCenter(biometric.RightEye).X) / 2,
		Y: (pts.EyeCenter(biometric.LeftEye).Y + pts.EyeCenter(biometric.RightEye).Y) / 2,
	}
	mouth := pts.MouthCenter()
	span := mouth.Y - eyes.Y

	t.Logf("eye midpoint %+v   mouth %+v   eye-to-mouth %.1f px", eyes, mouth, span)
	t.Logf("index   x        y      dx from centre   depth-fraction eyes->mouth")

	// The tip is the nose point nearest the vertical midline and lowest down
	// the bridge before the base flares out.
	best, bestScore := -1, math.Inf(1)
	for i := biometric.NoseFirst; i <= biometric.NoseLast; i++ {
		p := pts[i]
		dx := math.Abs(p.X - eyes.X)
		frac := (p.Y - eyes.Y) / span
		t.Logf("%5d %7.1f %7.1f %12.1f %18.2f", i, p.X, p.Y, dx, frac)

		// The tip is the midline point that continues the bridge downwards. Its
		// neighbours below are the nostril base, which flanks the centre rather
		// than sitting on it, and the wings are wider still.
		if frac > 0.45 && dx < bestScore {
			best, bestScore = i, dx
		}
	}

	t.Logf("nose tip: index %d (%.1f px off the midline)", best, bestScore)

	if best != biometric.NoseFirst+biometric.NoseTip {
		t.Errorf("the mean face puts the nose tip at index %d, but NoseTip names %d",
			best, biometric.NoseFirst+biometric.NoseTip)
	}
}

// TestContourBlockGeometry confirms that index 0 really is the chin.
//
// It was written expecting the opposite. The block is documented as running ear
// to chin to ear, which reads as though index 0 must be beside the jaw — and if
// so, the pose model telling the solver it sits at the chin.s 3D position would
// be a gross mis-correspondence.
//
// The mean face says otherwise: index 0 is the lowest point of the contour and
// sits near the midline. The block starts at the chin and runs outwards. The
// correspondence was right, and this test now guards it against being
// "corrected" by the next person to read that comment.
func TestContourBlockGeometry(t *testing.T) {
	l := newLandmarker(t)

	box := biometric.BBox{MinX: 200, MinY: 200, MaxX: 400, MaxY: 460}
	pts, err := l.Landmarks(context.Background(), syntheticScene(640, 800), box)
	if err != nil {
		t.Fatalf("Landmarks() returned an unexpected error: %v", err)
	}

	centre := (pts.EyeCenter(biometric.LeftEye).X + pts.EyeCenter(biometric.RightEye).X) / 2

	// The chin is the lowest point of the contour; on a frontal face it is also
	// the one nearest the midline.
	lowest, lowestY := -1, math.Inf(-1)
	for i := biometric.ContourFirst; i <= biometric.ContourLast; i++ {
		if pts[i].Y > lowestY {
			lowest, lowestY = i, pts[i].Y
		}
	}

	first := pts[biometric.ContourFirst]
	chin := pts[lowest]

	t.Logf("midline x           %.1f", centre)
	t.Logf("ContourFirst (%2d)   x %.1f  y %.1f   %.1f px off the midline",
		biometric.ContourFirst, first.X, first.Y, math.Abs(first.X-centre))
	t.Logf("lowest point (%2d)   x %.1f  y %.1f   %.1f px off the midline",
		lowest, chin.X, chin.Y, math.Abs(chin.X-centre))
	t.Logf("ContourFirst sits %.1f px ABOVE the lowest point", chin.Y-first.Y)

	if lowest != biometric.ContourFirst {
		t.Errorf("the lowest contour point is index %d, not ContourFirst (%d); "+
			"the pose model maps ContourFirst to the chin", lowest, biometric.ContourFirst)
	}

	// Near the midline, or it is a jaw point rather than the chin.
	if off := math.Abs(chin.X - centre); off > math.Abs(pts[biometric.ContourLast].X-centre)/2 {
		t.Errorf("the chin sits %.1f px off the midline; that is a jaw point, not a chin", off)
	}
}
