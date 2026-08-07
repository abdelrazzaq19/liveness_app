package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// makeZip builds an in-memory zip archive from the given entries.
func makeZip(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// serveBlobs starts a test server for the given path -> body map and counts
// how many requests it receives.
func serveBlobs(t *testing.T, routes map[string][]byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// remaining lists the files left in dir, so tests can assert that nothing
// partial survives a failure.
func remaining(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestDownloadInstallsSingleFileAndArchiveMembers(t *testing.T) {
	model := []byte("pretend this is an onnx graph")
	detector := []byte("detector weights")
	embedder := []byte("embedder weights")

	archive := makeZip(t, map[string][]byte{
		// Nested directory on purpose: lookup must fall back to the base name.
		"buffalo_l/det_10g.onnx":   detector,
		"buffalo_l/w600k_r50.onnx": embedder,
		"buffalo_l/unused.txt":     []byte("not requested"),
	})

	srv, hits := serveBlobs(t, map[string][]byte{
		"/solo.onnx":   model,
		"/buffalo.zip": archive,
	})

	dir := t.TempDir()
	m := &Manifest{Version: 1, Artifacts: []Artifact{
		{
			Name: "solo", URL: srv.URL + "/solo.onnx", As: "solo.onnx",
			SHA256: digest(model), License: "test",
		},
		{
			Name: "buffalo_l", URL: srv.URL + "/buffalo.zip",
			SHA256: digest(archive), License: "test",
			Extract: []Member{
				{Path: "det_10g.onnx", As: "det_10g.onnx", SHA256: digest(detector)},
				{Path: "w600k_r50.onnx", As: "w600k_r50.onnx", SHA256: digest(embedder)},
			},
		},
	}}

	if err := m.validate(); err != nil {
		t.Fatalf("manifest is invalid: %v", err)
	}
	if err := ensure(context.Background(), m, dir, srv.Client(), io.Discard); err != nil {
		t.Fatalf("ensure() returned an unexpected error: %v", err)
	}

	for name, want := range map[string][]byte{
		"solo.onnx":      model,
		"det_10g.onnx":   detector,
		"w600k_r50.onnx": embedder,
	} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s was not installed: %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s has the wrong contents", name)
		}
	}

	// The entry we did not ask for must not be installed.
	if _, err := os.Stat(filepath.Join(dir, "unused.txt")); err == nil {
		t.Error("an archive entry that was not requested was installed anyway")
	}

	if err := verify(m, dir, io.Discard); err != nil {
		t.Errorf("verify() failed right after a successful download: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("server received %d requests, want 2", got)
	}
}

// A second run must not re-download anything.
func TestDownloadIsIdempotent(t *testing.T) {
	model := []byte("weights")
	srv, hits := serveBlobs(t, map[string][]byte{"/m.onnx": model})

	dir := t.TempDir()
	m := &Manifest{Version: 1, Artifacts: []Artifact{{
		Name: "m", URL: srv.URL + "/m.onnx", As: "m.onnx",
		SHA256: digest(model), License: "test",
	}}}

	for i := 1; i <= 3; i++ {
		if err := ensure(context.Background(), m, dir, srv.Client(), io.Discard); err != nil {
			t.Fatalf("ensure() run %d returned an unexpected error: %v", i, err)
		}
	}

	if got := hits.Load(); got != 1 {
		t.Errorf("server received %d requests across three runs, want 1", got)
	}
}

// A corrupted download must be discarded, not installed, and must not leave a
// partial file behind that a later run would mistake for a real one.
func TestDownloadRejectsDigestMismatch(t *testing.T) {
	srv, _ := serveBlobs(t, map[string][]byte{"/m.onnx": []byte("tampered payload")})

	dir := t.TempDir()
	m := &Manifest{Version: 1, Artifacts: []Artifact{{
		Name: "m", URL: srv.URL + "/m.onnx", As: "m.onnx",
		SHA256: digest([]byte("the payload we expected")), License: "test",
	}}}

	err := ensure(context.Background(), m, dir, srv.Client(), io.Discard)
	if err == nil {
		t.Fatal("ensure() succeeded on a digest mismatch, want an error")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("error does not explain the mismatch: %v", err)
	}
	if left := remaining(t, dir); len(left) != 0 {
		t.Errorf("files left behind after a rejected download: %v", left)
	}
}

// The same rule applies to a member inside an archive.
func TestDownloadRejectsArchiveMemberDigestMismatch(t *testing.T) {
	good := []byte("real weights")
	archive := makeZip(t, map[string][]byte{"det_10g.onnx": []byte("swapped weights")})

	srv, _ := serveBlobs(t, map[string][]byte{"/a.zip": archive})

	dir := t.TempDir()
	m := &Manifest{Version: 1, Artifacts: []Artifact{{
		Name: "a", URL: srv.URL + "/a.zip", SHA256: digest(archive), License: "test",
		Extract: []Member{{Path: "det_10g.onnx", As: "det_10g.onnx", SHA256: digest(good)}},
	}}}

	if err := ensure(context.Background(), m, dir, srv.Client(), io.Discard); err == nil {
		t.Fatal("ensure() succeeded on a member digest mismatch, want an error")
	}
	if left := remaining(t, dir); len(left) != 0 {
		t.Errorf("files left behind after a rejected extraction: %v", left)
	}
}

// Downloading without a recorded digest would defeat the point of the manifest.
func TestDownloadRefusesWithoutRecordedDigest(t *testing.T) {
	srv, _ := serveBlobs(t, map[string][]byte{"/m.onnx": []byte("weights")})

	dir := t.TempDir()
	m := &Manifest{Version: 1, Artifacts: []Artifact{{
		Name: "m", URL: srv.URL + "/m.onnx", As: "m.onnx", License: "test",
	}}}

	err := ensure(context.Background(), m, dir, srv.Client(), io.Discard)
	if err == nil {
		t.Fatal("ensure() succeeded with no digest recorded, want an error")
	}
	if !strings.Contains(err.Error(), "pin") {
		t.Errorf("error does not point at the pin command: %v", err)
	}
}

func TestDownloadReportsHTTPFailure(t *testing.T) {
	srv, _ := serveBlobs(t, map[string][]byte{})

	dir := t.TempDir()
	m := &Manifest{Version: 1, Artifacts: []Artifact{{
		Name: "m", URL: srv.URL + "/absent.onnx", As: "m.onnx",
		SHA256: digest([]byte("x")), License: "test",
	}}}

	err := ensure(context.Background(), m, dir, srv.Client(), io.Discard)
	if err == nil {
		t.Fatal("ensure() succeeded against a 404, want an error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error does not mention the status code: %v", err)
	}
	if left := remaining(t, dir); len(left) != 0 {
		t.Errorf("files left behind after a failed download: %v", left)
	}
}

// verify must report every problem in one pass, the same way config.Load does.
func TestVerifyReportsEveryProblemAtOnce(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "corrupt.onnx"), []byte("not what was pinned"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	m := &Manifest{Version: 1, Artifacts: []Artifact{
		{Name: "a", URL: "http://x/a", As: "missing.onnx", SHA256: digest([]byte("a")), License: "test"},
		{Name: "b", URL: "http://x/b", As: "corrupt.onnx", SHA256: digest([]byte("b")), License: "test"},
		{Name: "c", URL: "http://x/c", As: "unpinned.onnx", License: "test"},
	}}

	err := verify(m, dir, io.Discard)
	if err == nil {
		t.Fatal("verify() succeeded, want an error")
	}

	msg := err.Error()
	for _, want := range []string{"missing.onnx", "corrupt.onnx", "unpinned.onnx"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %s\nfull error:\n%s", want, msg)
		}
	}
}

func TestPinRecordsObservedDigests(t *testing.T) {
	detector := []byte("detector weights")
	archive := makeZip(t, map[string][]byte{"det_10g.onnx": detector})
	srv, _ := serveBlobs(t, map[string][]byte{"/a.zip": archive})

	dir := t.TempDir()
	m := &Manifest{Version: 1, Artifacts: []Artifact{{
		Name: "a", URL: srv.URL + "/a.zip", License: "test",
		Extract: []Member{{Path: "det_10g.onnx", As: "det_10g.onnx"}},
	}}}

	if err := pin(context.Background(), m, dir, srv.Client(), io.Discard, false); err != nil {
		t.Fatalf("pin() returned an unexpected error: %v", err)
	}

	if got, want := m.Artifacts[0].SHA256, digest(archive); got != want {
		t.Errorf("archive digest = %q, want %q", got, want)
	}
	if got, want := m.Artifacts[0].SizeBytes, int64(len(archive)); got != want {
		t.Errorf("archive size = %d, want %d", got, want)
	}
	if got, want := m.Artifacts[0].Extract[0].SHA256, digest(detector); got != want {
		t.Errorf("member digest = %q, want %q", got, want)
	}

	// The pinned manifest must now satisfy an ordinary verify.
	if err := verify(m, dir, io.Discard); err != nil {
		t.Errorf("verify() failed on a freshly pinned manifest: %v", err)
	}
}

// Adding one entry to the manifest must not mean re-downloading every other
// one; the packs here are hundreds of megabytes each.
func TestPinSkipsArtifactsAlreadyPinned(t *testing.T) {
	body := []byte("weights")
	srv, hits := serveBlobs(t, map[string][]byte{"/m.onnx": body})

	dir := t.TempDir()
	m := &Manifest{Version: 1, Artifacts: []Artifact{{
		Name: "m", URL: srv.URL + "/m.onnx", As: "m.onnx", License: "test",
	}}}

	for i := 1; i <= 3; i++ {
		if err := pin(context.Background(), m, dir, srv.Client(), io.Discard, false); err != nil {
			t.Fatalf("pin() run %d returned an unexpected error: %v", i, err)
		}
	}

	if got := hits.Load(); got != 1 {
		t.Errorf("server received %d requests across three pins, want 1", got)
	}
}

// Forcing is how a manifest is refreshed when upstream replaces a release.
func TestPinForceRefetchesEverything(t *testing.T) {
	body := []byte("weights")
	srv, hits := serveBlobs(t, map[string][]byte{"/m.onnx": body})

	dir := t.TempDir()
	m := &Manifest{Version: 1, Artifacts: []Artifact{{
		Name: "m", URL: srv.URL + "/m.onnx", As: "m.onnx", License: "test",
	}}}

	for i := 1; i <= 2; i++ {
		if err := pin(context.Background(), m, dir, srv.Client(), io.Discard, true); err != nil {
			t.Fatalf("pin() run %d returned an unexpected error: %v", i, err)
		}
	}

	if got := hits.Load(); got != 2 {
		t.Errorf("server received %d requests across two forced pins, want 2", got)
	}
}

func TestManifestRoundTripsThroughDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	want := &Manifest{Version: 1, Artifacts: []Artifact{{
		Name: "a", URL: "https://example.test/a.onnx", As: "a.onnx",
		SHA256: digest([]byte("a")), SizeBytes: 1, License: "test",
	}}}

	if err := saveManifest(path, want); err != nil {
		t.Fatalf("saveManifest() returned an unexpected error: %v", err)
	}
	got, err := loadManifest(path)
	if err != nil {
		t.Fatalf("loadManifest() returned an unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got.Artifacts, want.Artifacts) {
		t.Errorf("round trip changed the artifacts:\n got %+v\nwant %+v", got.Artifacts, want.Artifacts)
	}
}

// Archive entries are attacker-controlled in the general case, so an output
// name that escapes the models directory must be refused outright.
func TestManifestRejectsUnsafeNames(t *testing.T) {
	tests := []struct {
		name     string
		manifest *Manifest
	}{
		{"parent directory in output", &Manifest{Version: 1, Artifacts: []Artifact{
			{Name: "a", URL: "u", License: "l", As: "../escaped.onnx"},
		}}},
		{"absolute output path", &Manifest{Version: 1, Artifacts: []Artifact{
			{Name: "a", URL: "u", License: "l", As: "/etc/passwd"},
		}}},
		{"nested output path", &Manifest{Version: 1, Artifacts: []Artifact{
			{Name: "a", URL: "u", License: "l", As: "sub/dir/a.onnx"},
		}}},
		{"traversal in artifact name", &Manifest{Version: 1, Artifacts: []Artifact{
			{Name: "../a", URL: "u", License: "l", As: "a.onnx"},
		}}},
		{"traversal in archive member", &Manifest{Version: 1, Artifacts: []Artifact{
			{Name: "a", URL: "u", License: "l", Extract: []Member{{Path: "x", As: "../x.onnx"}}},
		}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.manifest.validate(); err == nil {
				t.Error("validate() accepted an unsafe name, want an error")
			}
		})
	}
}

func TestManifestRejectsMalformedEntries(t *testing.T) {
	tests := []struct {
		name     string
		manifest *Manifest
		wantHint string
	}{
		{
			"unsupported version",
			&Manifest{Version: 2, Artifacts: []Artifact{{Name: "a", URL: "u", License: "l", As: "a.onnx"}}},
			"version",
		},
		{
			"no artifacts",
			&Manifest{Version: 1},
			"no artifacts",
		},
		{
			"missing url",
			&Manifest{Version: 1, Artifacts: []Artifact{{Name: "a", License: "l", As: "a.onnx"}}},
			"url",
		},
		{
			"missing licence",
			&Manifest{Version: 1, Artifacts: []Artifact{{Name: "a", URL: "u", As: "a.onnx"}}},
			"license",
		},
		{
			"neither as nor extract",
			&Manifest{Version: 1, Artifacts: []Artifact{{Name: "a", URL: "u", License: "l"}}},
			"extract",
		},
		{
			"both as and extract",
			&Manifest{Version: 1, Artifacts: []Artifact{
				{Name: "a", URL: "u", License: "l", As: "a.onnx", Extract: []Member{{Path: "x", As: "x.onnx"}}},
			}},
			"pick one",
		},
		{
			"two artifacts writing the same file",
			&Manifest{Version: 1, Artifacts: []Artifact{
				{Name: "a", URL: "u", License: "l", As: "same.onnx"},
				{Name: "b", URL: "u", License: "l", As: "same.onnx"},
			}},
			"same.onnx",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.manifest.validate()
			if err == nil {
				t.Fatalf("validate() succeeded, want an error mentioning %q", tt.wantHint)
			}
			if !strings.Contains(err.Error(), tt.wantHint) {
				t.Errorf("error does not mention %q: %v", tt.wantHint, err)
			}
		})
	}
}
