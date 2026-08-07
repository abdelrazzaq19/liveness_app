package onnx

import (
	"image"
	"image/color"
	"math"
	"strings"
	"testing"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

// anchorGrid builds the three raw tensors for a grid, with every anchor scored
// zero. Tests then plant values at the anchors they care about.
func anchorGrid(gridW, gridH, anchors int) (scores, boxes, kps []float32) {
	n := gridW * gridH * anchors
	return make([]float32, n), make([]float32, n*4), make([]float32, n*2*biometric.KeypointCount)
}

// anchorIndex is the flat position of one anchor, mirroring the layout SCRFD
// emits: cell-major, with a cell's anchors adjacent.
func anchorIndex(x, y, a, gridW, anchors int) int {
	return (y*gridW+x)*anchors + a
}

// bbox is shorthand for a BBox literal. go vet rejects unkeyed composite
// literals for types from another package, and the table-driven tests below
// would be unreadable with every field named.
func bbox(minX, minY, maxX, maxY float64) biometric.BBox {
	return biometric.BBox{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY}
}

func TestDecodeStrideProducesDistanceRegressedBox(t *testing.T) {
	const (
		gridW, gridH = 4, 4
		anchors      = 2
		stride       = 8
	)

	scores, boxes, kps := anchorGrid(gridW, gridH, anchors)

	// Cell (2,1), second anchor. Its centre is at (16, 8).
	idx := anchorIndex(2, 1, 1, gridW, anchors)
	scores[idx] = 0.9

	// Distances to each edge, in stride units: left 1, top 2, right 3, bottom 4.
	copy(boxes[idx*4:], []float32{1, 2, 3, 4})

	for i := 0; i < biometric.KeypointCount; i++ {
		kps[idx*2*biometric.KeypointCount+2*i] = float32(i)    // x offset
		kps[idx*2*biometric.KeypointCount+2*i+1] = float32(-i) // y offset
	}

	got, err := decodeStride(scores, boxes, kps, stride, gridW, gridH, anchors, 0.5)
	if err != nil {
		t.Fatalf("decodeStride() returned an unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("decoded %d detections, want 1", len(got))
	}

	det := got[0]
	want := biometric.BBox{
		MinX: 16 - 1*stride, // 8
		MinY: 8 - 2*stride,  // -8
		MaxX: 16 + 3*stride, // 40
		MaxY: 8 + 4*stride,  // 40
	}
	if det.Box != want {
		t.Errorf("box = %+v, want %+v", det.Box, want)
	}
	if math.Abs(det.Score-0.9) > 1e-6 {
		t.Errorf("score = %g, want 0.9", det.Score)
	}

	for i := 0; i < biometric.KeypointCount; i++ {
		wantPt := biometric.Point{
			X: 16 + float64(i*stride),
			Y: 8 - float64(i*stride),
		}
		if det.Keypoints[i] != wantPt {
			t.Errorf("keypoint %d = %+v, want %+v", i, det.Keypoints[i], wantPt)
		}
	}
}

// Anchor layout is the easiest thing to get subtly wrong: an off-by-one in the
// indexing puts every box in the wrong place while still looking plausible.
func TestDecodeStrideAnchorLayoutIsCellMajor(t *testing.T) {
	const (
		gridW, gridH = 3, 2
		anchors      = 2
		stride       = 16
	)

	tests := []struct {
		name       string
		x, y, a    int
		wantCenter biometric.Point
	}{
		{"first anchor of first cell", 0, 0, 0, biometric.Point{X: 0, Y: 0}},
		{"second anchor of first cell", 0, 0, 1, biometric.Point{X: 0, Y: 0}},
		{"last cell of first row", 2, 0, 0, biometric.Point{X: 32, Y: 0}},
		{"first cell of second row", 0, 1, 0, biometric.Point{X: 0, Y: 16}},
		{"last anchor overall", 2, 1, 1, biometric.Point{X: 32, Y: 16}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scores, boxes, kps := anchorGrid(gridW, gridH, anchors)
			idx := anchorIndex(tt.x, tt.y, tt.a, gridW, anchors)
			scores[idx] = 1

			// Zero distances collapse the box onto the anchor centre.
			got, err := decodeStride(scores, boxes, kps, stride, gridW, gridH, anchors, 0.5)
			if err != nil {
				t.Fatalf("decodeStride() returned an unexpected error: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("decoded %d detections, want 1", len(got))
			}
			if c := got[0].Box.Center(); c != tt.wantCenter {
				t.Errorf("anchor centre = %+v, want %+v", c, tt.wantCenter)
			}
		})
	}
}

func TestDecodeStrideAppliesScoreThreshold(t *testing.T) {
	const (
		gridW, gridH = 2, 2
		anchors      = 2
	)

	scores, boxes, kps := anchorGrid(gridW, gridH, anchors)
	scores[0] = 0.1
	scores[1] = 0.5
	scores[2] = 0.9

	got, err := decodeStride(scores, boxes, kps, 8, gridW, gridH, anchors, 0.5)
	if err != nil {
		t.Fatalf("decodeStride() returned an unexpected error: %v", err)
	}
	// 0.5 is kept: the threshold is inclusive.
	if len(got) != 2 {
		t.Fatalf("decoded %d detections, want 2", len(got))
	}
}

func TestDecodeStrideWithoutKeypoints(t *testing.T) {
	scores, boxes, _ := anchorGrid(2, 2, 2)
	scores[0] = 1

	got, err := decodeStride(scores, boxes, nil, 8, 2, 2, 2, 0.5)
	if err != nil {
		t.Fatalf("decodeStride() returned an unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("decoded %d detections, want 1", len(got))
	}
	if got[0].Keypoints != (biometric.Keypoints{}) {
		t.Errorf("keypoints = %+v, want the zero value", got[0].Keypoints)
	}
}

// Short tensors mean the grid does not match the model. Reading past the end
// would be a panic at best and silent garbage at worst.
func TestDecodeStrideRejectsUndersizedTensors(t *testing.T) {
	full := func() (s, b, k []float32) { return anchorGrid(4, 4, 2) }

	tests := []struct {
		name     string
		mutate   func(s, b, k []float32) (_, _, _ []float32)
		wantHint string
	}{
		{"short scores", func(s, b, k []float32) ([]float32, []float32, []float32) {
			return s[:len(s)-1], b, k
		}, "scores"},
		{"short boxes", func(s, b, k []float32) ([]float32, []float32, []float32) {
			return s, b[:len(b)-1], k
		}, "boxes"},
		{"short keypoints", func(s, b, k []float32) ([]float32, []float32, []float32) {
			return s, b, k[:len(k)-1]
		}, "keypoints"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, b, k := tt.mutate(full())
			_, err := decodeStride(s, b, k, 8, 4, 4, 2, 0.5)
			if err == nil {
				t.Fatalf("decodeStride() succeeded on a short tensor, want an error mentioning %q", tt.wantHint)
			}
			if !strings.Contains(err.Error(), tt.wantHint) {
				t.Errorf("error does not mention %q: %v", tt.wantHint, err)
			}
		})
	}
}

func TestNMS(t *testing.T) {
	tests := []struct {
		name      string
		dets      []biometric.Detection
		threshold float64
		wantBoxes []biometric.BBox
	}{
		{
			name:      "empty input",
			dets:      nil,
			threshold: 0.4,
			wantBoxes: nil,
		},
		{
			name: "heavy overlap keeps the stronger box",
			dets: []biometric.Detection{
				{Box: bbox(0, 0, 10, 10), Score: 0.6},
				{Box: bbox(1, 1, 11, 11), Score: 0.9},
			},
			threshold: 0.4,
			wantBoxes: []biometric.BBox{bbox(1, 1, 11, 11)},
		},
		{
			name: "disjoint boxes both survive",
			dets: []biometric.Detection{
				{Box: bbox(0, 0, 10, 10), Score: 0.9},
				{Box: bbox(100, 100, 110, 110), Score: 0.8},
			},
			threshold: 0.4,
			wantBoxes: []biometric.BBox{bbox(0, 0, 10, 10), bbox(100, 100, 110, 110)},
		},
		{
			name: "overlap below the threshold survives",
			dets: []biometric.Detection{
				{Box: bbox(0, 0, 10, 10), Score: 0.9},
				// IoU here is 25/175 = 0.14.
				{Box: bbox(5, 5, 15, 15), Score: 0.8},
			},
			threshold: 0.4,
			wantBoxes: []biometric.BBox{bbox(0, 0, 10, 10), bbox(5, 5, 15, 15)},
		},
		{
			name: "a suppressed box does not suppress others",
			dets: []biometric.Detection{
				{Box: bbox(0, 0, 10, 10), Score: 0.9},
				{Box: bbox(1, 1, 11, 11), Score: 0.8},
				{Box: bbox(2, 2, 12, 12), Score: 0.7},
			},
			threshold: 0.4,
			wantBoxes: []biometric.BBox{bbox(0, 0, 10, 10)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nms(tt.dets, tt.threshold)
			if len(got) != len(tt.wantBoxes) {
				t.Fatalf("kept %d boxes, want %d: %+v", len(got), len(tt.wantBoxes), got)
			}
			for i, want := range tt.wantBoxes {
				if got[i].Box != want {
					t.Errorf("box %d = %+v, want %+v", i, got[i].Box, want)
				}
			}
		})
	}
}

// NMS must return boxes strongest first, since Detect and the API both rely on
// that ordering.
func TestNMSSortsByDescendingScore(t *testing.T) {
	dets := []biometric.Detection{
		{Box: bbox(0, 0, 10, 10), Score: 0.3},
		{Box: bbox(100, 100, 110, 110), Score: 0.9},
		{Box: bbox(200, 200, 210, 210), Score: 0.6},
	}

	got := nms(dets, 0.4)
	if len(got) != 3 {
		t.Fatalf("kept %d boxes, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Score < got[i].Score {
			t.Fatalf("scores are not descending: %v", []float64{got[0].Score, got[1].Score, got[2].Score})
		}
	}
}

// solidImage builds a w x h image filled with one colour.
func solidImage(w, h int, c color.RGBA) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func TestLetterboxScale(t *testing.T) {
	tests := []struct {
		name          string
		srcW, srcH    int
		size          int
		wantScale     float64
		wantFilledMax int // last row or column index that holds image data
	}{
		{"square fits exactly", 32, 32, 32, 1, 31},
		{"landscape is limited by width", 64, 32, 32, 0.5, 15},
		{"portrait is limited by height", 32, 64, 32, 0.5, 15},
		{"upscaled", 16, 16, 32, 2, 31},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			img := solidImage(tt.srcW, tt.srcH, color.RGBA{R: 255, G: 255, B: 255, A: 255})
			planes, scale := letterbox(img, tt.size)

			if math.Abs(scale-tt.wantScale) > 1e-9 {
				t.Errorf("scale = %g, want %g", scale, tt.wantScale)
			}
			if got, want := len(planes), 3*tt.size*tt.size; got != want {
				t.Fatalf("tensor holds %d values, want %d", got, want)
			}
		})
	}
}

