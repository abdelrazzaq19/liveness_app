// Package migrations carries the SQL schema into the binary.
//
// Embedded rather than read from disk so that the container running a migration
// is the container that was built with it. A binary and a schema directory that
// have drifted apart is exactly the failure this avoids, and it is one that
// only shows up in the environment where it matters.
package migrations

import "embed"

// FS holds every migration file.
//
//go:embed *.sql
var FS embed.FS
