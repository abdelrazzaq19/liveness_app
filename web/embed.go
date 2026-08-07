// Package web carries the demo page into the binary.
//
// Embedded rather than served from disk so the page and the API it calls ship
// together. A demo that has drifted away from its own endpoints is a confusing
// way to discover a version mismatch.
package web

import "embed"

// FS holds the demo assets.
//
//go:embed static
var FS embed.FS