// Pre-processing constants are invisible when wrong: the model still runs and
// still returns numbers, they are simply meaningless.
func TestLetterboxNormalisation(t *testing.T) {
	const size = 32
	img := solidImage(size, size, color.RGBA{R: 255, G: 255, B: 255, A: 255})

	planes, _ := letterbox(img, size)

	wantWhite := float32((255 - 127.5) / 128)
	for i, v := range planes {
		if math.Abs(float64(v-wantWhite)) > 1e-6 {
			t.Fatalf("plane value %d = %g, want %g for a white image", i, v, wantWhite)
		}
	}
}

// The padding a letterbox adds is not neutral: it becomes a dark border, and
// the reference implementation feeds the model exactly that.
func TestLetterboxPadsWithNormalisedZero(t *testing.T) {
	const size = 32
	// Half as tall as it is wide, so the bottom half of the canvas is padding.
	img := solidImage(size, size/2, color.RGBA{R: 255, G: 255, B: 255, A: 255})

	planes, _ := letterbox(img, size)

	wantPad := float32((0 - 127.5) / 128)
	plane := size * size

	// Row 16 onwards is padding; row 0 is image.
	if got := planes[0]; math.Abs(float64(got-float32((255-127.5)/128))) > 1e-6 {
		t.Errorf("first image pixel = %g, want the white value", got)
	}
	for _, offset := range []int{0, plane, 2 * plane} {
		idx := offset + 20*size // a row inside the padding
		if got := planes[idx]; math.Abs(float64(got-wantPad)) > 1e-6 {
			t.Errorf("padding value at %d = %g, want %g", idx, got, wantPad)
		}
	}
}

