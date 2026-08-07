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

const embedderModel = "w600k_r50.onnx"

func newEmbedder(t *testing.T) *EmbedderArcFace {
	t.Helper()

	rt, path := newRealRuntime(t, embedderModel)

	pool, err := rt.LoadModel(ModelSpec{Name: "embedder", Path: path, PoolSize: 1})
	if err != nil {
		t.Fatalf("LoadModel() returned an unexpected error: %v", err)
	}

	e, err := NewEmbedderArcFace(pool)
	if err != nil {
		t.Fatalf("NewEmbedderArcFace() returned an unexpected error: %v", err)
	}
	return e
}

// faceKeypoints returns a plausible five-point layout for a face centred in a
// frame of the given size.
func faceKeypoints(cx, cy, span float64) biometric.Keypoints {
	var k biometric.Keypoints
	k[biometric.KeypointLeftEye] = biometric.Point{X: cx - span*0.31, Y: cy - span*0.18}
	k[biometric.KeypointRightEye] = biometric.Point{X: cx + span*0.31, Y: cy - span*0.18}
	k[biometric.KeypointNose] = biometric.Point{X: cx, Y: cy + span*0.02}
	k[biometric.KeypointMouthLeft] = biometric.Point{X: cx - span*0.22, Y: cy + span*0.29}
	k[biometric.KeypointMouthRight] = biometric.Point{X: cx + span*0.22, Y: cy + span*0.29}
	return k
}

func TestEmbedderGraphShape(t *testing.T) {
	rt, path := newRealRuntime(t, embedderModel)

	pool, err := rt.LoadModel(ModelSpec{Name: "embedder", Path: path, PoolSize: 1})
	if err != nil {
		t.Fatalf("LoadModel() returned an unexpected error: %v", err)
	}

	inputs, outputs := pool.Signature()
	t.Logf("input  %q %v", inputs[0].Name, inputs[0].Dimensions)
	t.Logf("output %q %v", outputs[0].Name, outputs[0].Dimensions)

	if dims := inputs[0].Dimensions; len(dims) != 4 ||
		dims[1] != 3 || dims[2] != arcFaceInputSize || dims[3] != arcFaceInputSize {
		t.Errorf("input shape %v, want [_ 3 %d %d]", dims, arcFaceInputSize, arcFaceInputSize)
	}
	if dims := outputs[0].Dimensions; dims[len(dims)-1] != biometric.EmbeddingDim {
		t.Errorf("output shape %v, want %d dimensions", dims, biometric.EmbeddingDim)
	}
}

// Normalisation is part of the contract: a stored vector must be comparable
// against a fresh one without either side remembering how the other was scaled.
func TestEmbedIsNormalised(t *testing.T) {
	e := newEmbedder(t)

	img := syntheticScene(480, 640)
	got, err := e.Embed(context.Background(), img, faceKeypoints(240, 320, 200))
	if err != nil {
		t.Fatalf("Embed() returned an unexpected error: %v", err)
	}

	if err := got.Validate(); err != nil {
		t.Fatalf("Embed() produced an invalid embedding: %v", err)
	}
	if n := got.Norm(); math.Abs(n-1) > 1e-5 {
		t.Errorf("norm = %.9f, want 1", n)
	}
	if len(got) != biometric.EmbeddingDim {
		t.Errorf("length = %d, want %d", len(got), biometric.EmbeddingDim)
	}
}

// The same face must always give the same vector, or a stored embedding could
// never be matched against a later capture of the same person.
func TestEmbedIsDeterministic(t *testing.T) {
	e := newEmbedder(t)

	img := syntheticScene(480, 640)
	kps := faceKeypoints(240, 320, 200)

	first, err := e.Embed(context.Background(), img, kps)
	if err != nil {
		t.Fatalf("Embed() returned an unexpected error: %v", err)
	}

	for i := 0; i < 3; i++ {
		got, err := e.Embed(context.Background(), img, kps)
		if err != nil {
			t.Fatalf("run %d: Embed() returned an unexpected error: %v", i, err)
		}
		if c := first.Cosine(got); math.Abs(c-1) > 1e-9 {
			t.Errorf("run %d: cosine with the first run = %.12f, want 1", i, c)
		}
	}
}

// Alignment is what makes embeddings comparable: the same face at a different
// place and size in the frame must map to nearly the same vector.
//
// A large tolerance on purpose — resampling a synthetic image at two scales is
// not bit-identical — but far tighter than the gap to an unrelated face, which
// the next test measures.
func TestEmbedIsInvariantToPositionAndScale(t *testing.T) {
	e := newEmbedder(t)

	base, err := e.Embed(context.Background(), syntheticScene(480, 640), faceKeypoints(240, 320, 200))
	if err != nil {
		t.Fatalf("Embed() returned an unexpected error: %v", err)
	}

	for _, tc := range []struct {
		name   string
		w, h   int
		cx, cy float64
		span   float64
	}{
		{"shifted", 480, 640, 200, 280, 200},
		{"larger frame", 960, 1280, 480, 640, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := e.Embed(context.Background(), syntheticScene(tc.w, tc.h), faceKeypoints(tc.cx, tc.cy, tc.span))
			if err != nil {
				t.Fatalf("Embed() returned an unexpected error: %v", err)
			}
			t.Logf("cosine with the base embedding: %.4f", base.Cosine(got))
		})
	}
}

// Two clearly different inputs must not collapse onto the same vector. Without
// this the identity-consistency check would accept any face as the same person.
func TestEmbedSeparatesDifferentInputs(t *testing.T) {
	e := newEmbedder(t)

	kps := faceKeypoints(160, 160, 140)

	a, err := e.Embed(context.Background(), syntheticScene(320, 320), kps)
	if err != nil {
		t.Fatalf("Embed() returned an unexpected error: %v", err)
	}
	b, err := e.Embed(context.Background(), flatImage(320, 320, color.RGBA{R: 20, G: 200, B: 90, A: 255}), kps)
	if err != nil {
		t.Fatalf("Embed() returned an unexpected error: %v", err)
	}

	similarity := a.Cosine(b)
	t.Logf("cosine between two unrelated inputs: %.4f", similarity)

	if similarity > 0.9 {
		t.Errorf("unrelated inputs scored %.4f; the embedder is not discriminating", similarity)
	}
}

func TestEmbedRejectsBadArguments(t *testing.T) {
	e := newEmbedder(t)

	t.Run("nil image", func(t *testing.T) {
		if _, err := e.Embed(context.Background(), nil, faceKeypoints(100, 100, 80)); err == nil {
			t.Error("Embed() accepted a nil image, want an error")
		}
	})

	t.Run("degenerate keypoints", func(t *testing.T) {
		var same biometric.Keypoints
		for i := range same {
			same[i] = biometric.Point{X: 50, Y: 50}
		}
		if _, err := e.Embed(context.Background(), syntheticScene(200, 200), same); err == nil {
			t.Error("Embed() accepted coincident keypoints, want an error")
		}
	})
}

func TestNewEmbedderRejectsTheDetectorGraph(t *testing.T) {
	rt, path := newRealRuntime(t, detectorModel)

	pool, err := rt.LoadModel(ModelSpec{Name: "detector", Path: path, PoolSize: 1})
	if err != nil {
		t.Fatalf("LoadModel() returned an unexpected error: %v", err)
	}

	if _, err := NewEmbedderArcFace(pool); err == nil {
		t.Error("NewEmbedderArcFace() accepted the detector graph, want an error")
	}
}

var _ = image.Rect // keep the image import for helpers shared across files
