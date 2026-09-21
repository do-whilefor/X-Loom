package web

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

// These are the SHA-256 digests of Cairn's source static assets. Pinning them
// preserves the classic management interface alongside the X-Loom workspace.
func TestLegacyCairnAssets(t *testing.T) {
	want := map[string]string{
		"favicon.svg":               "8600d99b56c62e2cb159a6cae0a3bf543906602507770c8e67b85b90fb1473bb",
		"legacy.html":               "6c6ae6f311347965a53e3045ecdc9ccfcaff26ece581337d2b09d7184930d7e6",
		"vendor/alpine.min.js":      "beeba63d08956f64fa060f6b6a29c87a341bf069fb96c9459c222c6fd42e58ae",
		"vendor/cola.min.js":        "b45eba3fb02d22d9ea9a2bcad07ddbec8296f807f05e96f8c9d4abbc497f65c7",
		"vendor/cytoscape-cola.js":  "bb820d9f649827582d005682e26a0cea915539a0dbda32a17964ad2210bbcd08",
		"vendor/cytoscape-dagre.js": "bf70fe402991dcbff33e05a7e4a5271c78020bb75e85d1c80ab7538e4157112e",
		"vendor/cytoscape-elk.js":   "0fdf92c76840c38ec3e166e59f3b760d194c3e07c31a0cf483d56963c330dd6d",
		"vendor/cytoscape-klay.js":  "316dc832a2708926ceb4eaad5e8e699f833fb9cae8634019ed33c23c9aa655b0",
		"vendor/cytoscape.min.js":   "f55947f3daa3bae53209d4b885c195c157f595c225e508a6b382598d9452d6e2",
		"vendor/dagre.min.js":       "62eb9787ccfdbdf4148d4d99d31dbf9ee4770eafee81e637d759b52aac22cd51",
		"vendor/elk.bundled.js":     "20dd2114d683ce758b3ce19bcc56e28a504a617b0d280f760407c37314631d0e",
		"vendor/klay.js":            "29e37d45e84d43ebe510b0180ddf5671aab0cdebbdccf6a450e0cfc00d3330df",
		"vendor/tailwindcss.js":     "3f81aa7f6ecdb1acc14c202e513dfee00b6c7703cd81ce1be25bf5215a92e8cb",
	}
	owned := map[string]bool{"index.html": true, "app.js": true, "api.js": true, "data.js": true, "graph.js": true, "style.css": true, "xloom.svg": true}
	count := 0
	err := fs.WalkDir(Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		count++
		data, err := fs.ReadFile(Files, path)
		if err != nil {
			return err
		}
		if owned[path] {
			return nil
		}
		// The classic page may use the project's X-Loom title; its original
		// scripts, markup and styles remain pinned to Cairn's asset digest.
		if path == "legacy.html" {
			data = []byte(strings.Replace(string(data), "<title>X-Loom</title>", "<title>Cairn</title>", 1))
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != want[path] {
			t.Errorf("asset differs from Cairn: %s = %s", path, got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != len(want)+len(owned) {
		t.Fatalf("embedded %d assets, want %d", count, len(want)+len(owned))
	}
}
