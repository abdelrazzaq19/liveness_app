package onnx

import (
	"image"
	"image/color"
	"math"
	"testing"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

func TestSoftmax(t *testing.T) {
	tests := []struct {
		name   string
		logits []float32
		want   []float64
	}{
		{"uniform", []float32{0, 0, 0}, []float64{1.0 / 3, 1.0 / 3, 1.0 / 3}},
		{"one dominant", []float32{0, 20, 0}, []float64{0, 1, 0}},
		{"shift invariant", []float32{5, 25, 5}, []float64{0, 1, 0}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := softmax(tt.logits)
			if len(got) != len(tt.want) {
				t.Fatalf("softmax returned %d values, want %d", len(got), len(tt.want))
			}

			var sum float64
			for i, want := range tt.want {
				if math.Abs(got[i]-want) > 1e-6 {
					t.Errorf("class %d = %.6f, want %.6f", i, got[i], want)
				}
				sum += got[i]
			}
			if math.Abs(sum-1) > 1e-9 {
				t.Errorf("probabilities sum to %.9f, want 1", sum)
			}
		})
	}
}

// Logits are unbounded. Exponentiating a large one directly overflows to +Inf
// and turns the whole distribution into NaN, which would then travel into a
// liveness decision as a score that compares false against every threshold.
func TestSoftmaxSurvivesLargeLogits(t *testing.T) {
	for _, logits := range [][]float32{
		{1000, 999, 998},
		{-1000, -999, -998},
		{0, 800, -800},
	} {
		got := softmax(logits)

		var sum float64
		for i, v := range got {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("softmax(%v) class %d = %v", logits, i, v)
			}
			sum += v
		}
		if math.Abs(sum-1) > 1e-9 {
			t.Errorf("softmax(%v) sums to %.9f, want 1", logits, sum)
		}
	}
}

func TestAntiSpoofCropWidensTheBox(t *testing.T) {
	// A small box in the middle of a large frame: nothing constrains the crop.
	box := biometric.BBox{MinX: 400, MinY: 400, MaxX: 500, MaxY: 500}
	got := antiSpoofCrop(box, 1000, 1000, 2.7)

	if w := got.Dx(); w < 265 || w > 275 {
		t.Errorf("crop width = %d, want about 270 (100 px box widened 2.7x)", w)
	}
	if h := got.Dy(); h < 265 || h > 275 {
		t.Errorf("crop height = %d, want about 270", h)
	}

	// Still centred on the face.
	center := box.Center()
	cx := float64(got.Min.X+got.Max.X) / 2
	cy := float64(got.Min.Y+got.Max.Y) / 2
	if math.Abs(cx-center.X) > 2 || math.Abs(cy-center.Y) > 2 {
		t.Errorf("crop centre (%.1f, %.1f), want (%.1f, %.1f)", cx, cy, center.X, center.Y)
	}
}

// A face near the edge of frame is the normal case for a handheld phone. The
// crop must slide back into the frame at full size rather than being clipped,
// which would silently zoom in on exactly those faces.
func TestAntiSpoofCropShiftsRatherThanClipsAtTheEdges(t *testing.T) {
	const frame = 640

	tests := []struct {
		name string
		box  biometric.BBox
	}{
		{"top left corner", biometric.BBox{MinX: 0, MinY: 0, MaxX: 100, MaxY: 100}},
		{"bottom right corner", biometric.BBox{MinX: 540, MinY: 540, MaxX: 640, MaxY: 640}},
		{"left edge", biometric.BBox{MinX: 0, MinY: 250, MaxX: 100, MaxY: 350}},
		{"top edge", biometric.BBox{MinX: 250, MinY: 0, MaxX: 350, MaxY: 100}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := antiSpoofCrop(tt.box, frame, frame, 2.7)

			if got.Dx() < 265 || got.Dy() < 265 {
				t.Errorf("crop is %dx%d, want it to keep its full size by shifting inwards", got.Dx(), got.Dy())
			}
			if got.Min.X < 0 || got.Min.Y < 0 || got.Max.X > frame || got.Max.Y > frame {
				t.Errorf("crop %+v escapes the %dx%d frame", got, frame, frame)
			}
		})
	}
}

// A crop wider than the frame cannot be satisfied; the scale has to shrink
// rather than produce a rectangle hanging off both sides.
func TestAntiSpoofCropShrinksWhenTheFrameIsTooSmall(t *testing.T) {
	box := biometric.BBox{MinX: 10, MinY: 10, MaxX: 190, MaxY: 190}
	got := antiSpoofCrop(box, 200, 200, 2.7)

	if got.Min.X < 0 || got.Min.Y < 0 || got.Max.X > 200 || got.Max.Y > 200 {
		t.Errorf("crop %+v escapes the 200x200 frame", got)
	}
	if got.Empty() {
		t.Error("crop is empty")
	}
}