// Channels must land in R, G, B plane order. Swapping them shifts every score
// without producing an error anywhere.
func TestLetterboxChannelOrder(t *testing.T) {
	const size = 32
	img := solidImage(size, size, color.RGBA{R: 200, G: 100, B: 50, A: 255})

	planes, _ := letterbox(img, size)
	plane := size * size

	for _, tc := range []struct {
		name  string
		index int
		raw   float64
	}{
		{"red plane", 0, 200},
		{"green plane", plane, 100},
		{"blue plane", 2 * plane, 50},
	} {
		want := float32((tc.raw - 127.5) / 128)
		if got := planes[tc.index]; math.Abs(float64(got-want)) > 1e-6 {
			t.Errorf("%s = %g, want %g", tc.name, got, want)
		}
	}
}

func TestSCRFDOptionsValidation(t *testing.T) {
	tests := []struct {
		name     string
		opts     SCRFDOptions
		wantHint string
	}{
		{"zero input size", SCRFDOptions{MinScore: 0.5, NMSIoU: 0.4}, "input size"},
		{
			"input size not a multiple of the largest stride",
			SCRFDOptions{InputSize: 100, MinScore: 0.5, NMSIoU: 0.4},
			"input size",
		},
		{
			"score out of range",
			SCRFDOptions{InputSize: 640, MinScore: 1.5, NMSIoU: 0.4},
			"min score",
		},
		{
			"zero NMS threshold",
			SCRFDOptions{InputSize: 640, MinScore: 0.5},
			"NMS IoU",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.validate()
			if err == nil {
				t.Fatalf("validate() succeeded, want an error mentioning %q", tt.wantHint)
			}
			if !strings.Contains(err.Error(), tt.wantHint) {
				t.Errorf("error does not mention %q: %v", tt.wantHint, err)
			}
		})
	}

	valid := SCRFDOptions{InputSize: 640, MinScore: 0.6, NMSIoU: 0.4}
	if err := valid.validate(); err != nil {
		t.Errorf("validate() rejected valid options: %v", err)
	}
}

