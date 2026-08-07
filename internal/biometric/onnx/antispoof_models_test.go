//go:build models

package onnx

import (
	"context"
	"image"
	"image/color"
	"math"
	"testing"

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
