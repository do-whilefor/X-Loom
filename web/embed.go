// Package web serves the unmodified Cairn browser interface.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var assets embed.FS

// Files is rooted at Cairn's static directory, preserving its original URLs.
var Files, _ = fs.Sub(assets, "static")
