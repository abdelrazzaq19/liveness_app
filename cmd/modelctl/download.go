package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// tmpSuffix marks a partially written file. Nothing is ever written straight to
// its final name: an interrupted download would otherwise leave a truncated
// file that looks present but is not.
const tmpSuffix = ".part"

// observed is what a download actually produced, as opposed to what the
// manifest claimed it would.
type observed struct {
	ArchiveSHA  string
	ArchiveSize int64
	Members     map[string]string // output file name -> sha256
}

// ensure downloads whatever is missing or does not match the manifest, and
// leaves everything else alone.
func ensure(ctx context.Context, m *Manifest, dir string, client *http.Client, out io.Writer) error {
	for _, a := range m.Artifacts {
		if satisfied(a, dir) {
			fmt.Fprintf(out, "ok       %s (already present)\n", a.Name)
			continue
		}

		// Nothing to fetch for a locally built artifact: say what produces it
		// rather than failing on a missing URL.
		if a.Build != nil {
			return fmt.Errorf("artifact %s must be built locally; run:\n\n    %s\n\nthen: modelctl pin",
				a.Name, a.Build.Command)
		}

		fmt.Fprintf(out, "fetching %s\n         %s\n", a.Name, a.URL)
		if _, err := fetchArtifact(ctx, a, dir, client, true); err != nil {
			return fmt.Errorf("artifact %s: %w", a.Name, err)
		}

		for _, o := range a.outputs() {
			fmt.Fprintf(out, "written  %s\n", o.Name)
		}
		fmt.Fprintf(out, "license  %s: %s\n", a.Name, a.License)
	}
	return nil
}

// verify checks what is on disk against the manifest without touching the
// network. It reports every problem it finds rather than stopping at the first.
func verify(m *Manifest, dir string, out io.Writer) error {
	var problems []error

	for _, a := range m.Artifacts {
		for _, o := range a.outputs() {
			if o.SHA256 == "" {
				problems = append(problems, fmt.Errorf("%s: no digest recorded in the manifest; run `modelctl pin`", o.Name))
				continue
			}

			sum, size, err := fileSHA256(filepath.Join(dir, o.Name))
			switch {
			case errors.Is(err, os.ErrNotExist):
				problems = append(problems, fmt.Errorf("%s: missing", o.Name))
			case err != nil:
				problems = append(problems, fmt.Errorf("%s: %w", o.Name, err))
			case !strings.EqualFold(sum, o.SHA256):
				problems = append(problems, fmt.Errorf("%s: digest mismatch (on disk %s, manifest %s)", o.Name, sum, o.SHA256))
			default:
				fmt.Fprintf(out, "ok       %s (%s)\n", o.Name, humanSize(size))
			}
		}
	}

	return errors.Join(problems...)
}

// pin downloads artifacts and records the digests it observes, so that a
// manifest written without known-good hashes can bootstrap itself.
//
// Artifacts that are already pinned and present are skipped unless force is
// set. Adding one entry to the manifest should not mean re-downloading every
// other one.
func pin(ctx context.Context, m *Manifest, dir string, client *http.Client, out io.Writer, force bool) error {
	for i := range m.Artifacts {
		a := m.Artifacts[i]

		if !force && a.SHA256 != "" && satisfied(a, dir) {
			fmt.Fprintf(out, "ok       %s (already pinned)\n", a.Name)
			continue
		}

		// A built artifact is already on disk; pinning it means recording what
		// the build produced.
		if a.Build != nil {
			sum, size, err := fileSHA256(filepath.Join(dir, a.As))
			if err != nil {
				return fmt.Errorf("artifact %s: %w\nrun: %s", a.Name, err, a.Build.Command)
			}
			m.Artifacts[i].SHA256 = sum
			m.Artifacts[i].SizeBytes = size
			fmt.Fprintf(out, "pinned   %s sha256=%s (%s, built locally)\n", a.Name, sum, humanSize(size))
			continue
		}

		fmt.Fprintf(out, "fetching %s\n         %s\n", a.Name, a.URL)
		obs, err := fetchArtifact(ctx, a, dir, client, false)
		if err != nil {
			return fmt.Errorf("artifact %s: %w", a.Name, err)
		}

		m.Artifacts[i].SHA256 = obs.ArchiveSHA
		m.Artifacts[i].SizeBytes = obs.ArchiveSize
		fmt.Fprintf(out, "pinned   %s sha256=%s (%s)\n", a.Name, obs.ArchiveSHA, humanSize(obs.ArchiveSize))

		for j := range m.Artifacts[i].Extract {
			as := m.Artifacts[i].Extract[j].As
			sum, ok := obs.Members[as]
			if !ok {
				return fmt.Errorf("artifact %s: nothing extracted for %q", a.Name, as)
			}
			m.Artifacts[i].Extract[j].SHA256 = sum
			fmt.Fprintf(out, "pinned   %s sha256=%s\n", as, sum)
		}
	}
	return nil
}

