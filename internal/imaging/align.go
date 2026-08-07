package imaging

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

// TemplateSize is the crop size the ArcFace template is defined for.
const TemplateSize = 112

// arcFaceTemplate is the canonical five-point layout ArcFace was trained on,
// expressed in a 112x112 crop.
//
// These constants are not adjustable. An embedding model only produces
// comparable vectors for faces posed the way its training data was, so
// "improving" this layout silently degrades every similarity score in the
// gallery.
var arcFaceTemplate = biometric.Keypoints{
	biometric.KeypointLeftEye:    {X: 38.2946, Y: 51.6963},
	biometric.KeypointRightEye:   {X: 73.5318, Y: 51.5014},
	biometric.KeypointNose:       {X: 56.0252, Y: 71.7366},
	biometric.KeypointMouthLeft:  {X: 41.5493, Y: 92.3655},
	biometric.KeypointMouthRight: {X: 70.7299, Y: 92.2041},
}

// ErrDegenerateLandmarks means the keypoints do not define a transform: they
// are coincident or collinear enough that no scale and rotation can be
// recovered.
var ErrDegenerateLandmarks = errors.New("imaging: landmarks are degenerate")

// Similarity is a 2D similarity transform: uniform scale, rotation, and
// translation, written as
//
//	[ A  -B ] [x]   [Tx]
//	[ B   A ] [y] + [Ty]
//
// Four parameters rather than an affine six. A face crop must not be sheared or
// stretched independently per axis: that changes the geometry the embedding
// model measures.
type Similarity struct {
	A, B   float64
	Tx, Ty float64
}

// Scale returns the uniform scale factor the transform applies.
func (s Similarity) Scale() float64 { return math.Hypot(s.A, s.B) }

// RotationDeg returns the rotation in degrees.
func (s Similarity) RotationDeg() float64 { return math.Atan2(s.B, s.A) * 180 / math.Pi }

// Apply maps a point through the transform.
func (s Similarity) Apply(p biometric.Point) biometric.Point {
	return biometric.Point{
		X: s.A*p.X - s.B*p.Y + s.Tx,
		Y: s.B*p.X + s.A*p.Y + s.Ty,
	}
}

// Invert returns the inverse transform, or an error if this one collapses
// everything to a point.
func (s Similarity) Invert() (Similarity, error) {
	det := s.A*s.A + s.B*s.B
	if det < 1e-12 {
		return Similarity{}, ErrDegenerateLandmarks
	}

	ia := s.A / det
	ib := -s.B / det
	return Similarity{
		A:  ia,
		B:  ib,
		Tx: -(ia*s.Tx - ib*s.Ty),
		Ty: -(ib*s.Tx + ia*s.Ty),
	}, nil
}

// EstimateSimilarity finds the least-squares similarity transform mapping src
// onto dst.
//
// Closed form rather than an iterative fit: with the rotation written as the
// pair (A,B) the problem is linear, so there is an exact answer and no
// convergence to worry about.
func EstimateSimilarity(src, dst biometric.Keypoints) (Similarity, error) {
	var meanSrc, meanDst biometric.Point
	n := float64(len(src))

	for i := range src {
		meanSrc.X += src[i].X
		meanSrc.Y += src[i].Y
		meanDst.X += dst[i].X
		meanDst.Y += dst[i].Y
	}
	meanSrc.X /= n
	meanSrc.Y /= n
	meanDst.X /= n
	meanDst.Y /= n

	var numA, numB, den float64
	for i := range src {
		sx := src[i].X - meanSrc.X
		sy := src[i].Y - meanSrc.Y
		dx := dst[i].X - meanDst.X
		dy := dst[i].Y - meanDst.Y

		numA += sx*dx + sy*dy
		numB += sx*dy - sy*dx
		den += sx*sx + sy*sy
	}

	// den is the spread of the source points. Zero means they are all the same
	// point, and no scale or rotation can be recovered from that.
	if den < 1e-12 {
		return Similarity{}, fmt.Errorf("%w: source points have no spread", ErrDegenerateLandmarks)
	}

	a := numA / den
	b := numB / den
	return Similarity{
		A:  a,
		B:  b,
		Tx: meanDst.X - (a*meanSrc.X - b*meanSrc.Y),
		Ty: meanDst.Y - (b*meanSrc.X + a*meanSrc.Y),
	}, nil
}

