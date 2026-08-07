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
