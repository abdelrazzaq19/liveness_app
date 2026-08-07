package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Manifest records every model file the service needs, where it comes from, and
// what it must hash to.
//
// It works like a lockfile. Recording a digest does not prove a model is
// trustworthy — the digests here were observed from a real download, not
// published by upstream — but it does prove that nobody swapped the file since
// it was pinned, and it makes a fresh checkout reproduce the same bytes.
type Manifest struct {
	Version   int        `json:"version"`
	Artifacts []Artifact `json:"artifacts"`
}

// Artifact is one downloadable unit.
//
// When Extract is empty the artifact is a single model file written as As.
// Otherwise it is a zip archive that yields several files.
type Artifact struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"size_bytes"`
	License    string `json:"license"`
	LicenseURL string `json:"license_url,omitempty"`
	Notes      string `json:"notes,omitempty"`

	// As is the file name written into the models directory. Single-file
	// artifacts only.
	As string `json:"as,omitempty"`

	Extract []Member `json:"extract,omitempty"`

	// Build is set instead of URL for artifacts that have no downloadable
	// release and must be produced locally.
	Build *Build `json:"build,omitempty"`
}

// Build describes how an artifact is produced when nobody publishes it.
//
// The digest still gets recorded, so a locally built model is pinned exactly
// like a downloaded one: what changes is only where the bytes came from, not
// whether anyone can tell they have changed.
type Build struct {
	// Command is what the operator runs to produce the file.
	Command string `json:"command"`

	// Source and SourceRef record what the build was made from, which is the
	// only provenance a locally produced file has.
	Source    string `json:"source,omitempty"`
	SourceRef string `json:"source_ref,omitempty"`
}

// Member is one file lifted out of a zip archive.
type Member struct {
	// Path is the entry inside the archive. Lookup falls back to matching on
	// the bare base name, so a change to the archive's internal directory
	// layout does not break the manifest.
	Path string `json:"path"`

	// As is the file name written into the models directory.
	As string `json:"as"`

	SHA256 string `json:"sha256"`
}

// output is a file the models directory is expected to end up with.
type output struct {
	Name   string
	SHA256 string
}

// outputs lists the files this artifact produces.
func (a Artifact) outputs() []output {
	if len(a.Extract) == 0 {
		return []output{{Name: a.As, SHA256: a.SHA256}}
	}

	outs := make([]output, 0, len(a.Extract))
	for _, m := range a.Extract {
		outs = append(outs, output{Name: m.As, SHA256: m.SHA256})
	}
	return outs
}

// loadManifest reads and validates a manifest from disk.
func loadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", path, err)
	}
	return &m, nil
}

// saveManifest writes the manifest back, replacing it atomically so an
// interrupted write cannot leave a truncated file behind.
func saveManifest(path string, m *Manifest) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	raw = append(raw, '\n')

	tmp := path + tmpSuffix
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func (m *Manifest) validate() error {
	if m.Version != 1 {
		return fmt.Errorf("unsupported version %d, this build understands version 1", m.Version)
	}
	if len(m.Artifacts) == 0 {
		return errors.New("contains no artifacts")
	}

	seen := make(map[string]string, len(m.Artifacts))
	var problems []error

	for _, a := range m.Artifacts {
		if a.Name == "" {
			problems = append(problems, errors.New("an artifact is missing its name"))
			continue
		}
		// The name is used to build the temporary download path, so it has to
		// be as constrained as an output name.
		if err := safeName(a.Name); err != nil {
			problems = append(problems, fmt.Errorf("artifact name: %w", err))
			continue
		}
		switch {
		case a.Build != nil && a.URL != "":
			problems = append(problems, fmt.Errorf("%s: has both \"url\" and \"build\"; pick one", a.Name))
		case a.Build != nil && a.Build.Command == "":
			problems = append(problems, fmt.Errorf("%s: build needs a command", a.Name))
		case a.Build == nil && a.URL == "":
			problems = append(problems, fmt.Errorf("%s: needs either \"url\" or \"build\"", a.Name))
		}

		if a.Build != nil && len(a.Extract) > 0 {
			problems = append(problems, fmt.Errorf("%s: a built artifact cannot also be an archive", a.Name))
		}

		if a.License == "" {
			problems = append(problems, fmt.Errorf("%s: missing license; every model must record one", a.Name))
		}

		if len(a.Extract) == 0 && a.As == "" {
			problems = append(problems, fmt.Errorf("%s: needs either \"as\" or \"extract\"", a.Name))
		}
		if len(a.Extract) > 0 && a.As != "" {
			problems = append(problems, fmt.Errorf("%s: has both \"as\" and \"extract\"; pick one", a.Name))
		}

		for _, m := range a.Extract {
			if m.Path == "" {
				problems = append(problems, fmt.Errorf("%s: an extract entry is missing its path", a.Name))
			}
		}

		for _, o := range a.outputs() {
			if err := safeName(o.Name); err != nil {
				problems = append(problems, fmt.Errorf("%s: %w", a.Name, err))
				continue
			}
			if prev, dup := seen[o.Name]; dup {
				problems = append(problems, fmt.Errorf("%s and %s both write %q", prev, a.Name, o.Name))
				continue
			}
			seen[o.Name] = a.Name
		}
	}

	return errors.Join(problems...)
}

// safeName rejects anything that is not a plain file name.
//
// Archive entries are attacker-controlled in the general case, so a name that
// can escape the models directory has to be refused rather than cleaned up.
func safeName(n string) error {
	switch {
	case n == "":
		return errors.New("empty output file name")
	case n != filepath.Base(n), strings.Contains(n, ".."), filepath.IsAbs(n):
		return fmt.Errorf("output name %q must be a plain file name", n)
	default:
		return nil
	}
}