// satisfied reports whether every file this artifact produces is already on
// disk and matches its recorded digest.
func satisfied(a Artifact, dir string) bool {
	for _, o := range a.outputs() {
		if o.SHA256 == "" {
			return false
		}
		sum, _, err := fileSHA256(filepath.Join(dir, o.Name))
		if err != nil || !strings.EqualFold(sum, o.SHA256) {
			return false
		}
	}
	return true
}

// fetchArtifact downloads one artifact and writes its outputs into dir.
//
// With checkDigest set, a file whose hash does not match the manifest is
// discarded and reported; with it clear the hashes are merely observed, which
// is how pin bootstraps a manifest that has none yet.
func fetchArtifact(ctx context.Context, a Artifact, dir string, client *http.Client, checkDigest bool) (observed, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return observed{}, fmt.Errorf("create %s: %w", dir, err)
	}

	tmp := filepath.Join(dir, a.Name+tmpSuffix)
	defer func() { _ = os.Remove(tmp) }()

	sum, size, err := download(ctx, client, a.URL, tmp)
	if err != nil {
		return observed{}, err
	}

	if checkDigest {
		if err := matches(a.Name, sum, a.SHA256); err != nil {
			return observed{}, err
		}
	}

	obs := observed{ArchiveSHA: sum, ArchiveSize: size, Members: map[string]string{}}

	// Single-file artifact: the download is the model.
	if len(a.Extract) == 0 {
		if err := safeName(a.As); err != nil {
			return observed{}, err
		}
		if err := os.Rename(tmp, filepath.Join(dir, a.As)); err != nil {
			return observed{}, fmt.Errorf("install %s: %w", a.As, err)
		}
		obs.Members[a.As] = sum
		return obs, nil
	}

	members, err := extract(tmp, dir, a.Extract, checkDigest)
	if err != nil {
		return observed{}, err
	}
	obs.Members = members
	return obs, nil
}

// download streams url into tmpPath and returns the digest and size of what was
// written.
func download(ctx context.Context, client *http.Client, url, tmpPath string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, fmt.Errorf("build request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}

	f, err := os.Create(tmpPath)
	if err != nil {
		return "", 0, fmt.Errorf("create %s: %w", tmpPath, err)
	}

	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), resp.Body)
	closeErr := f.Close()

	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = os.Remove(tmpPath)
		return "", 0, fmt.Errorf("write %s: %w", tmpPath, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// extract lifts the named members out of a zip archive and installs them.
func extract(zipPath, dir string, want []Member, checkDigest bool) (map[string]string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = zr.Close() }()

	// Index by full path and by base name. Upstream has moved files between a
	// flat archive and a nested directory before; matching on the base name
	// survives that without a manifest edit.
	index := make(map[string]*zip.File, len(zr.File)*2)
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		index[f.Name] = f
		if base := path.Base(f.Name); index[base] == nil {
			index[base] = f
		}
	}

	got := make(map[string]string, len(want))
	for _, m := range want {
		zf := index[m.Path]
		if zf == nil {
			zf = index[path.Base(m.Path)]
		}
		if zf == nil {
			return nil, fmt.Errorf("archive has no entry %q", m.Path)
		}

		tmp, sum, err := extractOne(zf, dir, m.As)
		if err != nil {
			return nil, err
		}

		if checkDigest {
			if err := matches(m.As, sum, m.SHA256); err != nil {
				_ = os.Remove(tmp)
				return nil, err
			}
		}

		if err := os.Rename(tmp, filepath.Join(dir, m.As)); err != nil {
			_ = os.Remove(tmp)
			return nil, fmt.Errorf("install %s: %w", m.As, err)
		}
		got[m.As] = sum
	}
	return got, nil
}

// extractOne writes a single archive entry to a temporary file and returns its
// path and digest. The caller verifies before renaming it into place.
func extractOne(zf *zip.File, dir, as string) (string, string, error) {
	if err := safeName(as); err != nil {
		return "", "", err
	}

	rc, err := zf.Open()
	if err != nil {
		return "", "", fmt.Errorf("open %s inside archive: %w", zf.Name, err)
	}
	defer func() { _ = rc.Close() }()

	tmp := filepath.Join(dir, as+tmpSuffix)
	f, err := os.Create(tmp)
	if err != nil {
		return "", "", fmt.Errorf("create %s: %w", tmp, err)
	}

	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h), rc)
	closeErr := f.Close()

	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = os.Remove(tmp)
		return "", "", fmt.Errorf("extract %s: %w", as, err)
	}
	return tmp, hex.EncodeToString(h.Sum(nil)), nil
}

// matches compares an observed digest against the recorded one.
func matches(name, got, want string) error {
	if want == "" {
		return fmt.Errorf("%s: no digest recorded in the manifest; run `modelctl pin` to record one", name)
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("%s: digest mismatch (downloaded %s, manifest says %s)", name, got, want)
	}
	return nil
}

func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("read %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
