//go:build models

package onnx

import (
	"context"
	"image"
	"image/color"
	"math"
	"testing"

	xdraw "golang.org/x/image/draw"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

const antiSpoofModel = "minifasnet_v2.onnx"

func newAntiSpoofer(t *testing.T) *AntiSpoofMiniFASNet {
	t.Helper()

	rt, path := newRealRuntime(t, antiSpoofModel)

	pool, err := rt.LoadModel(ModelSpec{Name: "antispoof", Path: path, PoolSize: 1})
	if err != nil {
		t.Fatalf("LoadModel() returned an unexpected error: %v", err)
	}

	a, err := NewAntiSpoofMiniFASNet(pool)
	if err != nil {
		t.Fatalf("NewAntiSpoofMiniFASNet() returned an unexpected error: %v", err)
	}
	return a
}

// The converted graph must have the shape the Go side assumes. A conversion
// that produced something else would otherwise be discovered as a wrong score
// rather than as an error.
func TestAntiSpoofGraphShape(t *testing.T) {
	rt, path := newRealRuntime(t, antiSpoofModel)

	pool, err := rt.LoadModel(ModelSpec{Name: "antispoof", Path: path, PoolSize: 1})
	if err != nil {
		t.Fatalf("LoadModel() returned an unexpected error: %v", err)
	}

	inputs, outputs := pool.Signature()
	t.Logf("input  %q %v", inputs[0].Name, inputs[0].Dimensions)
	t.Logf("output %q %v", outputs[0].Name, outputs[0].Dimensions)

	if dims := inputs[0].Dimensions; len(dims) != 4 ||
		dims[1] != 3 || dims[2] != antiSpoofInputSize || dims[3] != antiSpoofInputSize {
		t.Errorf("input shape %v, want [_ 3 %d %d]", dims, antiSpoofInputSize, antiSpoofInputSize)
	}
	if dims := outputs[0].Dimensions; dims[len(dims)-1] != antiSpoofClasses {
		t.Errorf("output shape %v, want %d classes", dims, antiSpoofClasses)
	}
}

// Whatever the input, the score must be a usable probability. A NaN or an
// out-of-range value would compare false against every threshold and silently
// pass every frame.
func TestLivenessScoreIsAlwaysAProbability(t *testing.T) {
	a := newAntiSpoofer(t)

	tests := []struct {
		name string
		img  image.Image
		box  biometric.BBox
	}{
		{
			name: "synthetic face",
			img:  syntheticScene(480, 640),
			box:  biometric.BBox{MinX: 180, MinY: 250, MaxX: 300, MaxY: 400},
		},
		{
			name: "flat grey",
			img:  flatImage(320, 320, color.RGBA{R: 128, G: 128, B: 128, A: 255}),
			box:  biometric.BBox{MinX: 100, MinY: 100, MaxX: 200, MaxY: 220},
		},
		{
			name: "pure black",
			img:  flatImage(320, 320, color.RGBA{A: 255}),
			box:  biometric.BBox{MinX: 100, MinY: 100, MaxX: 200, MaxY: 220},
		},
		{
			name: "face at the frame edge",
			img:  syntheticScene(480, 640),
			box:  biometric.BBox{MinX: 0, MinY: 0, MaxX: 90, MaxY: 120},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score, err := a.LivenessScore(context.Background(), tt.img, tt.box)
			if err != nil {
				t.Fatalf("LivenessScore() returned an unexpected error: %v", err)
			}
			if math.IsNaN(score) || math.IsInf(score, 0) {
				t.Fatalf("score = %v", score)
			}
			if score < 0 || score > 1 {
				t.Errorf("score = %.6f, outside [0,1]", score)
			}
			t.Logf("score %.4f", score)
		})
	}
}

// The same frame must always score the same. A liveness decision that varied
// between two runs on identical input would be untestable and unarguable.
func TestLivenessScoreIsDeterministic(t *testing.T) {
	a := newAntiSpoofer(t)

	img := syntheticScene(480, 640)
	box := biometric.BBox{MinX: 180, MinY: 250, MaxX: 300, MaxY: 400}

	first, err := a.LivenessScore(context.Background(), img, box)
	if err != nil {
		t.Fatalf("LivenessScore() returned an unexpected error: %v", err)
	}

	for i := 0; i < 4; i++ {
		got, err := a.LivenessScore(context.Background(), img, box)
		if err != nil {
			t.Fatalf("run %d: LivenessScore() returned an unexpected error: %v", i, err)
		}
		if got != first {
			t.Errorf("run %d scored %.9f, first run scored %.9f", i, got, first)
		}
	}
}

