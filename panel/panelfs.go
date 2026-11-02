// Package panelfs embeds the compiled Next.js management panel so the entire
// origin-mediaserver ships as a single binary with no external static files.
package panelfs

import "embed"

//go:embed all:out
var FS embed.FS
