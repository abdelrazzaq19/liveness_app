package biometric

import (
	"math"
	"testing"
)

// faceShape describes a synthetic face precisely enough that its aspect ratios
// can be predicted by hand.
type faceShape struct {
	origin     Point
	scale      float64
	eyeWidth   float64
	eyeOpening float64
	mouthWidth float64
	mouthGap   float64
}

func defaultShape() faceShape {
	return faceShape{
		origin:     Point{X: 100, Y: 200},
		scale:      1,
		eyeWidth:   20,
		eyeOpening: 6,
		mouthWidth: 40,
		mouthGap:   22,
	}
}

// build lays out landmarks according to the documented index map.
//
// Every point that the metrics read is placed deliberately; the rest are filled
// with something plausible so that Bounds has real work to do.
func (f faceShape) build() Landmarks106 {
	var l Landmarks106

	at := func(x, y float64) Point {
		return Point{X: f.origin.X + x*f.scale, Y: f.origin.Y + y*f.scale}
	}

	// Contour: a rough ellipse, present so Bounds is not driven by the eyes.
	for i := ContourFirst; i <= ContourLast; i++ {
		t := float64(i-ContourFirst) / float64(ContourLast-ContourFirst)
		angle := math.Pi * t
		l[i] = at(50+60*math.Cos(angle), 40+70*math.Sin(angle))
	}

	// Eyes. Corners define the width; three upper-lid points sit directly above
	// three lower-lid ones, half the opening either side of the eye line.
	eye := func(base int, cx, cy float64) {
		half := f.eyeOpening / 2
		w := f.eyeWidth

		l[base+eyeCornerLeft] = at(cx, cy)
		l[base+eyeCornerRight] = at(cx+w, cy)

		l[base+eyeUpperLeft] = at(cx+w*0.25, cy-half)
		l[base+eyeUpperMid] = at(cx+w*0.50, cy-half)
		l[base+eyeUpperRight] = at(cx+w*0.75, cy-half)

		l[base+eyeLowerLeft] = at(cx+w*0.25, cy+half)
		l[base+eyeLowerMid] = at(cx+w*0.50, cy+half)
		l[base+eyeLowerRight] = at(cx+w*0.75, cy+half)

		// The two centre points the model duplicates.
		l[base+1] = at(cx+w*0.5, cy)
		l[base+5] = at(cx+w*0.5, cy)
	}
	eye(LeftEyeFirst, 20, 40)
	eye(RightEyeFirst, 60, 40)

	for i := LeftBrowFirst; i <= LeftBrowLast; i++ {
		l[i] = at(20+float64(i-LeftBrowFirst)*2.5, 28)
	}
	for i := RightBrowFirst; i <= RightBrowLast; i++ {
		l[i] = at(60+float64(i-RightBrowFirst)*2.5, 28)
	}
	for i := NoseFirst; i <= NoseLast; i++ {
		l[i] = at(50, 45+float64(i-NoseFirst)*1.5)
	}

	// Mouth: corners set the width, two inner pairs set the aperture.
	for i := MouthFirst; i <= MouthLast; i++ {
		l[i] = at(50, 90)
	}
	half := f.mouthGap / 2
	w := f.mouthWidth
	l[MouthFirst+mouthCornerLeft] = at(30, 90)
	l[MouthFirst+mouthCornerRight] = at(30+w, 90)
	l[MouthFirst+mouthInnerUpperL] = at(30+w*0.325, 90-half)
	l[MouthFirst+mouthInnerLowerL] = at(30+w*0.325, 90+half)
	l[MouthFirst+mouthInnerUpperR] = at(30+w*0.675, 90-half)
	l[MouthFirst+mouthInnerLowerR] = at(30+w*0.675, 90+half)

	return l
}

