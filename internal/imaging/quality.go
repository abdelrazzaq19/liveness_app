package imaging

import (
	"errors"
	"fmt"
	"image"
	"image/draw"
)

// ErrLowQuality means a frame is not worth running a model on.
//
// It is a routine outcome, not a malfunction: the caller is expected to ask the
// client for another frame rather than fail the session.
var ErrLowQuality = errors.New("imaging: frame quality too low")

// Metrics describes how usable a frame is.
type Metrics struct {
	Width  int
	Height int

	// Brightness is the mean luma, in [0,255].
	Brightness float64

	// LaplacianVariance measures edge energy. A sharp frame has a lot of it; a
	// blurred or out-of-focus one has very little. This is the standard cheap
	// blur detector, and it is only meaningful relative to a threshold
	// calibrated for a given camera.
	LaplacianVariance float64
}

// Measure computes the quality metrics of a frame.
func Measure(img image.Image) Metrics {
	gray := toGray(img)
	b := gray.Bounds()
	w, h := b.Dx(), b.Dy()

	m := Metrics{Width: w, Height: h}
	if w == 0 || h == 0 {
		return m
	}

	var sum float64
	for _, v := range gray.Pix {
		sum += float64(v)
	}
	m.Brightness = sum / float64(len(gray.Pix))

	m.LaplacianVariance = laplacianVariance(gray)
	return m
}

// Gate rejects frames that fail the quality bar.
//
// Every threshold arrives from configuration. None of these numbers has been
// measured against a real camera yet, and they are expected to move.
type Gate struct {
	MinLaplacianVariance float64
	MinBrightness        float64
	MaxBrightness        float64

	// MinFaceWidth is the narrowest face, in source pixels, worth analysing.
	// Below the model's own input width the crop is upsampled guesswork.
	MinFaceWidth int
}

// Check reports why a frame is unusable, or nil if it passes.
//
// faceWidth is the detected face width in source pixels; pass zero to skip that
// check, which is what a caller does before a face has been found.
func (g Gate) Check(m Metrics, faceWidth float64) error {
	switch {
	case m.Width == 0 || m.Height == 0:
		return fmt.Errorf("%w: frame has no pixels", ErrLowQuality)

	case m.LaplacianVariance < g.MinLaplacianVariance:
		return fmt.Errorf("%w: blurred (edge variance %.1f, need %.1f)",
			ErrLowQuality, m.LaplacianVariance, g.MinLaplacianVariance)

	case m.Brightness < g.MinBrightness:
		return fmt.Errorf("%w: too dark (mean luma %.1f, need %.1f)",
			ErrLowQuality, m.Brightness, g.MinBrightness)

	case m.Brightness > g.MaxBrightness:
		return fmt.Errorf("%w: too bright (mean luma %.1f, limit %.1f)",
			ErrLowQuality, m.Brightness, g.MaxBrightness)

	case faceWidth > 0 && faceWidth < float64(g.MinFaceWidth):
		return fmt.Errorf("%w: face too small (%.0f px wide, need %d)",
			ErrLowQuality, faceWidth, g.MinFaceWidth)

	default:
		return nil
	}
}

// laplacianVariance convolves with a 4-neighbour Laplacian and returns the
// variance of the response.
//
// The border is skipped rather than padded: a padded edge invents a strong
// artificial gradient all the way around the frame, which inflates the variance
// of exactly the blurred images this is meant to catch.
func laplacianVariance(gray *image.Gray) float64 {
	b := gray.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < 3 || h < 3 {
		return 0
	}

	var sum, sumSq float64
	var n float64

	for y := 1; y < h-1; y++ {
		row := y * gray.Stride
		up := (y - 1) * gray.Stride
		down := (y + 1) * gray.Stride

		for x := 1; x < w-1; x++ {
			v := 4*float64(gray.Pix[row+x]) -
				float64(gray.Pix[row+x-1]) -
				float64(gray.Pix[row+x+1]) -
				float64(gray.Pix[up+x]) -
				float64(gray.Pix[down+x])

			sum += v
			sumSq += v * v
			n++
		}
	}

	if n == 0 {
		return 0
	}
	mean := sum / n
	return sumSq/n - mean*mean
}

// toGray converts to 8-bit grayscale, reusing the input when it already is.
//
// draw.Draw carries the fast paths for the formats that matter here: JPEG
// decodes to YCbCr, whose luma plane is already what we want.
func toGray(img image.Image) *image.Gray {
	if g, ok := img.(*image.Gray); ok {
		return g
	}

	b := img.Bounds()
	g := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(g, g.Bounds(), img, b.Min, draw.Src)
	return g
}
