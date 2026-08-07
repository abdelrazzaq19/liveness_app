package onnx

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"

	xdraw "golang.org/x/image/draw"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

const (
	// antiSpoofInputSize is the square crop the graph expects, fixed by the
	// checkpoint the model was converted from (2.7_80x80_MiniFASNetV2).
	antiSpoofInputSize = 80

	// antiSpoofCropScale is how much wider than the detected face box the crop
	// must be. It is the 2.7 in the checkpoint's name, and it is part of the
	// model rather than a tuning knob: the network only ever saw faces framed
	// this way.
	antiSpoofCropScale = 2.7

	// antiSpoofClasses is the number of logits the graph emits.
	antiSpoofClasses = 3

	// antiSpoofRealClass is the logit that means "a live face". The other two
	// are kinds of attack.
	antiSpoofRealClass = 1
)

// AntiSpoofMiniFASNet scores frames for passive liveness.
//
// It implements biometric.AntiSpoofer.
type AntiSpoofMiniFASNet struct {
	pool *Pool
}

var _ biometric.AntiSpoofer = (*AntiSpoofMiniFASNet)(nil)

// NewAntiSpoofMiniFASNet wraps a loaded pool as a passive liveness scorer.
func NewAntiSpoofMiniFASNet(pool *Pool) (*AntiSpoofMiniFASNet, error) {
	if pool == nil {
		return nil, errors.New("onnx: anti-spoofer needs a session pool")
	}

	inputs, outputs := pool.Signature()
	if len(inputs) != 1 || len(outputs) != 1 {
		return nil, fmt.Errorf("onnx: anti-spoofer expects 1 input and 1 output, model %q has %d and %d",
			pool.Name(), len(inputs), len(outputs))
	}

	if dims := inputs[0].Dimensions; len(dims) != 4 ||
		dims[2] != antiSpoofInputSize || dims[3] != antiSpoofInputSize {
		return nil, fmt.Errorf("onnx: anti-spoofer expects a %dx%d input, model %q declares %v",
			antiSpoofInputSize, antiSpoofInputSize, pool.Name(), dims)
	}
	if dims := outputs[0].Dimensions; dims[len(dims)-1] != antiSpoofClasses {
		return nil, fmt.Errorf("onnx: anti-spoofer expects %d output classes, model %q declares %v",
			antiSpoofClasses, pool.Name(), dims)
	}

	return &AntiSpoofMiniFASNet{pool: pool}, nil
}