func TestAntiSpoofCropRejectsDegenerateInput(t *testing.T) {
	tests := []struct {
		name          string
		box           biometric.BBox
		width, height int
	}{
		{"empty box", biometric.BBox{}, 640, 480},
		{"inverted box", biometric.BBox{MinX: 100, MinY: 100, MaxX: 50, MaxY: 50}, 640, 480},
		{"empty frame", biometric.BBox{MinX: 10, MinY: 10, MaxX: 50, MaxY: 50}, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := antiSpoofCrop(tt.box, tt.width, tt.height, 2.7); !got.Empty() {
				t.Errorf("antiSpoofCrop() = %+v, want an empty rectangle", got)
			}
		})
	}
}

// The channel order is invisible when wrong: the model still runs and still
// returns a plausible score, just a different one.
func TestAntiSpoofPlanesUseBGROrder(t *testing.T) {
	patch := image.NewRGBA(image.Rect(0, 0, antiSpoofInputSize, antiSpoofInputSize))
	for y := 0; y < antiSpoofInputSize; y++ {
		for x := 0; x < antiSpoofInputSize; x++ {
			patch.SetRGBA(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}

	planes := antiSpoofPlanes(patch)
	plane := antiSpoofInputSize * antiSpoofInputSize

	if got, want := len(planes), 3*plane; got != want {
		t.Fatalf("tensor holds %d values, want %d", got, want)
	}

	for _, tc := range []struct {
		name  string
		index int
		raw   float64
	}{
		{"first plane is blue", 0, 50},
		{"second plane is green", plane, 100},
		{"third plane is red", 2 * plane, 200},
	} {
		want := float32(tc.raw / 255)
		if got := planes[tc.index]; math.Abs(float64(got-want)) > 1e-6 {
			t.Errorf("%s: %.6f, want %.6f", tc.name, got, want)
		}
	}
}

func TestAntiSpoofPlanesAreScaledToUnitRange(t *testing.T) {
	patch := image.NewRGBA(image.Rect(0, 0, antiSpoofInputSize, antiSpoofInputSize))
	for y := 0; y < antiSpoofInputSize; y++ {
		for x := 0; x < antiSpoofInputSize; x++ {
			// A gradient so the test would catch a constant being written.
			v := uint8(x * 255 / (antiSpoofInputSize - 1))
			patch.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}

	for i, v := range antiSpoofPlanes(patch) {
		if v < 0 || v > 1 {
			t.Fatalf("value %d = %.6f, outside [0,1]", i, v)
		}
	}
}

func TestNewAntiSpoofRejectsWrongGraphShape(t *testing.T) {
	t.Run("nil pool", func(t *testing.T) {
		if _, err := NewAntiSpoofMiniFASNet(nil); err == nil {
			t.Error("NewAntiSpoofMiniFASNet() accepted a nil pool, want an error")
		}
	})

	t.Run("wrong input size", func(t *testing.T) {
		p, _ := newFakePool(t, 1)
		p.all[0].Inputs = []ort.InputOutputInfo{{Dimensions: ort.NewShape(1, 3, 112, 112)}}
		p.all[0].Outputs = []ort.InputOutputInfo{{Dimensions: ort.NewShape(1, antiSpoofClasses)}}

		if _, err := NewAntiSpoofMiniFASNet(p); err == nil {
			t.Error("NewAntiSpoofMiniFASNet() accepted a 112x112 input, want an error")
		}
	})

	t.Run("wrong class count", func(t *testing.T) {
		p, _ := newFakePool(t, 1)
		p.all[0].Inputs = []ort.InputOutputInfo{{Dimensions: ort.NewShape(1, 3, antiSpoofInputSize, antiSpoofInputSize)}}
		p.all[0].Outputs = []ort.InputOutputInfo{{Dimensions: ort.NewShape(1, 512)}}

		if _, err := NewAntiSpoofMiniFASNet(p); err == nil {
			t.Error("NewAntiSpoofMiniFASNet() accepted 512 output classes, want an error")
		}
	})

	// A constructor whose whole job is to reject the wrong model must not crash
	// on one. An output with no declared dimensions is what a graph emitting a
	// scalar looks like, and reading its last dimension walks off the front of
	// the slice — so pointing the service at the wrong .onnx would panic during
	// boot instead of naming the mismatch.
	t.Run("output with no dimensions", func(t *testing.T) {
		p, _ := newFakePool(t, 1)
		p.all[0].Inputs = []ort.InputOutputInfo{{Dimensions: ort.NewShape(1, 3, antiSpoofInputSize, antiSpoofInputSize)}}
		p.all[0].Outputs = []ort.InputOutputInfo{{Dimensions: ort.NewShape()}}

		if _, err := NewAntiSpoofMiniFASNet(p); err == nil {
			t.Error("NewAntiSpoofMiniFASNet() accepted an output with no dimensions, want an error")
		}
	})

	t.Run("matching graph", func(t *testing.T) {
		p, _ := newFakePool(t, 1)
		p.all[0].Inputs = []ort.InputOutputInfo{{Dimensions: ort.NewShape(1, 3, antiSpoofInputSize, antiSpoofInputSize)}}
		p.all[0].Outputs = []ort.InputOutputInfo{{Dimensions: ort.NewShape(1, antiSpoofClasses)}}

		if _, err := NewAntiSpoofMiniFASNet(p); err != nil {
			t.Errorf("NewAntiSpoofMiniFASNet() rejected a matching graph: %v", err)
		}
	})
}
