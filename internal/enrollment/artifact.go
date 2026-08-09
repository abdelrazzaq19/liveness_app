package enrollment

import (
	"context"
	"errors"
)

// ErrArtifactNotFound means no object exists under that key.
var ErrArtifactNotFound = errors.New("enrollment: artifact not found")

// ArtifactStore keeps the images an enrollment chose to retain.
//
// Retention is optional and off by default. A stored face crop is biometric
// data in its most directly usable form — an embedding at least needs a model
// to interpret it, whereas a photograph needs nothing — so keeping one is a
// decision an operator makes deliberately, not a side effect of enrolling.
//
// Keys are opaque. Nothing derived from a subject goes into them: an object
// store key appears in access logs, bucket listings, and error messages, and a
// key like "faces/budi-santoso.jpg" leaks who is enrolled to anyone who can see
// any of those, without ever reading the object.
type ArtifactStore interface {
	// Put stores the bytes under key, replacing whatever was there.
	Put(ctx context.Context, key string, data []byte, contentType string) error

	// Get returns the object, or ErrArtifactNotFound.
	Get(ctx context.Context, key string) ([]byte, error)

	// Delete removes the object. Deleting something that is not there is not an
	// error: the caller wanted it gone, and it is.
	Delete(ctx context.Context, key string) error
}

// ArtifactKeyFor builds the storage key for one enrolled face.
//
// Derived from the face id, which is random, rather than from the subject.
// The gallery row already ties the two together, and it is behind an API key;
// the object store key is not.
func ArtifactKeyFor(id FaceID) string {
	return "faces/" + id.String() + ".jpg"
}
