// Package biometric declares the face analysis contracts and the value types
// that cross them.
//
// Nothing here knows about ONNX Runtime, HTTP, or storage. That is the point:
// the liveness and enrollment domains depend on this package, and swapping the
// inference engine — or substituting the deterministic stub — must not reach
// them.
package biometric

import "math"

// Point is a coordinate in the source image's pixel space.
//
// Every coordinate that leaves this package is expressed against the original
// image, never against whatever resized copy a model happened to see.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// BBox is an axis-aligned rectangle in the source image's pixel space.
type BBox struct {
	MinX float64 `json:"min_x"`
	MinY float64 `json:"min_y"`
	MaxX float64 `json:"max_x"`
	MaxY float64 `json:"max_y"`
}

// Width returns the box width, or zero if the box is inverted.
func (b BBox) Width() float64 { return math.Max(0, b.MaxX-b.MinX) }

// Height returns the box height, or zero if the box is inverted.
func (b BBox) Height() float64 { return math.Max(0, b.MaxY-b.MinY) }

// Area returns the enclosed area.
func (b BBox) Area() float64 { return b.Width() * b.Height() }

// Center returns the midpoint of the box.
func (b BBox) Center() Point {
	return Point{X: (b.MinX + b.MaxX) / 2, Y: (b.MinY + b.MaxY) / 2}
}

// IoU returns the intersection over union of two boxes, in [0,1].
//
// Non-overlapping and degenerate boxes both give zero, so callers never have to
// guard against a division by zero here.
func (b BBox) IoU(other BBox) float64 {
	iw := math.Min(b.MaxX, other.MaxX) - math.Max(b.MinX, other.MinX)
	ih := math.Min(b.MaxY, other.MaxY) - math.Max(b.MinY, other.MinY)
	if iw <= 0 || ih <= 0 {
		return 0
	}

	intersection := iw * ih
	union := b.Area() + other.Area() - intersection
	if union <= 0 {
		return 0
	}
	return intersection / union
}

// Clip returns the box confined to a width x height image.
//
// A model can predict a box that runs off the edge of the frame — the
// regression is unconstrained — so coordinates are clipped before anything
// downstream tries to crop with them.
func (b BBox) Clip(width, height float64) BBox {
	return BBox{
		MinX: math.Max(0, math.Min(b.MinX, width)),
		MinY: math.Max(0, math.Min(b.MinY, height)),
		MaxX: math.Max(0, math.Min(b.MaxX, width)),
		MaxY: math.Max(0, math.Min(b.MaxY, height)),
	}
}

// Scale returns the box with every coordinate multiplied by f. It is how a
// detection made on a resized image is mapped back to the original.
func (b BBox) Scale(f float64) BBox {
	return BBox{MinX: b.MinX * f, MinY: b.MinY * f, MaxX: b.MaxX * f, MaxY: b.MaxY * f}
}

// Keypoint indices into Keypoints. The order is fixed by the detector and is
// what the face alignment step depends on.
const (
	KeypointLeftEye = iota
	KeypointRightEye
	KeypointNose
	KeypointMouthLeft
	KeypointMouthRight
	KeypointCount
)

// Keypoints are the five coarse landmarks a face detector produces.
//
// "Left" and "right" are from the viewer's side, matching the detector's own
// convention rather than the subject's.
type Keypoints [KeypointCount]Point

// Scale returns the keypoints with every coordinate multiplied by f.
func (k Keypoints) Scale(f float64) Keypoints {
	var out Keypoints
	for i, p := range k {
		out[i] = Point{X: p.X * f, Y: p.Y * f}
	}
	return out
}

// Detection is one face found in an image.
type Detection struct {
	Box       BBox      `json:"box"`
	Keypoints Keypoints `json:"keypoints"`

	// Score is the detector's confidence, in [0,1].
	Score float64 `json:"score"`
}
