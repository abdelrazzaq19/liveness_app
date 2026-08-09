package onnx

import (
	"testing"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

// The embedder had no unit tests at all — only the models-tagged ones, which
// need a real .onnx and so never run in the ordinary gate. That is how the same
// unguarded shape check survived here after being written in the anti-spoofer.
func TestNewEmbedderRejectsWrongGraphShape(t *testing.T) {
	matchingInput := []ort.InputOutputInfo{
		{Dimensions: ort.NewShape(1, 3, arcFaceInputSize, arcFaceInputSize)},
	}

	t.Run("nil pool", func(t *testing.T) {
		if _, err := NewEmbedderArcFace(nil); err == nil {
			t.Error("NewEmbedderArcFace() accepted a nil pool, want an error")
		}
	})

	t.Run("wrong input size", func(t *testing.T) {
		p, _ := newFakePool(t, 1)
		p.all[0].Inputs = []ort.InputOutputInfo{{Dimensions: ort.NewShape(1, 3, 80, 80)}}
		p.all[0].Outputs = []ort.InputOutputInfo{{Dimensions: ort.NewShape(1, biometric.EmbeddingDim)}}

		if _, err := NewEmbedderArcFace(p); err == nil {
			t.Error("NewEmbedderArcFace() accepted an 80x80 input, want an error")
		}
	})

	t.Run("wrong embedding width", func(t *testing.T) {
		p, _ := newFakePool(t, 1)
		p.all[0].Inputs = matchingInput
		p.all[0].Outputs = []ort.InputOutputInfo{{Dimensions: ort.NewShape(1, 128)}}

		if _, err := NewEmbedderArcFace(p); err == nil {
			t.Error("NewEmbedderArcFace() accepted a 128-dimension output, want an error")
		}
	})

	// The same defect the anti-spoofer had: reading the last element of a shape
	// that has no elements. It panics rather than reporting the mismatch, and it
	// does so while the pools are being built at boot.
	t.Run("output with no dimensions", func(t *testing.T) {
		p, _ := newFakePool(t, 1)
		p.all[0].Inputs = matchingInput
		p.all[0].Outputs = []ort.InputOutputInfo{{Dimensions: ort.NewShape()}}

		if _, err := NewEmbedderArcFace(p); err == nil {
			t.Error("NewEmbedderArcFace() accepted an output with no dimensions, want an error")
		}
	})

	t.Run("matching graph", func(t *testing.T) {
		p, _ := newFakePool(t, 1)
		p.all[0].Inputs = matchingInput
		p.all[0].Outputs = []ort.InputOutputInfo{{Dimensions: ort.NewShape(1, biometric.EmbeddingDim)}}

		if _, err := NewEmbedderArcFace(p); err != nil {
			t.Errorf("NewEmbedderArcFace() rejected a matching graph: %v", err)
		}
	})
}
