// Package web embeds the panel frontend (built assets, no external files at runtime).
package web

import "embed"

//go:embed index.html logo.svg talk
var FS embed.FS
