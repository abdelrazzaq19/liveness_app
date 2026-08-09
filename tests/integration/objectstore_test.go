//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ziad/liveness-verifier/internal/config"
	"github.com/ziad/liveness-verifier/internal/enrollment"
	"github.com/ziad/liveness-verifier/internal/storage/objectstore"
)

// newArtifactStore connects to the compose MinIO, skipping when it is not up
// rather than failing: someone running the suite without the stack should not
// see red for a service they never started.
func newArtifactStore(t *testing.T) *objectstore.Store {
	t.Helper()

	endpoint := os.Getenv("LV_OBJSTORE_ENDPOINT")
	if endpoint == "" {
		t.Skip("LV_OBJSTORE_ENDPOINT is not set; run: docker compose up -d minio")
	}

	cfg := config.ObjectStore{
		Endpoint:  endpoint,
		AccessKey: config.Secret(os.Getenv("LV_OBJSTORE_ACCESS_KEY")),
		SecretKey: config.Secret(os.Getenv("LV_OBJSTORE_SECRET_KEY")),
		Bucket:    "liveness-test-artifacts",
		Region:    "us-east-1",
	}

	store, err := objectstore.Open(context.Background(), cfg)
	if err != nil {
		t.Skipf("minio is not reachable: %v", err)
	}
	return store
}

func TestArtifactRoundTrip(t *testing.T) {
	store := newArtifactStore(t)
	ctx := context.Background()

	key := enrollment.ArtifactKeyFor("face-roundtrip")
	want := []byte("\xff\xd8\xff\xe0 not really a jpeg, but bytes are bytes")

	if err := store.Put(ctx, key, want, "image/jpeg"); err != nil {
		t.Fatalf("Put() returned an unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, key) })

	got, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get() returned an unexpected error: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %d bytes, want %d; the object changed in storage", len(got), len(want))
	}
}

// A key must not carry anything about the subject. Keys turn up in access logs,
// bucket listings, and error messages, and one like "faces/budi-santoso.jpg"
// tells anyone who can see any of those who is enrolled, without their ever
// reading the object.
func TestArtifactKeysCarryNothingPersonal(t *testing.T) {
	key := enrollment.ArtifactKeyFor("9f2c1ab7e4d3")

	if !strings.HasPrefix(key, "faces/") {
		t.Errorf("key %q is not under the faces prefix", key)
	}
	if !strings.Contains(key, "9f2c1ab7e4d3") {
		t.Errorf("key %q does not identify the face row it belongs to", key)
	}

	// The face id is random, so anything else in the key would have to have
	// come from the subject.
	rest := strings.TrimSuffix(strings.TrimPrefix(key, "faces/"), ".jpg")
	if rest != "9f2c1ab7e4d3" {
		t.Errorf("key carries %q beyond the face id", rest)
	}
}

func TestGetMissingArtifactIsRecognisable(t *testing.T) {
	store := newArtifactStore(t)

	_, err := store.Get(context.Background(), enrollment.ArtifactKeyFor("never-stored"))
	if err == nil {
		t.Fatal("Get() succeeded for an object that was never stored")
	}
	if !errors.Is(err, enrollment.ErrArtifactNotFound) {
		t.Errorf("error = %v, want ErrArtifactNotFound so a caller can tell a missing image from a broken store", err)
	}
}

// Deleting something that is not there is not an error: the caller wanted it
// gone, and it is. Reporting a failure would make a retry after a partial
// delete look like a new problem.
func TestDeleteIsIdempotent(t *testing.T) {
	store := newArtifactStore(t)
	ctx := context.Background()

	key := enrollment.ArtifactKeyFor("face-delete-twice")
	if err := store.Put(ctx, key, []byte("some bytes"), "image/jpeg"); err != nil {
		t.Fatalf("Put() returned an unexpected error: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := store.Delete(ctx, key); err != nil {
			t.Errorf("Delete() attempt %d returned an error: %v", i+1, err)
		}
	}

	if _, err := store.Get(ctx, key); !errors.Is(err, enrollment.ErrArtifactNotFound) {
		t.Errorf("the object survived deletion: %v", err)
	}
}

func TestPutRejectsNothingToStore(t *testing.T) {
	store := newArtifactStore(t)
	ctx := context.Background()

	tests := []struct {
		name string
		key  string
		data []byte
	}{
		{"no key", "", []byte("bytes")},
		{"no data", enrollment.ArtifactKeyFor("face-empty"), nil},
		{"empty data", enrollment.ArtifactKeyFor("face-empty"), []byte{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := store.Put(ctx, tt.key, tt.data, "image/jpeg"); err == nil {
				t.Error("Put() accepted a request with nothing to store")
			}
		})
	}
}

// The bucket has to be created when it is missing, or a fresh deployment fails
// on its first enrollment instead of at boot.
func TestOpenCreatesTheBucket(t *testing.T) {
	endpoint := os.Getenv("LV_OBJSTORE_ENDPOINT")
	if endpoint == "" {
		t.Skip("LV_OBJSTORE_ENDPOINT is not set")
	}

	bucket := fmt.Sprintf("liveness-test-fresh-%d", os.Getpid())

	cfg := config.ObjectStore{
		Endpoint:  endpoint,
		AccessKey: config.Secret(os.Getenv("LV_OBJSTORE_ACCESS_KEY")),
		SecretKey: config.Secret(os.Getenv("LV_OBJSTORE_SECRET_KEY")),
		Bucket:    bucket,
		Region:    "us-east-1",
	}

	store, err := objectstore.Open(context.Background(), cfg)
	if err != nil {
		t.Skipf("minio is not reachable: %v", err)
	}

	// Reopening must not fail on the bucket already existing.
	if _, err := objectstore.Open(context.Background(), cfg); err != nil {
		t.Errorf("reopening an existing bucket failed: %v", err)
	}

	key := "faces/probe.jpg"
	if err := store.Put(context.Background(), key, []byte("bytes"), "image/jpeg"); err != nil {
		t.Errorf("the freshly created bucket is not writable: %v", err)
	}
	_ = store.Delete(context.Background(), key)
}

// Credentials must not reach an error message. config.Secret redacts itself
// when printed, but an error assembled by hand can still carry one.
func TestConnectionErrorsDoNotLeakCredentials(t *testing.T) {
	cfg := config.ObjectStore{
		Endpoint:  "127.0.0.1:1",
		AccessKey: config.Secret("access-key-that-must-not-appear"),
		SecretKey: config.Secret("secret-key-that-must-not-appear"),
		Bucket:    "whatever",
		Region:    "us-east-1",
	}

	_, err := objectstore.Open(context.Background(), cfg)
	if err == nil {
		t.Skip("something is listening on port 1; cannot test the failure path")
	}

	for _, secret := range []string{"access-key-that-must-not-appear", "secret-key-that-must-not-appear"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("a credential appears in the error: %v", err)
		}
	}
}