// AlignFace warps the face onto the ArcFace template and returns a size x size
// crop.
//
// size is normally TemplateSize; a larger value scales the template up
// proportionally, which keeps the layout identical.
func AlignFace(img image.Image, kps biometric.Keypoints, size int) (*image.RGBA, error) {
	if img == nil {
		return nil, errors.New("imaging: AlignFace: image is nil")
	}
	if size <= 0 {
		return nil, fmt.Errorf("imaging: AlignFace: size must be positive, got %d", size)
	}

	scale := float64(size) / TemplateSize
	target := arcFaceTemplate.Scale(scale)

	forward, err := EstimateSimilarity(kps, target)
	if err != nil {
		return nil, err
	}
	return Warp(img, forward, size, size)
}

// Warp resamples img through a similarity transform into a width x height
// canvas.
//
// forward maps source coordinates to destination ones; the resampling runs the
// other way, from each destination pixel back to where it came from. Going
// forwards would leave unwritten holes wherever the transform magnifies.
func Warp(img image.Image, forward Similarity, width, height int) (*image.RGBA, error) {
	if img == nil {
		return nil, errors.New("imaging: Warp: image is nil")
	}
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("imaging: Warp: size must be positive, got %dx%d", width, height)
	}

	inverse, err := forward.Invert()
	if err != nil {
		return nil, err
	}

	src := toRGBA(img)
	out := image.NewRGBA(image.Rect(0, 0, width, height))

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			p := inverse.Apply(biometric.Point{X: float64(x), Y: float64(y)})
			out.SetRGBA(x, y, sampleBilinear(src, p.X, p.Y))
		}
	}
	return out, nil
}

// sampleBilinear reads a sub-pixel position, clamping at the edges.
//
// Clamping rather than returning black keeps a face that runs slightly off the
// frame from acquiring a hard artificial border, which the embedding model
// would read as real structure.
func sampleBilinear(img *image.RGBA, x, y float64) color.RGBA {
	b := img.Bounds()
	maxX := float64(b.Dx() - 1)
	maxY := float64(b.Dy() - 1)

	x = math.Min(math.Max(x, 0), maxX)
	y = math.Min(math.Max(y, 0), maxY)

	x0 := int(math.Floor(x))
	y0 := int(math.Floor(y))
	x1 := min(x0+1, b.Dx()-1)
	y1 := min(y0+1, b.Dy()-1)

	fx := x - float64(x0)
	fy := y - float64(y0)

	at := func(px, py int) [4]float64 {
		i := img.PixOffset(b.Min.X+px, b.Min.Y+py)
		return [4]float64{
			float64(img.Pix[i]),
			float64(img.Pix[i+1]),
			float64(img.Pix[i+2]),
			float64(img.Pix[i+3]),
		}
	}

	c00, c10 := at(x0, y0), at(x1, y0)
	c01, c11 := at(x0, y1), at(x1, y1)

	var out color.RGBA
	channels := [4]*uint8{&out.R, &out.G, &out.B, &out.A}
	for c := 0; c < 4; c++ {
		top := c00[c]*(1-fx) + c10[c]*fx
		bottom := c01[c]*(1-fx) + c11[c]*fx
		*channels[c] = uint8(math.Round(top*(1-fy) + bottom*fy))
	}
	return out
}

// toRGBA converts to *image.RGBA, reusing the input when it already is.
func toRGBA(img image.Image) *image.RGBA {
	if r, ok := img.(*image.RGBA); ok {
		return r
	}

	b := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)
	return out
}