func TestNewSCRFDRejectsWrongGraphShape(t *testing.T) {
	opts := SCRFDOptions{InputSize: 640, MinScore: 0.6, NMSIoU: 0.4}

	t.Run("nil pool", func(t *testing.T) {
		if _, err := NewSCRFD(nil, opts); err == nil {
			t.Error("NewSCRFD() accepted a nil pool, want an error")
		}
	})

	t.Run("wrong output count", func(t *testing.T) {
		// A pool whose graph has three outputs is not SCRFD. Decoding it anyway
		// would produce plausible-looking nonsense.
		p, _ := newFakePool(t, 1)
		p.all[0].Inputs = make([]ort.InputOutputInfo, 1)
		p.all[0].Outputs = make([]ort.InputOutputInfo, 3)

		_, err := NewSCRFD(p, opts)
		if err == nil {
			t.Fatal("NewSCRFD() accepted a non-SCRFD graph, want an error")
		}
		if !strings.Contains(err.Error(), "9") {
			t.Errorf("error does not say how many outputs are expected: %v", err)
		}
	})

	t.Run("wrong input count", func(t *testing.T) {
		p, _ := newFakePool(t, 1)
		p.all[0].Inputs = make([]ort.InputOutputInfo, 2)
		p.all[0].Outputs = make([]ort.InputOutputInfo, scrfdOutputCount)

		if _, err := NewSCRFD(p, opts); err == nil {
			t.Error("NewSCRFD() accepted a graph with two inputs, want an error")
		}
	})
}

func TestNewSCRFDAcceptsMatchingGraph(t *testing.T) {
	p, _ := newFakePool(t, 1)
	p.all[0].Inputs = make([]ort.InputOutputInfo, 1)
	p.all[0].Outputs = make([]ort.InputOutputInfo, scrfdOutputCount)

	d, err := NewSCRFD(p, SCRFDOptions{InputSize: 640, MinScore: 0.6, NMSIoU: 0.4})
	if err != nil {
		t.Fatalf("NewSCRFD() returned an unexpected error: %v", err)
	}
	if d == nil {
		t.Fatal("NewSCRFD() returned a nil detector without an error")
	}
}
