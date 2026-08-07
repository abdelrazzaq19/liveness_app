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

// Landmarker refines a detected face into dense landmarks.
//
// It takes the detector's box rather than finding the face itself: the crop it
// needs is defined relative to that box, and re-detecting would be both slower
// and liable to disagree with the detection the rest of the pipeline is using.
//
// Coordinates come back in the source image's pixel space, never in the crop's.
type Landmarker interface {
	Landmarks(ctx context.Context, img image.Image, box BBox) (Landmarks106, error)
}

// AntiSpoofer scores how likely a detected face is to be a live person rather
// than a photograph, a screen, or a mask.
//
// This is the passive check: it looks at one frame and asks whether the texture
// is that of a face or of a reproduction of one. It complements the
// challenge-response flow rather than replacing it — a print attack defeats
// challenges by being held still, and a replayed video defeats texture analysis
// by being a real recording.
type AntiSpoofer interface {
	// LivenessScore returns a value in [0,1]. Higher means more likely live.
	LivenessScore(ctx context.Context, img image.Image, box BBox) (float64, error)
}