// The index map is a claim about the model, and every metric depends on it. If
// the ranges ever stop tiling 0..105 exactly, something has been edited by hand
// and the eye indices are no longer trustworthy.
func TestLandmarkIndexRangesTileEveryPoint(t *testing.T) {
	ranges := []struct {
		name        string
		first, last int
	}{
		{"contour", ContourFirst, ContourLast},
		{"left eye", LeftEyeFirst, LeftEyeLast},
		{"left brow", LeftBrowFirst, LeftBrowLast},
		{"mouth", MouthFirst, MouthLast},
		{"nose", NoseFirst, NoseLast},
		{"right eye", RightEyeFirst, RightEyeLast},
		{"right brow", RightBrowFirst, RightBrowLast},
	}

	seen := make([]string, LandmarkCount)
	for _, r := range ranges {
		if r.last < r.first {
			t.Fatalf("%s: range %d..%d is inverted", r.name, r.first, r.last)
		}
		for i := r.first; i <= r.last; i++ {
			if i < 0 || i >= LandmarkCount {
				t.Fatalf("%s: index %d is out of range", r.name, i)
			}
			if seen[i] != "" {
				t.Errorf("index %d claimed by both %s and %s", i, seen[i], r.name)
			}
			seen[i] = r.name
		}
	}

	for i, owner := range seen {
		if owner == "" {
			t.Errorf("index %d belongs to no group", i)
		}
	}
}

// Both eyes must share a block layout, or the offsets used for one would read
// the wrong points on the other.
func TestEyeBlocksAreTheSameSize(t *testing.T) {
	left := LeftEyeLast - LeftEyeFirst
	right := RightEyeLast - RightEyeFirst
	if left != right {
		t.Errorf("left eye spans %d indices, right eye %d", left+1, right+1)
	}

	// Every offset the metrics use must fall inside the block.
	for _, off := range []int{
		eyeLowerMid, eyeCornerLeft, eyeLowerLeft, eyeLowerRight,
		eyeCornerRight, eyeUpperMid, eyeUpperLeft, eyeUpperRight,
	} {
		if off < 0 || off > left {
			t.Errorf("eye offset %d falls outside a block of %d points", off, left+1)
		}
	}

	for _, off := range []int{
		mouthCornerLeft, mouthCornerRight,
		mouthInnerUpperL, mouthInnerUpperR, mouthInnerLowerL, mouthInnerLowerR,
	} {
		if off < 0 || off > MouthLast-MouthFirst {
			t.Errorf("mouth offset %d falls outside a block of %d points", off, MouthLast-MouthFirst+1)
		}
	}
}

// The layout makes the ratio exactly opening/width, so the arithmetic can be
// checked rather than merely eyeballed.
func TestEyeAspectRatioIsExact(t *testing.T) {
	tests := []struct {
		name    string
		opening float64
		want    float64
	}{
		{"shut", 0, 0},
		{"nearly shut", 2, 0.10},
		{"half open", 4, 0.20},
		{"open", 6, 0.30},
		{"wide", 10, 0.50},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shape := defaultShape()
			shape.eyeOpening = tt.opening
			l := shape.build()

			for _, e := range []struct {
				name string
				eye  Eye
			}{{"left", LeftEye}, {"right", RightEye}} {
				got := l.EyeAspectRatio(e.eye)
				if math.Abs(got-tt.want) > 1e-9 {
					t.Errorf("%s eye ratio = %.9f, want %.9f", e.name, got, tt.want)
				}
			}
		})
	}
}

// Scale invariance is the whole reason for using a ratio: a subject who leans
// towards the camera must not appear to blink.
func TestEyeAspectRatioIsScaleInvariant(t *testing.T) {
	base := defaultShape()
	want := base.build().MeanEyeAspectRatio()

	for _, scale := range []float64{0.25, 0.5, 2, 8} {
		shape := base
		shape.scale = scale
		shape.origin = Point{X: 500 * scale, Y: 120 * scale}

		got := shape.build().MeanEyeAspectRatio()
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("at scale %.2f ratio = %.9f, want %.9f", scale, got, want)
		}
	}
}

// The behaviour a blink challenge depends on: closed and open land on opposite
// sides of the configured hysteresis band.
func TestEyeAspectRatioSeparatesClosedFromOpen(t *testing.T) {
	const (
		blinkThreshold = 0.21 // LV_LIVENESS_EAR_BLINK
		openThreshold  = 0.30 // LV_LIVENESS_EAR_OPEN
	)

	closed := defaultShape()
	closed.eyeOpening = 1
	if got := closed.build().MeanEyeAspectRatio(); got >= blinkThreshold {
		t.Errorf("a nearly shut eye scores %.3f, want below %.2f", got, blinkThreshold)
	}

	open := defaultShape()
	open.eyeOpening = 7
	if got := open.build().MeanEyeAspectRatio(); got < openThreshold {
		t.Errorf("an open eye scores %.3f, want at least %.2f", got, openThreshold)
	}
}

