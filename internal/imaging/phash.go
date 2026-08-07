package imaging

import (
	"image"
	"math"
	"math/bits"
	"sort"

	xdraw "golang.org/x/image/draw"
)

// phashSize is the working resolution the DCT runs on.
const phashSize = 32

// phashLowFreq is the side of the low-frequency square kept from the DCT.
// 8x8 gives 64 coefficients, one of which is discarded, so the hash is 63
// meaningful bits in a 64-bit word.
const phashLowFreq = 8

// dctBasis caches the cosine terms. The DCT here is the naive O(n^3) form,
// which at n=32 is a few hundred thousand multiplies — far cheaper than the
// inference it guards.
var dctBasis = func() [phashSize][phashSize]float64 {
	var basis [phashSize][phashSize]float64
	for u := 0; u < phashSize; u++ {
		for x := 0; x < phashSize; x++ {
			basis[u][x] = math.Cos(float64(2*x+1) * float64(u) * math.Pi / (2 * phashSize))
		}
	}
	return basis
}()

// PHash returns a 64-bit perceptual hash of an image.
//
// Two frames of the same static scene hash to nearly the same value; two
// different scenes do not. That is what makes it useful for spotting a replayed
// still image, where an attacker holds one photograph in front of the camera
// and every frame is effectively identical.
//
// It is deliberately insensitive to scale, brightness, and compression noise —
// the things that differ between two captures of the same thing.
func PHash(img image.Image) uint64 {
	small := image.NewGray(image.Rect(0, 0, phashSize, phashSize))
	xdraw.BiLinear.Scale(small, small.Bounds(), img, img.Bounds(), xdraw.Src, nil)

	pixels := make([]float64, phashSize*phashSize)
	for i, v := range small.Pix {
		pixels[i] = float64(v)
	}

	coeffs := dct2D(pixels)

	// Skip the DC term: it encodes overall brightness, which is exactly the
	// difference we want the hash to ignore.
	values := make([]float64, 0, phashLowFreq*phashLowFreq-1)
	for u := 0; u < phashLowFreq; u++ {
		for v := 0; v < phashLowFreq; v++ {
			if u == 0 && v == 0 {
				continue
			}
			values = append(values, coeffs[u*phashSize+v])
		}
	}

	median := medianOf(values)

	var hash uint64
	bit := 0
	for u := 0; u < phashLowFreq; u++ {
		for v := 0; v < phashLowFreq; v++ {
			if u == 0 && v == 0 {
				continue
			}
			if coeffs[u*phashSize+v] > median {
				hash |= 1 << bit
			}
			bit++
		}
	}
	return hash
}

// HammingDistance counts the differing bits of two hashes.
//
// Small means similar. Two consecutive frames of a static scene land within a
// handful of bits; unrelated images sit near 32, which is what random noise
// gives on 64 bits.
func HammingDistance(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

// dct2D applies a separable 2D DCT-II to a phashSize square.
func dct2D(in []float64) []float64 {
	rows := make([]float64, phashSize*phashSize)
	for y := 0; y < phashSize; y++ {
		for u := 0; u < phashSize; u++ {
			var sum float64
			for x := 0; x < phashSize; x++ {
				sum += in[y*phashSize+x] * dctBasis[u][x]
			}
			rows[y*phashSize+u] = sum
		}
	}

	out := make([]float64, phashSize*phashSize)
	for u := 0; u < phashSize; u++ {
		for v := 0; v < phashSize; v++ {
			var sum float64
			for y := 0; y < phashSize; y++ {
				sum += rows[y*phashSize+v] * dctBasis[u][y]
			}
			out[u*phashSize+v] = sum
		}
	}
	return out
}

func medianOf(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}

	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)

	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}
