package biometric

import (
	"math"
	"testing"
)

const eps = 1e-9

func TestBBoxGeometry(t *testing.T) {
	b := BBox{MinX: 10, MinY: 20, MaxX: 40, MaxY: 60}

	if got, want := b.Width(), 30.0; math.Abs(got-want) > eps {
		t.Errorf("Width() = %g, want %g", got, want)
	}
	if got, want := b.Height(), 40.0; math.Abs(got-want) > eps {
		t.Errorf("Height() = %g, want %g", got, want)
	}
	if got, want := b.Area(), 1200.0; math.Abs(got-want) > eps {
		t.Errorf("Area() = %g, want %g", got, want)
	}
	if got := b.Center(); math.Abs(got.X-25) > eps || math.Abs(got.Y-40) > eps {
		t.Errorf("Center() = %+v, want {25 40}", got)
	}
}

// A regression head is unconstrained, so an inverted box is something the
// detector can genuinely emit. It must read as empty rather than negative.
func TestBBoxInvertedReadsAsEmpty(t *testing.T) {
	b := BBox{MinX: 40, MinY: 60, MaxX: 10, MaxY: 20}

	if got := b.Width(); got != 0 {
		t.Errorf("Width() = %g on an inverted box, want 0", got)
	}
	if got := b.Height(); got != 0 {
		t.Errorf("Height() = %g on an inverted box, want 0", got)
	}
	if got := b.Area(); got != 0 {
		t.Errorf("Area() = %g on an inverted box, want 0", got)
	}
}

func TestBBoxIoU(t *testing.T) {
	tests := []struct {
		name string
		a, b BBox
		want float64
	}{
		{
			name: "identical",
			a:    BBox{0, 0, 10, 10},
			b:    BBox{0, 0, 10, 10},
			want: 1,
		},
		{
			name: "disjoint",
			a:    BBox{0, 0, 10, 10},
			b:    BBox{20, 20, 30, 30},
			want: 0,
		},
		{
			name: "touching edges only",
			a:    BBox{0, 0, 10, 10},
			b:    BBox{10, 0, 20, 10},
			want: 0,
		},
		{
			name: "quarter overlap",
			// Intersection 5x5=25, union 100+100-25=175.
			a:    BBox{0, 0, 10, 10},
			b:    BBox{5, 5, 15, 15},
			want: 25.0 / 175.0,
		},
		{
			name: "contained",
			// Intersection 25, union 100.
			a:    BBox{0, 0, 10, 10},
			b:    BBox{2, 2, 7, 7},
			want: 25.0 / 100.0,
		},
		{
			name: "degenerate box",
			a:    BBox{5, 5, 5, 5},
			b:    BBox{0, 0, 10, 10},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.a.IoU(tt.b)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("IoU() = %g, want %g", got, tt.want)
			}
			// IoU is symmetric; an implementation that is not would break NMS
			// in a way that depends on detection order.
			if rev := tt.b.IoU(tt.a); math.Abs(got-rev) > 1e-9 {
				t.Errorf("IoU is not symmetric: %g forwards, %g backwards", got, rev)
			}
		})
	}
}

func TestBBoxClipConfinesToFrame(t *testing.T) {
	tests := []struct {
		name string
		box  BBox
		want BBox
	}{
		{"already inside", BBox{10, 10, 50, 50}, BBox{10, 10, 50, 50}},
		{"over the right edge", BBox{80, 10, 120, 50}, BBox{80, 10, 100, 50}},
		{"negative origin", BBox{-20, -30, 50, 50}, BBox{0, 0, 50, 50}},
		{"entirely outside", BBox{200, 200, 300, 300}, BBox{100, 100, 100, 100}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.box.Clip(100, 100)
			if got != tt.want {
				t.Errorf("Clip() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestScalingMapsBackToTheOriginalImage(t *testing.T) {
	// A detection made on an image shrunk by 0.5 must double when mapped back.
	const scale = 0.5

	box := BBox{MinX: 10, MinY: 20, MaxX: 30, MaxY: 40}
	if got, want := box.Scale(1/scale), (BBox{20, 40, 60, 80}); got != want {
		t.Errorf("Scale() = %+v, want %+v", got, want)
	}

	var kps Keypoints
	kps[KeypointLeftEye] = Point{X: 4, Y: 8}
	kps[KeypointMouthRight] = Point{X: 12, Y: 16}

	scaled := kps.Scale(1 / scale)
	if got, want := scaled[KeypointLeftEye], (Point{8, 16}); got != want {
		t.Errorf("left eye = %+v, want %+v", got, want)
	}
	if got, want := scaled[KeypointMouthRight], (Point{24, 32}); got != want {
		t.Errorf("mouth right = %+v, want %+v", got, want)
	}
}

// The alignment step indexes keypoints positionally, so the order is part of
// the contract rather than an implementation detail.
func TestKeypointOrderIsFixed(t *testing.T) {
	order := []int{
		KeypointLeftEye, KeypointRightEye, KeypointNose,
		KeypointMouthLeft, KeypointMouthRight,
	}
	for want, got := range order {
		if got != want {
			t.Errorf("keypoint constant at position %d = %d, want %d", want, got, want)
		}
	}
	if KeypointCount != len(order) {
		t.Errorf("KeypointCount = %d, want %d", KeypointCount, len(order))
	}
}
