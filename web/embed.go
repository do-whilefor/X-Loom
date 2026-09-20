package web

import "embed"

// Files are copied unchanged from Cairn's static directory.
//
//go:embed index.html favicon.svg vendor/*
var Files embed.FS
