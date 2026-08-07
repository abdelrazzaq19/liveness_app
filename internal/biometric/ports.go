package biometric

import (
	"context"
	"errors"
	"image"
)

// Sentinel errors that callers are expected to branch on.
//
// The distinction between them matters upstream: a frame that produced no
// usable face is a reason to ask the client for another one, whereas an
// unclassified error is a reason to fail the request.
var (
	// ErrNoFaceFound means the image contains no face the detector is
	// confident about. It is an ordinary outcome, not a malfunction.
	ErrNoFaceFound = errors.New("biometric: no face found")

	// ErrLowQuality means a face was found but the frame is not good enough to
	// analyse: too blurry, too dark, or the face is too small.
	ErrLowQuality = errors.New("biometric: frame quality too low")
)

// Detector finds the most prominent face in an image.
//
// Implementations return ErrNoFaceFound rather than a zero Detection when
// nothing is found, so a caller cannot mistake an empty box for a real one.
type Detector interface {
	Detect(ctx context.Context, img image.Image) (Detection, error)
}
