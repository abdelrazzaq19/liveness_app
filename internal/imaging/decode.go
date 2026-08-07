// Package imaging holds the pixel-level work between an uploaded frame and a
// model: decoding, quality gating, face alignment, and perceptual hashing.
//
// Nothing here knows about ONNX, HTTP, or storage. Every function is
// deterministic and testable without a model file.
package imaging

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"io"

	// Registered for their decoders; the format is chosen from the header.
	_ "image/jpeg"
	_ "image/png"
)

// Errors callers are expected to branch on.
var (
	// ErrTooLarge means the encoded image exceeded the byte limit.
	ErrTooLarge = errors.New("imaging: encoded image exceeds the size limit")

	// ErrTooManyPixels means the declared dimensions exceeded the pixel limit.
	ErrTooManyPixels = errors.New("imaging: image dimensions exceed the pixel limit")

	// ErrUnsupportedFormat means the data is not JPEG or PNG.
	ErrUnsupportedFormat = errors.New("imaging: unsupported image format")

	// ErrCorrupt means the header parsed but the pixel data did not.
	ErrCorrupt = errors.New("imaging: image data is corrupt")
)

// Limits bound what Decode will accept.
//
// Both limits exist because they stop different attacks. A byte limit alone
// does not help: a few kilobytes of PNG can declare a 50000x50000 canvas, and
// decoding that is how a service runs out of memory.
type Limits struct {
	// MaxBytes caps the encoded size.
	MaxBytes int64

	// MaxPixels caps width x height, checked from the header before any pixel
	// data is decoded.
	MaxPixels int
}

// Decode reads a JPEG or PNG, enforcing limits and applying EXIF orientation.
//
// It never panics on malformed input: a corrupt file is an error, and the
// decoders in the standard library are fuzzed against exactly this.
func Decode(r io.Reader, limits Limits) (image.Image, error) {
	if limits.MaxBytes <= 0 {
		return nil, errors.New("imaging: MaxBytes must be positive")
	}
	if limits.MaxPixels <= 0 {
		return nil, errors.New("imaging: MaxPixels must be positive")
	}

	// Read one byte past the limit: enough to know the input is too large,
	// without buffering however much more the caller intended to send.
	raw, err := io.ReadAll(io.LimitReader(r, limits.MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("imaging: read: %w", err)
	}
	if int64(len(raw)) > limits.MaxBytes {
		return nil, fmt.Errorf("%w: over %d bytes", ErrTooLarge, limits.MaxBytes)
	}

	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedFormat, err)
	}
	if format != "jpeg" && format != "png" {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedFormat, format)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("%w: header declares %dx%d", ErrCorrupt, cfg.Width, cfg.Height)
	}
	if int64(cfg.Width)*int64(cfg.Height) > int64(limits.MaxPixels) {
		return nil, fmt.Errorf("%w: %dx%d is %d pixels, limit %d",
			ErrTooManyPixels, cfg.Width, cfg.Height, cfg.Width*cfg.Height, limits.MaxPixels)
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}

	// Only JPEG carries EXIF. A phone camera writes the sensor orientation
	// there rather than rotating the pixels, so a portrait selfie arrives
	// sideways unless this is applied.
	if format == "jpeg" {
		img = ApplyOrientation(img, exifOrientation(raw))
	}
	return img, nil
}
