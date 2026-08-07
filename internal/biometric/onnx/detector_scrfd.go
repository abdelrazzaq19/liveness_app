package onnx

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"
	"sort"

	xdraw "golang.org/x/image/draw"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

// SCRFD's architecture, fixed by the exported graph rather than by choice.
//
// The model emits three feature maps, and for each one a score, a box
// regression, and a keypoint regression — nine outputs, grouped by kind.
var scrfdStrides = [...]int{8, 16, 32}

const (
	scrfdAnchorsPerCell = 2
	scrfdOutputGroups   = 3 // scores, boxes, keypoints
	scrfdOutputCount    = scrfdOutputGroups * len(scrfdStrides)

	// Pixel normalisation, matching the reference pre-processing. Getting
	// either constant wrong shifts every score without producing an error.
	scrfdPixelMean  = 127.5
	scrfdPixelScale = 128.0
)

// SCRFDOptions configures the detector.
type SCRFDOptions struct {
	// InputSize is the side of the square the image is letterboxed into. It
	// dominates inference cost: halving it is roughly four times cheaper, at
	// the price of missing smaller faces.
	InputSize int

	// MinScore drops low-confidence anchors before NMS.
	MinScore float64

	// NMSIoU is the overlap above which the weaker of two boxes is discarded.
	NMSIoU float64
}

func (o SCRFDOptions) validate() error {
	var problems []error
	if o.InputSize <= 0 || o.InputSize%32 != 0 {
		problems = append(problems, fmt.Errorf(
			"input size must be a positive multiple of 32 (the largest stride), got %d", o.InputSize))
	}
	if o.MinScore < 0 || o.MinScore > 1 {
		problems = append(problems, fmt.Errorf("min score must be in [0,1], got %g", o.MinScore))
	}
	if o.NMSIoU <= 0 || o.NMSIoU > 1 {
		problems = append(problems, fmt.Errorf("NMS IoU must be in (0,1], got %g", o.NMSIoU))
	}
	return errors.Join(problems...)
}

// SCRFD detects faces using an SCRFD graph loaded through a session pool.
//
// It implements biometric.Detector.
type SCRFD struct {
	pool *Pool
	opts SCRFDOptions
}

var _ biometric.Detector = (*SCRFD)(nil)

// NewSCRFD wraps a loaded pool as a detector.
//
// The graph's shape is checked here rather than on the first request: a model
// that is not SCRFD produces plausible-looking numbers if you decode it anyway,
// and that failure is far more expensive to find later.
func NewSCRFD(pool *Pool, opts SCRFDOptions) (*SCRFD, error) {
	if pool == nil {
		return nil, errors.New("onnx: SCRFD needs a session pool")
	}
	if err := opts.validate(); err != nil {
		return nil, fmt.Errorf("onnx: SCRFD options: %w", err)
	}

	inputs, outputs := pool.Signature()
	if len(inputs) != 1 {
		return nil, fmt.Errorf("onnx: SCRFD expects 1 graph input, model %q has %d", pool.Name(), len(inputs))
	}
	if len(outputs) != scrfdOutputCount {
		return nil, fmt.Errorf("onnx: SCRFD expects %d graph outputs, model %q has %d",
			scrfdOutputCount, pool.Name(), len(outputs))
	}

	return &SCRFD{pool: pool, opts: opts}, nil
}

// Detect returns the largest face in the image.
//
// Largest rather than highest-scoring: in a liveness session the subject is the
// person closest to the camera, and a confident detection of a bystander in the
// background is still the wrong face.
func (d *SCRFD) Detect(ctx context.Context, img image.Image) (biometric.Detection, error) {
	found, err := d.DetectAll(ctx, img)
	if err != nil {
		return biometric.Detection{}, err
	}
	if len(found) == 0 {
		return biometric.Detection{}, biometric.ErrNoFaceFound
	}

	largest := found[0]
	for _, cand := range found[1:] {
		if cand.Box.Area() > largest.Box.Area() {
			largest = cand
		}
	}
	return largest, nil
}

