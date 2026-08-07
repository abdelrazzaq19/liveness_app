package biometric

import "math"

// LandmarkCount is the number of points the dense landmark model produces.
const LandmarkCount = 106

// Landmarks106 is the dense landmark set, in source image pixel coordinates.
type Landmarks106 [LandmarkCount]Point

// Landmark index ranges.
//
// These were read off the model's own output rather than taken from
// documentation: a landmark network regresses towards a mean face when given an
// input with no face in it, and that canonical shape makes the groups
// unmistakable. The evidence is asserted in metrics_test.go, so a wrong index
// map fails a test rather than quietly skewing every blink measurement.
//
//	 0- 32  face contour, ear to chin to ear
//	33- 42  eye, image left
//	43- 51  eyebrow above it
//	52- 71  mouth, outer ring then inner
//	72- 86  nose, bridge then base
//	87- 96  eye, image right
//	97-105  eyebrow above it
//
// "Left" and "right" are the viewer's, matching Keypoints.
const (
	ContourFirst = 0
	ContourLast  = 32

	LeftEyeFirst  = 33
	LeftEyeLast   = 42
	LeftBrowFirst = 43
	LeftBrowLast  = 51

	MouthFirst = 52
	MouthLast  = 71

	NoseFirst = 72
	NoseLast  = 86

	RightEyeFirst  = 87
	RightEyeLast   = 96
	RightBrowFirst = 97
	RightBrowLast  = 105
)

// Offsets within an eye's ten-point block. Both eyes share the layout, so these
// are added to LeftEyeFirst or RightEyeFirst.
//
// Named by position in the image, not anatomy. The block orders its points the
// same way for both eyes — offset 2 is always the leftmost point and 6 the
// rightmost — so "outer" and "inner" would name the same offset differently
// depending on which eye it was applied to, which is exactly the sort of thing
// that reads fine and computes the wrong answer half the time.
//
// The three upper-lid points sit directly above the three lower-lid ones, which
// is what makes an aspect ratio meaningful: each pair measures the eye opening
// at one horizontal position.
const (
	eyeLowerMid   = 0
	eyeCornerLeft = 2
	eyeLowerLeft  = 3
	eyeLowerRight = 4

	eyeCornerRight = 6
	eyeUpperMid    = 7
	eyeUpperLeft   = 8
	eyeUpperRight  = 9
)

// Offsets within the mouth's twenty-point block.
//
// The inner ring is used rather than the outer one. Outer lip boundaries barely
// move when the mouth opens a little, because they include lip thickness; the
// inner aperture is the thing a "open your mouth" challenge is actually asking
// about.
const (
	mouthCornerLeft  = 0
	mouthCornerRight = 9
	mouthInnerUpperL = 19
	mouthInnerUpperR = 15
	mouthInnerLowerL = 8
	mouthInnerLowerR = 5
)

// Eye selects which eye a metric applies to.
type Eye int

const (
	LeftEye Eye = iota
	RightEye
)

// first returns the index the eye's block starts at.
func (e Eye) first() int {
	if e == RightEye {
		return RightEyeFirst
	}
	return LeftEyeFirst
}

// EyeAspectRatio returns the eye opening as a fraction of the eye width.
//
// This is the Soukupová–Čech eye aspect ratio, widened from one vertical pair
// to three: a single pair is noisy, and the extra pairs cost nothing once the
// landmarks are already computed. It is scale-invariant, so it does not change
// when the subject moves closer to the camera.
//
// A closed eye tends towards zero. What counts as "closed" is a threshold, and
// the one in configuration is a literature value for a different landmark
// scheme — see the warning on MouthAspectRatio.
func (l Landmarks106) EyeAspectRatio(e Eye) float64 {
	base := e.first()

	width := distance(l[base+eyeCornerLeft], l[base+eyeCornerRight])
	if width <= 0 {
		return 0
	}

	opening := distance(l[base+eyeUpperLeft], l[base+eyeLowerLeft]) +
		distance(l[base+eyeUpperMid], l[base+eyeLowerMid]) +
		distance(l[base+eyeUpperRight], l[base+eyeLowerRight])

	return opening / (3 * width)
}

// MeanEyeAspectRatio averages both eyes.
//
// Blink detection uses both together because a genuine blink closes them at
// once, while one eye alone is as likely to be a landmark glitch as a wink.
func (l Landmarks106) MeanEyeAspectRatio() float64 {
	return (l.EyeAspectRatio(LeftEye) + l.EyeAspectRatio(RightEye)) / 2
}

// MouthAspectRatio returns the inner mouth opening as a fraction of mouth width.
//
// ⚠ The thresholds in configuration (EAR 0.21/0.30, MAR 0.55) are values from
// the literature for the 68-point dlib scheme. They do not transfer to this
// 106-point layout with these index choices, and are placeholders until the
// calibration harness measures real ones. Until then any challenge decision
// built on them is provisional.
func (l Landmarks106) MouthAspectRatio() float64 {
	width := distance(l[MouthFirst+mouthCornerLeft], l[MouthFirst+mouthCornerRight])
	if width <= 0 {
		return 0
	}

	opening := distance(l[MouthFirst+mouthInnerUpperL], l[MouthFirst+mouthInnerLowerL]) +
		distance(l[MouthFirst+mouthInnerUpperR], l[MouthFirst+mouthInnerLowerR])

	return opening / (2 * width)
}

// EyeCenter returns the midpoint between an eye's two corners.
//
// It is what the coarse detector keypoints can be checked against: if the dense
// landmarks are decoded into the wrong coordinate space, these drift far from
// where the detector said the eyes were.
func (l Landmarks106) EyeCenter(e Eye) Point {
	base := e.first()
	a := l[base+eyeCornerLeft]
	b := l[base+eyeCornerRight]
	return Point{X: (a.X + b.X) / 2, Y: (a.Y + b.Y) / 2}
}

// MouthCenter returns the midpoint between the mouth corners.
func (l Landmarks106) MouthCenter() Point {
	a := l[MouthFirst+mouthCornerLeft]
	b := l[MouthFirst+mouthCornerRight]
	return Point{X: (a.X + b.X) / 2, Y: (a.Y + b.Y) / 2}
}

// Bounds returns the axis-aligned box enclosing every landmark.
func (l Landmarks106) Bounds() BBox {
	b := BBox{MinX: math.Inf(1), MinY: math.Inf(1), MaxX: math.Inf(-1), MaxY: math.Inf(-1)}
	for _, p := range l {
		b.MinX = math.Min(b.MinX, p.X)
		b.MinY = math.Min(b.MinY, p.Y)
		b.MaxX = math.Max(b.MaxX, p.X)
		b.MaxY = math.Max(b.MaxY, p.Y)
	}
	return b
}

func distance(a, b Point) float64 {
	return math.Hypot(a.X-b.X, a.Y-b.Y)
}
