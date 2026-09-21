// Package web embeds the X-Loom workspace and the classic Cairn interface.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var assets embed.FS

// Files contains locally bundled assets, preserving the classic static URLs.
var Files, _ = fs.Sub(assets, "static")
