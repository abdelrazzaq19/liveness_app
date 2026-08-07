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
	// landmarkInputSize is the square crop the 2d106 graph expects. It is fixed
	// by the exported model, not a choice.
	landmarkInputSize = 192

	// landmarkCropMargin widens the crop beyond the detected box.
	//
	// The detector's box is tight around the face, and the landmark model was
	// trained on a looser crop. Feeding it a tight one pushes the contour
	// points against the edge, where they clip.
	landmarkCropMargin = 1.5

	// landmarkOutputValues is 106 points of x and y.
	landmarkOutputValues = 2 * biometric.LandmarkCount
)

// Landmarker2d106 produces 106 dense landmarks for a detected face.
//
// It implements biometric.Landmarker.
type Landmarker2d106 struct {
	pool *Pool
}

var _ biometric.Landmarker = (*Landmarker2d106)(nil)

// NewLandmarker2d106 wraps a loaded pool as a landmarker.
//
// The graph shape is checked here so that pointing this at the wrong model
// fails at wiring time rather than producing 106 plausible-looking points in
// the wrong places.
func NewLandmarker2d106(pool *Pool) (*Landmarker2d106, error) {
	if pool == nil {
		return nil, errors.New("onnx: landmarker needs a session pool")
	}

	inputs, outputs := pool.Signature()
	if len(inputs) != 1 {
		return nil, fmt.Errorf("onnx: landmarker expects 1 graph input, model %q has %d", pool.Name(), len(inputs))
	}
	if len(outputs) != 1 {
		return nil, fmt.Errorf("onnx: landmarker expects 1 graph output, model %q has %d", pool.Name(), len(outputs))
	}

	// The output is a flat vector of 106 coordinate pairs.
	if dims := outputs[0].Dimensions; len(dims) != 2 || dims[1] != landmarkOutputValues {
		return nil, fmt.Errorf("onnx: landmarker expects an output of %d values, model %q declares %v",
			landmarkOutputValues, pool.Name(), dims)
	}

	return &Landmarker2d106{pool: pool}, nil
}

// Landmarks returns the dense landmarks for the face in box, in the source
// image's pixel coordinates.
func (l *Landmarker2d106) Landmarks(ctx context.Context, img image.Image, box biometric.BBox) (biometric.Landmarks106, error) {
	var out biometric.Landmarks106

	if img == nil {
		return out, errors.New("onnx: landmarker: image is nil")
	}
	if box.Width() <= 0 || box.Height() <= 0 {
		return out, fmt.Errorf("onnx: landmarker: box %+v has no area", box)
	}

	crop := landmarkCrop(box)

	warped, err := imaging.Warp(img, crop, landmarkInputSize, landmarkInputSize)
	if err != nil {
		return out, fmt.Errorf("onnx: landmarker: crop face: %w", err)
	}
	// The reference pre-processing feeds raw 0-255 values here, unlike the
	// detector's (v-127.5)/128. Normalising anyway shifts every landmark
	// without producing an error.
	planes := toPlanesRaw(warped, landmarkInputSize)

	toImage, err := crop.Invert()
	if err != nil {
		return out, fmt.Errorf("onnx: landmarker: %w", err)
	}

	err = l.pool.Use(ctx, func(s *Session) error {
		in, err := ort.NewTensor(ort.NewShape(1, 3, landmarkInputSize, landmarkInputSize), planes)
		if err != nil {
			return fmt.Errorf("build input tensor: %w", err)
		}
		defer func() { _ = in.Destroy() }()

		outs := make([]ort.Value, 1)
		if err := s.Run([]ort.Value{in}, outs); err != nil {
			return fmt.Errorf("run landmarker: %w", err)
		}
		defer destroyValues(outs)

		raw, err := floatData(outs[0], "landmarks", 0)
		if err != nil {
			return err
		}
		if len(raw) < landmarkOutputValues {
			return fmt.Errorf("landmark output holds %d values, need %d", len(raw), landmarkOutputValues)
		}

		out = decodeLandmarks(raw, toImage)
		return nil
	})
	if err != nil {
		return biometric.Landmarks106{}, fmt.Errorf("onnx: landmarker on %q: %w", l.pool.Name(), err)
	}
	return out, nil
}

// landmarkCrop is the transform from source image coordinates into the model's
// square crop.
//
// It is a pure scale and translation, centred on the detected box: no rotation,
// because the landmark model is trained to cope with head roll itself and
// straightening the crop first would only lose the pose the caller wants back.
func landmarkCrop(box biometric.BBox) imaging.Similarity {
	side := box.Width()
	if box.Height() > side {
		side = box.Height()
	}

	scale := landmarkInputSize / (side * landmarkCropMargin)
	center := box.Center()

	return imaging.Similarity{
		A:  scale,
		B:  0,
		Tx: landmarkInputSize/2 - scale*center.X,
		Ty: landmarkInputSize/2 - scale*center.Y,
	}
}

// decodeLandmarks turns the model's normalised output into image coordinates.
//
// The graph emits values in roughly [-1,1] relative to the crop centre, so each
// is shifted and scaled into crop pixels before the crop transform is undone.
func decodeLandmarks(raw []float32, toImage imaging.Similarity) biometric.Landmarks106 {
	const half = landmarkInputSize / 2

	var out biometric.Landmarks106
	for i := 0; i < biometric.LandmarkCount; i++ {
		crop := biometric.Point{
			X: (float64(raw[2*i]) + 1) * half,
			Y: (float64(raw[2*i+1]) + 1) * half,
		}
		out[i] = toImage.Apply(crop)
	}
	return out
}

// toPlanesRaw writes an RGBA image as an NCHW float tensor without
// normalisation.
func toPlanesRaw(img *image.RGBA, size int) []float32 {
	plane := size * size
	out := make([]float32, 3*plane)

	for y := 0; y < size; y++ {
		row := y * img.Stride
		for x := 0; x < size; x++ {
			px := row + x*4
			i := y*size + x
			out[i] = float32(img.Pix[px])
			out[plane+i] = float32(img.Pix[px+1])
			out[2*plane+i] = float32(img.Pix[px+2])
		}
	}
	return out
}
