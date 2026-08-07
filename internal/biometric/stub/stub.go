// Package stub provides a deterministic stand-in for the biometric pipeline.
//
// It exists so that the service, its HTTP layer, its storage, and its whole
// test suite can run on a checkout with an empty models directory. That is not
// a convenience: a test suite that only runs where 190 MB of model files happen
// to be present is a test suite that stops being run.
//
// What it produces is deterministic and content-sensitive, and it measures
// nothing about faces. The numbers are crude proxies over image statistics,
// chosen so that a person in front of a webcam can still drive the demo — dim
// the top of the frame and the blink metric drops, lean to one side and the yaw
// follows. Do not read anything into a stub session beyond "the plumbing
// works".
package stub

import (
	"context"
	"image"
	"math"

	xdraw "golang.org/x/image/draw"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

// gridSize is the resolution every statistic is computed at.
//
// Small on purpose: it makes the stub cheap, and it makes the derived
// embedding tolerant of camera noise the way a real one is, so that the
// identity-consistency check has something meaningful to work against.
const gridSize = 16

// Pipeline is a deterministic fake biometric pipeline.
//
// The zero value is usable.
type Pipeline struct {
	// FaceFraction is how much of the frame the reported face box covers.
	// Zero means the default.
	FaceFraction float64
}

var _ biometric.Analyzer = (*Pipeline)(nil)

// Analyze returns a Face derived entirely from the frame's pixels.
//
// The same image always produces the same Face, and similar images produce
// similar embeddings, which is what makes downstream behaviour reproducible in
// a test.
func (p *Pipeline) Analyze(_ context.Context, img image.Image, opts biometric.AnalyzeOptions) (biometric.Face, error) {
	var face biometric.Face

	if img == nil {
		return face, biometric.ErrNoFaceFound
	}
	bounds := img.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return face, biometric.ErrNoFaceFound
	}

	grid := downsample(img)

	face.Quality = biometric.Quality{
		Width:      bounds.Dx(),
		Height:     bounds.Dy(),
		Brightness: grid.mean(),
		Sharpness:  grid.variance(),
	}

	face.Box = p.box(bounds)
	face.Score = 0.99
	face.Keypoints = keypointsFor(face.Box)
	face.Landmarks = landmarksFor(face.Box)

	// Crude proxies, chosen so a human can steer them. The eye band darkening
	// stands in for a blink; the horizontal brightness centroid stands in for
	// turning the head.
	face.EAR = lerp(0.05, 0.45, grid.bandMean(0.15, 0.45)/255)
	face.MAR = lerp(0.0, 1.0, grid.bandVariance(0.6, 0.9)/4000)
	face.Pose = biometric.Pose{
		Yaw:   lerp(-45, 45, grid.horizontalCentroid()),
		Pitch: lerp(-30, 30, grid.verticalCentroid()),
		Roll:  lerp(-20, 20, grid.diagonalBalance()),
	}

	// A flat, evenly lit frame scores low, the way a printed photograph tends
	// to. It is the right direction for the wrong reason, and it is only here
	// so the wiring can be exercised.
	face.LivenessScore = clamp(grid.variance()/1500, 0, 1)

	if !opts.SkipEmbedding {
		face.Embedding = grid.embedding()
	}
	return face, nil
}

func (p *Pipeline) box(bounds image.Rectangle) biometric.BBox {
	fraction := p.FaceFraction
	if fraction <= 0 || fraction > 1 {
		fraction = 0.5
	}

	w := float64(bounds.Dx())
	h := float64(bounds.Dy())
	side := math.Min(w, h) * fraction

	cx := w / 2
	cy := h / 2
	return biometric.BBox{
		MinX: cx - side/2,
		MinY: cy - side/2,
		MaxX: cx + side/2,
		MaxY: cy + side/2,
	}
}

// keypointsFor places the five coarse landmarks at fixed fractions of the box,
// matching roughly where a frontal face has them.
func keypointsFor(b biometric.BBox) biometric.Keypoints {
	w := b.Width()
	h := b.Height()

	var k biometric.Keypoints
	k[biometric.KeypointLeftEye] = biometric.Point{X: b.MinX + w*0.34, Y: b.MinY + h*0.40}
	k[biometric.KeypointRightEye] = biometric.Point{X: b.MinX + w*0.66, Y: b.MinY + h*0.40}
	k[biometric.KeypointNose] = biometric.Point{X: b.MinX + w*0.50, Y: b.MinY + h*0.58}
	k[biometric.KeypointMouthLeft] = biometric.Point{X: b.MinX + w*0.38, Y: b.MinY + h*0.75}
	k[biometric.KeypointMouthRight] = biometric.Point{X: b.MinX + w*0.62, Y: b.MinY + h*0.75}
	return k
}

