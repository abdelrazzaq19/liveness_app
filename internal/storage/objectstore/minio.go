// Package objectstore is the S3-compatible adapter for retained enrollment
// images.
//
// It exists behind enrollment.ArtifactStore so that the domain never learns
// there is an object store at all, let alone which one.
package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/ziad/liveness-verifier/internal/config"
	"github.com/ziad/liveness-verifier/internal/enrollment"
)

// maxArtifactBytes caps what Get will read into memory.
//
// The objects this stores are aligned 112x112 crops, a few kilobytes each. The
// cap is not about them: it is about what happens when something else ends up
// in the bucket, because an unbounded read of an attacker-chosen object is how
// a single request exhausts the process.
const maxArtifactBytes = 8 << 20

// Store is the MinIO-backed artifact store.
type Store struct {
	client *minio.Client
	bucket string
}

var _ enrollment.ArtifactStore = (*Store)(nil)

// Open connects and makes sure the bucket exists.
//
// Connecting eagerly rather than on first use: a misconfigured endpoint or a
// wrong key should stop the process at boot, where somebody is watching, not on
// the first enrollment of the day.
func Open(ctx context.Context, cfg config.ObjectStore) (*Store, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("objectstore: endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("objectstore: bucket is required")
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey.Reveal(), cfg.SecretKey.Reveal(), ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		// The credentials are not in the message. config.Secret redacts itself
		// when printed, but an error built by hand can still leak one.
		return nil, fmt.Errorf("objectstore: connect to %s: %w", cfg.Endpoint, err)
	}

	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("objectstore: check bucket %s: %w", cfg.Bucket, err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
			return nil, fmt.Errorf("objectstore: create bucket %s: %w", cfg.Bucket, err)
		}
	}

	return &Store{client: client, bucket: cfg.Bucket}, nil
}

// Put stores the bytes under key.
func (s *Store) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if key == "" {
		return errors.New("objectstore: key is required")
	}
	if len(data) == 0 {
		return errors.New("objectstore: refusing to store an empty object")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	_, err := s.client.PutObject(ctx, s.bucket, key,
		bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("objectstore: put %s: %w", key, err)
	}
	return nil
}

// Get returns the object.
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	if key == "" {
		return nil, errors.New("objectstore: key is required")
	}

	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("objectstore: get %s: %w", key, err)
	}
	defer func() { _ = obj.Close() }()

	// Bounded by one byte more than the cap, so an object sitting exactly at the
	// limit is still distinguishable from one that overruns it.
	data, err := io.ReadAll(io.LimitReader(obj, maxArtifactBytes+1))
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", enrollment.ErrArtifactNotFound, key)
		}
		return nil, fmt.Errorf("objectstore: read %s: %w", key, err)
	}
	if len(data) > maxArtifactBytes {
		return nil, fmt.Errorf("objectstore: %s is larger than %d bytes", key, maxArtifactBytes)
	}
	return data, nil
}

// Delete removes the object.
//
// Removing something that is not there is not an error. The caller wanted it
// gone; it is gone. Reporting a failure would make a retry after a partial
// delete look like a new problem.
func (s *Store) Delete(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("objectstore: key is required")
	}

	err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("objectstore: delete %s: %w", key, err)
	}
	return nil
}

// isNotFound recognises the several shapes a missing object arrives in.
//
// The client reports it as a typed response for some operations and as a
// deferred error on the read for others, so checking one alone misses the case
// where a Get succeeds and the absence only surfaces when the body is read.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}

	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp.StatusCode == http.StatusNotFound ||
			resp.Code == "NoSuchKey" ||
			resp.Code == "NoSuchBucket"
	}
	return false
}