// DetectAll returns every face that survives thresholding and NMS, in
// descending score order.
func (d *SCRFD) DetectAll(ctx context.Context, img image.Image) ([]biometric.Detection, error) {
	if img == nil {
		return nil, errors.New("onnx: SCRFD: image is nil")
	}
	bounds := img.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return nil, errors.New("onnx: SCRFD: image has no pixels")
	}

	planes, scale := letterbox(img, d.opts.InputSize)

	var raw []biometric.Detection
	err := d.pool.Use(ctx, func(s *Session) error {
		in, err := ort.NewTensor(ort.NewShape(1, 3, int64(d.opts.InputSize), int64(d.opts.InputSize)), planes)
		if err != nil {
			return fmt.Errorf("build input tensor: %w", err)
		}
		defer func() { _ = in.Destroy() }()

		// Nil entries let ONNX Runtime size the outputs, which the graph's
		// dynamic anchor counts require.
		outs := make([]ort.Value, len(s.Outputs))
		if err := s.Run([]ort.Value{in}, outs); err != nil {
			return fmt.Errorf("run detector: %w", err)
		}
		defer destroyValues(outs)

		raw, err = d.decodeOutputs(outs)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("onnx: SCRFD on %q: %w", d.pool.Name(), err)
	}

	kept := nms(raw, d.opts.NMSIoU)

	// Map back from the letterboxed copy to the caller's coordinate space.
	// Everything downstream crops the original image, not the resized one.
	inv := 1 / scale
	imgW := float64(bounds.Dx())
	imgH := float64(bounds.Dy())
	for i := range kept {
		kept[i].Box = kept[i].Box.Scale(inv).Clip(imgW, imgH)
		kept[i].Keypoints = kept[i].Keypoints.Scale(inv)
	}
	return kept, nil
}

// decodeOutputs turns the nine raw tensors into detections in letterboxed
// coordinates.
func (d *SCRFD) decodeOutputs(outs []ort.Value) ([]biometric.Detection, error) {
	if len(outs) != scrfdOutputCount {
		return nil, fmt.Errorf("expected %d outputs, got %d", scrfdOutputCount, len(outs))
	}

	var all []biometric.Detection
	for i, stride := range scrfdStrides {
		scores, err := floatData(outs[i], "scores", stride)
		if err != nil {
			return nil, err
		}
		boxes, err := floatData(outs[len(scrfdStrides)+i], "boxes", stride)
		if err != nil {
			return nil, err
		}
		kps, err := floatData(outs[2*len(scrfdStrides)+i], "keypoints", stride)
		if err != nil {
			return nil, err
		}

		grid := d.opts.InputSize / stride
		dets, err := decodeStride(scores, boxes, kps, stride, grid, grid, scrfdAnchorsPerCell, d.opts.MinScore)
		if err != nil {
			return nil, fmt.Errorf("stride %d: %w", stride, err)
		}
		all = append(all, dets...)
	}
	return all, nil
}

