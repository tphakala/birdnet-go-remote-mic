//go:build skipfrontend

// Package web provides a stub for builds that intentionally leave the web UI
// out (the `skipfrontend` tag). It lets pure-Go work (CI test/vet/lint and a
// clean checkout with no compiled web/dist) build without a Node toolchain,
// since go:embed would otherwise require the generated dist directory to exist.
package web

import (
	"errors"
	"io/fs"
)

// errNotEmbedded reports that the compiled web UI is not part of this build.
var errNotEmbedded = errors.New("web UI not embedded (built with skipfrontend)")

// DistFS reports that no embedded UI is available in a skipfrontend build. The
// sole caller (cmd/remotemic) handles this by serving the management JSON API
// without the static UI, so the appliance still starts.
func DistFS() (fs.FS, error) {
	return nil, errNotEmbedded
}
