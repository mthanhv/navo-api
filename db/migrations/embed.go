// Package migrations embeds the numbered SQL migrations into the binaries.
package migrations

import "embed"

// FS holds every migration file.
//
//go:embed *.sql
var FS embed.FS