// decodeStride turns one feature map into detections.
//
// SCRFD regresses distances from the anchor centre to each box edge, in units
// of the stride — not corner coordinates. Treating the four numbers as a box
// directly yields boxes that look almost plausible, which is why this is worth
// stating.
func decodeStride(scores, boxes, kps []float32, stride, gridW, gridH, anchors int, minScore float64) ([]biometric.Detection, error) {
	cells := gridW * gridH * anchors
	if len(scores) < cells {
		return nil, fmt.Errorf("scores hold %d anchors, grid needs %d", len(scores), cells)
	}
	if len(boxes) < cells*4 {
		return nil, fmt.Errorf("boxes hold %d values, grid needs %d", len(boxes), cells*4)
	}
	if len(kps) > 0 && len(kps) < cells*2*biometric.KeypointCount {
		return nil, fmt.Errorf("keypoints hold %d values, grid needs %d", len(kps), cells*2*biometric.KeypointCount)
	}

	s := float64(stride)
	dets := make([]biometric.Detection, 0, 8)

	anchor := 0
	for y := 0; y < gridH; y++ {
		for x := 0; x < gridW; x++ {
			// Anchors of one cell sit next to each other in the output, which
			// is why the centre is computed once per cell and reused.
			cx := float64(x) * s
			cy := float64(y) * s

			for a := 0; a < anchors; a++ {
				score := float64(scores[anchor])
				if score < minScore {
					anchor++
					continue
				}

				b := boxes[anchor*4:]
				det := biometric.Detection{
					Score: score,
					Box: biometric.BBox{
						MinX: cx - float64(b[0])*s,
						MinY: cy - float64(b[1])*s,
						MaxX: cx + float64(b[2])*s,
						MaxY: cy + float64(b[3])*s,
					},
				}

				if len(kps) > 0 {
					k := kps[anchor*2*biometric.KeypointCount:]
					for i := 0; i < biometric.KeypointCount; i++ {
						det.Keypoints[i] = biometric.Point{
							X: cx + float64(k[2*i])*s,
							Y: cy + float64(k[2*i+1])*s,
						}
					}
				}

				dets = append(dets, det)
				anchor++
			}
		}
	}
	return dets, nil
}

// nms keeps the highest-scoring box of each overlapping cluster.
//
// It sorts dets in place.
func nms(dets []biometric.Detection, iouThreshold float64) []biometric.Detection {
	if len(dets) == 0 {
		return nil
	}

	sort.SliceStable(dets, func(i, j int) bool { return dets[i].Score > dets[j].Score })

	kept := make([]biometric.Detection, 0, len(dets))
	suppressed := make([]bool, len(dets))

	for i := range dets {
		if suppressed[i] {
			continue
		}
		kept = append(kept, dets[i])

		for j := i + 1; j < len(dets); j++ {
			if !suppressed[j] && dets[i].Box.IoU(dets[j].Box) > iouThreshold {
				suppressed[j] = true
			}
		}
	}
	return kept
}

// letterbox scales img to fit a size x size square and writes it as an NCHW
// float tensor, returning the scale that was applied.
//
// The image is pasted at the top left rather than centred. That is not a
// stylistic choice: it matches the reference pre-processing, and a centred
// paste would offset every coordinate the model produces by half the padding.
func letterbox(img image.Image, size int) ([]float32, float64) {
	bounds := img.Bounds()
	srcW := float64(bounds.Dx())
	srcH := float64(bounds.Dy())

	scale := math.Min(float64(size)/srcW, float64(size)/srcH)
	dstW := int(srcW * scale)
	dstH := int(srcH * scale)

	// Padding stays at zero, which after normalisation is a uniform dark
	// border — the same thing the reference implementation feeds the model.
	canvas := image.NewRGBA(image.Rect(0, 0, size, size))
	xdraw.BiLinear.Scale(canvas, image.Rect(0, 0, dstW, dstH), img, bounds, xdraw.Src, nil)

	plane := size * size
	out := make([]float32, 3*plane)

	for y := 0; y < size; y++ {
		row := y * canvas.Stride
		for x := 0; x < size; x++ {
			px := row + x*4
			i := y*size + x
			out[i] = (float32(canvas.Pix[px]) - scrfdPixelMean) / scrfdPixelScale
			out[plane+i] = (float32(canvas.Pix[px+1]) - scrfdPixelMean) / scrfdPixelScale
			out[2*plane+i] = (float32(canvas.Pix[px+2]) - scrfdPixelMean) / scrfdPixelScale
		}
	}
	return out, scale
}

// floatData extracts the float32 payload of an output tensor.
func floatData(v ort.Value, kind string, stride int) ([]float32, error) {
	if v == nil {
		return nil, fmt.Errorf("%s output for stride %d was not allocated", kind, stride)
	}
	t, ok := v.(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("%s output for stride %d is %T, want a float32 tensor", kind, stride, v)
	}
	return t.GetData(), nil
}

func destroyValues(values []ort.Value) {
	for _, v := range values {
		if v != nil {
			_ = v.Destroy()
		}
	}
}
