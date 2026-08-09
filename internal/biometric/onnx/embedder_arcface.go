package onnx

import (
	"context"
	"errors"
	"fmt"
	"image"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/ziad/liveness-verifier/internal/biometric"
	"github.com/ziad/liveness-verifier/internal/imaging"
)

const (
	// arcFaceInputSize is the aligned crop the graph expects, fixed by the
	// model and matching the five-point template in internal/imaging.
	arcFaceInputSize = imaging.TemplateSize

	// ArcFace normalises to [-1,1] by dividing by 127.5, not by 128 the way the
	// detector does. The two constants look interchangeable and are not: a
	// wrong one shifts every embedding slightly, which never fails and quietly
	// degrades every similarity score in the gallery.
	arcFacePixelMean  = 127.5
	arcFacePixelScale = 127.5
)

// EmbedderArcFace turns an aligned face into a 512-dimensional descriptor.
//
// It implements biometric.Embedder.
type EmbedderArcFace struct {
	pool *Pool
}

var _ biometric.Embedder = (*EmbedderArcFace)(nil)

// NewEmbedderArcFace wraps a loaded pool as an embedder.
func NewEmbedderArcFace(pool *Pool) (*EmbedderArcFace, error) {
	if pool == nil {
		return nil, errors.New("onnx: embedder needs a session pool")
	}

	inputs, outputs := pool.Signature()
	if len(inputs) != 1 || len(outputs) != 1 {
		return nil, fmt.Errorf("onnx: embedder expects 1 input and 1 output, model %q has %d and %d",
			pool.Name(), len(inputs), len(outputs))
	}

	if dims := inputs[0].Dimensions; len(dims) != 4 ||
		dims[2] != arcFaceInputSize || dims[3] != arcFaceInputSize {
		return nil, fmt.Errorf("onnx: embedder expects a %dx%d input, model %q declares %v",
			arcFaceInputSize, arcFaceInputSize, pool.Name(), dims)
	}
	// Guarded on length first: an output with no declared dimensions is legal in
	// a graph and reading its last element panics. See the same check in
	// NewAntiSpoofMiniFASNet.
	if dims := outputs[0].Dimensions; len(dims) == 0 || dims[len(dims)-1] != biometric.EmbeddingDim {
		return nil, fmt.Errorf("onnx: embedder expects %d output dimensions, model %q declares %v",
			biometric.EmbeddingDim, pool.Name(), dims)
	}

	return &EmbedderArcFace{pool: pool}, nil
}

// Embed aligns the face onto the ArcFace template and returns its normalised
// descriptor.
func (e *EmbedderArcFace) Embed(ctx context.Context, img image.Image, kps biometric.Keypoints) (biometric.Embedding, error) {
	if img == nil {
		return nil, errors.New("onnx: embedder: image is nil")
	}

	aligned, err := imaging.AlignFace(img, kps, arcFaceInputSize)
	if err != nil {
		return nil, fmt.Errorf("onnx: embedder: align face: %w", err)
	}

	planes := arcFacePlanes(aligned)

	var out biometric.Embedding
	err = e.pool.Use(ctx, func(s *Session) error {
		in, err := ort.NewTensor(ort.NewShape(1, 3, arcFaceInputSize, arcFaceInputSize), planes)
		if err != nil {
			return fmt.Errorf("build input tensor: %w", err)
		}
		defer func() { _ = in.Destroy() }()

		outs := make([]ort.Value, 1)
		if err := s.Run([]ort.Value{in}, outs); err != nil {
			return fmt.Errorf("run embedder: %w", err)
		}
		defer destroyValues(outs)

		raw, err := floatData(outs[0], "embedding", 0)
		if err != nil {
			return err
		}
		if len(raw) < biometric.EmbeddingDim {
			return fmt.Errorf("embedding output holds %d values, need %d", len(raw), biometric.EmbeddingDim)
		}

		// Copy: the tensor's memory is freed when this closure returns.
		vec := make(biometric.Embedding, biometric.EmbeddingDim)
		copy(vec, raw[:biometric.EmbeddingDim])

		out = vec.Normalize()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("onnx: embedder on %q: %w", e.pool.Name(), err)
	}

	if err := out.Validate(); err != nil {
		return nil, fmt.Errorf("onnx: embedder on %q: %w", e.pool.Name(), err)
	}
	return out, nil
}

// arcFacePlanes writes an aligned crop as an NCHW tensor in red, green, blue
// order, normalised to [-1,1].
func arcFacePlanes(aligned *image.RGBA) []float32 {
	const size = arcFaceInputSize
	plane := size * size

	out := make([]float32, 3*plane)
	for y := 0; y < size; y++ {
		row := y * aligned.Stride
		for x := 0; x < size; x++ {
			px := row + x*4
			i := y*size + x

			out[i] = (float32(aligned.Pix[px]) - arcFacePixelMean) / arcFacePixelScale
			out[plane+i] = (float32(aligned.Pix[px+1]) - arcFacePixelMean) / arcFacePixelScale
			out[2*plane+i] = (float32(aligned.Pix[px+2]) - arcFacePixelMean) / arcFacePixelScale
		}
	}
	return out
}