func TestLivenessScoreRejectsBadArguments(t *testing.T) {
	a := newAntiSpoofer(t)
	img := syntheticScene(320, 320)

	t.Run("nil image", func(t *testing.T) {
		box := biometric.BBox{MinX: 10, MinY: 10, MaxX: 100, MaxY: 100}
		if _, err := a.LivenessScore(context.Background(), nil, box); err == nil {
			t.Error("LivenessScore() accepted a nil image, want an error")
		}
	})

	for _, tt := range []struct {
		name string
		box  biometric.BBox
	}{
		{"empty box", biometric.BBox{}},
		{"inverted box", biometric.BBox{MinX: 100, MinY: 100, MaxX: 50, MaxY: 50}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := a.LivenessScore(context.Background(), img, tt.box); err == nil {
				t.Error("LivenessScore() accepted a degenerate box, want an error")
			}
		})
	}
}

func TestNewAntiSpoofRejectsTheDetectorGraph(t *testing.T) {
	rt, path := newRealRuntime(t, detectorModel)

	pool, err := rt.LoadModel(ModelSpec{Name: "detector", Path: path, PoolSize: 1})
	if err != nil {
		t.Fatalf("LoadModel() returned an unexpected error: %v", err)
	}

	if _, err := NewAntiSpoofMiniFASNet(pool); err == nil {
		t.Error("NewAntiSpoofMiniFASNet() accepted the detector graph, want an error")
	}
}

func flatImage(w, h int, c color.RGBA) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// TestAntiSpoofClassDistribution prints the full three-class output, because a
// real session measured 0.006 for the live class on every frame of every
// attempt — stable to four decimal places across different poses and lighting.
//
// A threshold set too high produces something like 0.7. A number pinned at
// 0.006 regardless of the input means the score is not a judgement about the
// pixels at all, and the only way to tell which of the three assumptions in
// this file is wrong is to look at where the probability actually went.
func TestAntiSpoofClassDistribution(t *testing.T) {
	rt, path := newRealRuntime(t, antiSpoofModel)

	pool, err := rt.LoadModel(ModelSpec{Name: "antispoof", Path: path, PoolSize: 1})
	if err != nil {
		t.Fatalf("LoadModel() returned an unexpected error: %v", err)
	}

	inputs, outputs := pool.Signature()
	t.Logf("input  %q %v", inputs[0].Name, inputs[0].Dimensions)
	t.Logf("output %q %v", outputs[0].Name, outputs[0].Dimensions)

	frames := []struct {
		name string
		img  image.Image
	}{
		{"textured scene", syntheticScene(480, 640)},
		{"mid grey", flatImage(480, 640, color.RGBA{R: 128, G: 128, B: 128, A: 255})},
		{"skin-ish", flatImage(480, 640, color.RGBA{R: 222, G: 180, B: 150, A: 255})},
		{"black", flatImage(480, 640, color.RGBA{A: 255})},
		{"white", flatImage(480, 640, color.RGBA{R: 255, G: 255, B: 255, A: 255})},
	}

	box := biometric.BBox{MinX: 180, MinY: 240, MaxX: 300, MaxY: 400}

	for _, f := range frames {
		patch := image.NewRGBA(image.Rect(0, 0, antiSpoofInputSize, antiSpoofInputSize))
		crop := antiSpoofCrop(box, 480, 640, antiSpoofCropScale)
		xdraw.BiLinear.Scale(patch, patch.Bounds(), f.img, crop, xdraw.Src, nil)

		var probs []float64
		err := pool.Use(context.Background(), func(s *Session) error {
			in, err := ort.NewTensor(ort.NewShape(1, 3, antiSpoofInputSize, antiSpoofInputSize),
				antiSpoofPlanes(patch))
			if err != nil {
				return err
			}
			defer func() { _ = in.Destroy() }()

			outs := make([]ort.Value, 1)
			if err := s.Run([]ort.Value{in}, outs); err != nil {
				return err
			}
			defer destroyValues(outs)

			raw, err := floatData(outs[0], "logits", 0)
			if err != nil {
				return err
			}
			t.Logf("%-15s raw logits % .4f", f.name, raw[:antiSpoofClasses])
			probs = softmax(raw[:antiSpoofClasses])
			return nil
		})
		if err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}

		t.Logf("%-15s p0=%.4f  p1=%.4f (taken as live)  p2=%.4f",
			f.name, probs[0], probs[1], probs[2])
	}
}
