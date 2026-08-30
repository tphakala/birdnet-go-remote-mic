// Package web embeds the pre-built management web UI static assets into the Go binary.
package web

import (
	"embed"
	"io/fs"
)

// DistEmbed contains the compiled frontend assets from the dist directory.
//
//go:embed dist
var DistEmbed embed.FS

// DistFS returns an fs.FS rooted at the dist directory.
func DistFS() (fs.FS, error) {
	return fs.Sub(DistEmbed, "dist")
}