// landmarksFor lays out 106 points inside the box.
//
// The groups land where the index map says they should — eyes above the mouth,
// contour outermost — so that code reading them by index gets something
// structurally sane rather than a cloud of points.
func landmarksFor(b biometric.BBox) biometric.Landmarks106 {
	w := b.Width()
	h := b.Height()

	at := func(fx, fy float64) biometric.Point {
		return biometric.Point{X: b.MinX + w*fx, Y: b.MinY + h*fy}
	}

	var l biometric.Landmarks106

	for i := biometric.ContourFirst; i <= biometric.ContourLast; i++ {
		t := float64(i-biometric.ContourFirst) / float64(biometric.ContourLast-biometric.ContourFirst)
		angle := math.Pi * (0.15 + 0.7*t)
		l[i] = at(0.5-0.48*math.Cos(angle), 0.45+0.5*math.Sin(angle))
	}

	eye := func(first int, cx float64) {
		for i := first; i <= first+9; i++ {
			t := float64(i-first) / 9
			l[i] = at(cx-0.08+0.16*t, 0.40+0.02*math.Sin(2*math.Pi*t))
		}
	}
	eye(biometric.LeftEyeFirst, 0.34)
	eye(biometric.RightEyeFirst, 0.66)

	for i := biometric.LeftBrowFirst; i <= biometric.LeftBrowLast; i++ {
		t := float64(i-biometric.LeftBrowFirst) / float64(biometric.LeftBrowLast-biometric.LeftBrowFirst)
		l[i] = at(0.24+0.20*t, 0.31)
	}
	for i := biometric.RightBrowFirst; i <= biometric.RightBrowLast; i++ {
		t := float64(i-biometric.RightBrowFirst) / float64(biometric.RightBrowLast-biometric.RightBrowFirst)
		l[i] = at(0.56+0.20*t, 0.31)
	}
	for i := biometric.NoseFirst; i <= biometric.NoseLast; i++ {
		t := float64(i-biometric.NoseFirst) / float64(biometric.NoseLast-biometric.NoseFirst)
		l[i] = at(0.5+0.06*math.Sin(2*math.Pi*t), 0.46+0.16*t)
	}
	for i := biometric.MouthFirst; i <= biometric.MouthLast; i++ {
		t := float64(i-biometric.MouthFirst) / float64(biometric.MouthLast-biometric.MouthFirst)
		l[i] = at(0.38+0.24*t, 0.75+0.03*math.Sin(2*math.Pi*t))
	}
	return l
}

// lumaGrid is a small greyscale summary of a frame.
type lumaGrid [gridSize * gridSize]float64

func downsample(img image.Image) lumaGrid {
	small := image.NewGray(image.Rect(0, 0, gridSize, gridSize))
	xdraw.ApproxBiLinear.Scale(small, small.Bounds(), img, img.Bounds(), xdraw.Src, nil)

	var g lumaGrid
	for i, v := range small.Pix {
		g[i] = float64(v)
	}
	return g
}

func (g lumaGrid) mean() float64 {
	var sum float64
	for _, v := range g {
		sum += v
	}
	return sum / float64(len(g))
}

func (g lumaGrid) variance() float64 {
	mean := g.mean()

	var sum float64
	for _, v := range g {
		d := v - mean
		sum += d * d
	}
	return sum / float64(len(g))
}

// bandMean averages a horizontal band of the frame, given as fractions of its
// height.
func (g lumaGrid) bandMean(from, to float64) float64 {
	y0 := clampInt(int(from*gridSize), 0, gridSize-1)
	y1 := clampInt(int(to*gridSize), y0+1, gridSize)

	var sum float64
	var n int
	for y := y0; y < y1; y++ {
		for x := 0; x < gridSize; x++ {
			sum += g[y*gridSize+x]
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

func (g lumaGrid) bandVariance(from, to float64) float64 {
	mean := g.bandMean(from, to)

	y0 := clampInt(int(from*gridSize), 0, gridSize-1)
	y1 := clampInt(int(to*gridSize), y0+1, gridSize)

	var sum float64
	var n int
	for y := y0; y < y1; y++ {
		for x := 0; x < gridSize; x++ {
			d := g[y*gridSize+x] - mean
			sum += d * d
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// horizontalCentroid returns where the frame's brightness sits left to right,
// as a fraction in [0,1]. A uniform frame gives 0.5.
func (g lumaGrid) horizontalCentroid() float64 {
	var weighted, total float64
	for y := 0; y < gridSize; y++ {
		for x := 0; x < gridSize; x++ {
			v := g[y*gridSize+x]
			weighted += v * float64(x)
			total += v
		}
	}
	if total == 0 {
		return 0.5
	}
	return weighted / total / float64(gridSize-1)
}

func (g lumaGrid) verticalCentroid() float64 {
	var weighted, total float64
	for y := 0; y < gridSize; y++ {
		for x := 0; x < gridSize; x++ {
			v := g[y*gridSize+x]
			weighted += v * float64(y)
			total += v
		}
	}
	if total == 0 {
		return 0.5
	}
	return weighted / total / float64(gridSize-1)
}

// diagonalBalance compares the two diagonal halves, giving something that moves
// when the frame is tilted.
func (g lumaGrid) diagonalBalance() float64 {
	var upper, lower float64
	for y := 0; y < gridSize; y++ {
		for x := 0; x < gridSize; x++ {
			if x > y {
				upper += g[y*gridSize+x]
			} else if x < y {
				lower += g[y*gridSize+x]
			}
		}
	}
	total := upper + lower
	if total == 0 {
		return 0.5
	}
	return upper / total
}

// embedding projects the grid into a normalised descriptor.
//
// Built from the grid rather than from a hash, so that the property the
// identity checks rely on holds: two frames of the same scene land close
// together, two different scenes do not. A hash would give the opposite —
// maximally different vectors for nearly identical frames.
func (g lumaGrid) embedding() biometric.Embedding {
	out := make(biometric.Embedding, biometric.EmbeddingDim)

	for i := range out {
		// Each output dimension is a fixed projection of the grid. The
		// coefficients are deterministic and spread across frequencies so that
		// different scenes separate.
		//
		// The constant offset on each sample matters: without it a completely
		// dark frame — a covered lens — projects to the zero vector, which has
		// no direction and fails validation. With it, such a frame still yields
		// a well-defined descriptor that simply carries no information.
		var sum float64
		for j, v := range g {
			sum += (v + 1) * math.Cos(float64((i+1)*(j+1))*0.017)
		}
		out[i] = float32(sum)
	}
	return out.Normalize()
}

func lerp(lo, hi, t float64) float64 { return lo + (hi-lo)*clamp(t, 0, 1) }

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