// LivenessScore returns the probability that the face in box is a live person.
func (a *AntiSpoofMiniFASNet) LivenessScore(ctx context.Context, img image.Image, box biometric.BBox) (float64, error) {
	if img == nil {
		return 0, errors.New("onnx: anti-spoofer: image is nil")
	}
	if box.Width() <= 0 || box.Height() <= 0 {
		return 0, fmt.Errorf("onnx: anti-spoofer: box %+v has no area", box)
	}

	bounds := img.Bounds()
	crop := antiSpoofCrop(box, bounds.Dx(), bounds.Dy(), antiSpoofCropScale)
	if crop.Empty() {
		return 0, fmt.Errorf("onnx: anti-spoofer: crop for box %+v is empty", box)
	}

	// Anisotropic on purpose: upstream resizes whatever rectangle it cropped
	// straight to the square input, so a non-square face box is stretched. The
	// network was trained on exactly that, and "fixing" it would feed it
	// geometry it has never seen.
	patch := image.NewRGBA(image.Rect(0, 0, antiSpoofInputSize, antiSpoofInputSize))
	xdraw.BiLinear.Scale(patch, patch.Bounds(), img, crop.Add(bounds.Min), xdraw.Src, nil)

	planes := antiSpoofPlanes(patch)

	var score float64
	err := a.pool.Use(ctx, func(s *Session) error {
		in, err := ort.NewTensor(ort.NewShape(1, 3, antiSpoofInputSize, antiSpoofInputSize), planes)
		if err != nil {
			return fmt.Errorf("build input tensor: %w", err)
		}
		defer func() { _ = in.Destroy() }()

		outs := make([]ort.Value, 1)
		if err := s.Run([]ort.Value{in}, outs); err != nil {
			return fmt.Errorf("run anti-spoofer: %w", err)
		}
		defer destroyValues(outs)

		logits, err := floatData(outs[0], "logits", 0)
		if err != nil {
			return err
		}
		if len(logits) < antiSpoofClasses {
			return fmt.Errorf("anti-spoof output holds %d values, need %d", len(logits), antiSpoofClasses)
		}

		score = softmax(logits[:antiSpoofClasses])[antiSpoofRealClass]
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("onnx: anti-spoofer on %q: %w", a.pool.Name(), err)
	}
	return score, nil
}

// antiSpoofCrop widens the face box by scale and keeps it inside the frame.
//
// A box that would run off an edge is shifted back in rather than clipped, so
// the crop keeps its size and the face keeps its proportions. Clipping instead
// would silently zoom in on faces near the edge of frame, which is where a
// handheld phone puts them.
func antiSpoofCrop(box biometric.BBox, width, height int, scale float64) image.Rectangle {
	boxW := box.Width()
	boxH := box.Height()
	if boxW <= 0 || boxH <= 0 || width <= 0 || height <= 0 {
		return image.Rectangle{}
	}

	// A crop larger than the frame cannot be satisfied; shrink the scale until
	// it fits rather than producing a rectangle that hangs off both sides.
	scale = math.Min(scale, math.Min(float64(width-1)/boxW, float64(height-1)/boxH))

	newW := boxW * scale
	newH := boxH * scale
	center := box.Center()

	left := center.X - newW/2
	top := center.Y - newH/2
	right := center.X + newW/2
	bottom := center.Y + newH/2

	if left < 0 {
		right -= left
		left = 0
	}
	if top < 0 {
		bottom -= top
		top = 0
	}
	if maxX := float64(width - 1); right > maxX {
		left -= right - maxX
		right = maxX
	}
	if maxY := float64(height - 1); bottom > maxY {
		top -= bottom - maxY
		bottom = maxY
	}

	r := image.Rect(int(left), int(top), int(right)+1, int(bottom)+1)
	return r.Intersect(image.Rect(0, 0, width, height))
}

// antiSpoofPlanes writes the patch as an NCHW tensor in blue, green, red order,
// scaled to [0,1].
//
// BGR, not RGB. Upstream reads frames with OpenCV, which decodes to BGR, and
// then applies a plain ToTensor that only divides by 255 and moves the axes —
// it never swaps the channels. Feeding RGB here produces no error and a
// meaningfully different score.
func antiSpoofPlanes(patch *image.RGBA) []float32 {
	const size = antiSpoofInputSize
	plane := size * size

	out := make([]float32, 3*plane)
	for y := 0; y < size; y++ {
		row := y * patch.Stride
		for x := 0; x < size; x++ {
			px := row + x*4
			i := y*size + x

			out[i] = float32(patch.Pix[px+2]) / 255       // blue
			out[plane+i] = float32(patch.Pix[px+1]) / 255 // green
			out[2*plane+i] = float32(patch.Pix[px]) / 255 // red
		}
	}
	return out
}

// softmax converts logits to probabilities.
//
// The maximum is subtracted first: the values are unbounded, and exponentiating
// a large one directly overflows to +Inf, which turns the whole distribution
// into NaN.
func softmax(logits []float32) []float64 {
	maxLogit := float64(logits[0])
	for _, v := range logits[1:] {
		if float64(v) > maxLogit {
			maxLogit = float64(v)
		}
	}

	out := make([]float64, len(logits))
	var sum float64
	for i, v := range logits {
		e := math.Exp(float64(v) - maxLogit)
		out[i] = e
		sum += e
	}

	if sum == 0 {
		return out
	}
	for i := range out {
		out[i] /= sum
	}
	return out
}
