// Package migrations embeds FirmScout's SQL migration files so that a compiled binary
// carries its own schema.
//
// The files live outside internal/ because they are also read by operational tooling
// (psql, goose, CI schema diffing) that has no Go build step. This package exists only
// so the Go migration runner in internal/adapters/postgres can reach them: //go:embed
// cannot escape its own directory, so the embed declaration has to live here.
package migrations

import "embed"

// FS holds every migration file, keyed by its bare filename such as
// "00001_initial.sql". Filename order is migration order, which is why the numeric
// prefix is fixed width.
//
//go:embed *.sql
var FS embed.FS
