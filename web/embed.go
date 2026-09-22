// Package web embeds the X-Loom workspace.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var assets embed.FS

// Files contains the workspace's locally bundled static assets.
var Files, _ = fs.Sub(assets, "static")