func TestMeanEyeAspectRatioAveragesBothEyes(t *testing.T) {
	l := defaultShape().build()

	// Close the right eye only, by collapsing its lids onto the eye line.
	for _, off := range []int{eyeUpperLeft, eyeUpperMid, eyeUpperRight, eyeLowerLeft, eyeLowerMid, eyeLowerRight} {
		p := l[RightEyeFirst+off]
		p.Y = l[RightEyeFirst+eyeCornerLeft].Y
		l[RightEyeFirst+off] = p
	}

	left := l.EyeAspectRatio(LeftEye)
	right := l.EyeAspectRatio(RightEye)

	if right != 0 {
		t.Errorf("collapsed eye ratio = %.9f, want 0", right)
	}
	if got, want := l.MeanEyeAspectRatio(), (left+right)/2; math.Abs(got-want) > 1e-9 {
		t.Errorf("mean = %.9f, want %.9f", got, want)
	}
}

func TestMouthAspectRatioIsExact(t *testing.T) {
	tests := []struct {
		name string
		gap  float64
		want float64
	}{
		{"shut", 0, 0},
		{"slightly parted", 4, 0.10},
		{"open", 22, 0.55},
		{"wide", 40, 1.00},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shape := defaultShape()
			shape.mouthGap = tt.gap

			if got := shape.build().MouthAspectRatio(); math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("ratio = %.9f, want %.9f", got, tt.want)
			}
		})
	}
}

func TestMouthAspectRatioIsScaleInvariant(t *testing.T) {
	base := defaultShape()
	want := base.build().MouthAspectRatio()

	for _, scale := range []float64{0.3, 3, 11} {
		shape := base
		shape.scale = scale
		got := shape.build().MouthAspectRatio()
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("at scale %.2f ratio = %.9f, want %.9f", scale, got, want)
		}
	}
}

// A degenerate landmark set must give zero rather than an infinity that
// propagates into a challenge decision.
func TestRatiosOfCollapsedLandmarksAreZero(t *testing.T) {
	var l Landmarks106 // every point at the origin

	if got := l.EyeAspectRatio(LeftEye); got != 0 {
		t.Errorf("eye ratio of collapsed landmarks = %v, want 0", got)
	}
	if got := l.MouthAspectRatio(); got != 0 {
		t.Errorf("mouth ratio of collapsed landmarks = %v, want 0", got)
	}
	if got := l.MeanEyeAspectRatio(); got != 0 {
		t.Errorf("mean eye ratio of collapsed landmarks = %v, want 0", got)
	}
}

// The eye centres are what a decoded landmark set can be checked against: if it
// lands in the wrong coordinate space, these drift away from the coarse
// detector keypoints.
func TestEyeAndMouthCentres(t *testing.T) {
	shape := defaultShape()
	l := shape.build()

	left := l.EyeCenter(LeftEye)
	if math.Abs(left.X-(shape.origin.X+30)) > 1e-9 || math.Abs(left.Y-(shape.origin.Y+40)) > 1e-9 {
		t.Errorf("left eye centre = %+v, want {%.0f %.0f}", left, shape.origin.X+30, shape.origin.Y+40)
	}

	right := l.EyeCenter(RightEye)
	if right.X <= left.X {
		t.Errorf("right eye centre x = %.1f, want it to the right of the left eye at %.1f", right.X, left.X)
	}

	mouth := l.MouthCenter()
	if mouth.Y <= left.Y {
		t.Errorf("mouth centre y = %.1f, want it below the eyes at %.1f", mouth.Y, left.Y)
	}
	if math.Abs(mouth.X-(shape.origin.X+50)) > 1e-9 {
		t.Errorf("mouth centre x = %.1f, want %.1f", mouth.X, shape.origin.X+50)
	}
}

func TestBoundsEnclosesEveryLandmark(t *testing.T) {
	l := defaultShape().build()
	b := l.Bounds()

	for i, p := range l {
		if p.X < b.MinX || p.X > b.MaxX || p.Y < b.MinY || p.Y > b.MaxY {
			t.Errorf("landmark %d at %+v falls outside bounds %+v", i, p, b)
		}
	}
	if b.Width() <= 0 || b.Height() <= 0 {
		t.Errorf("bounds %+v have no area", b)
	}
}
