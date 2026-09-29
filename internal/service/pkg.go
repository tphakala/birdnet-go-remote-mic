//go:build linux

package service

import (
	"bufio"
	"os"
	"path/filepath"
)

// dpkgListGlob matches the file list dpkg keeps for the .deb package (the
// package name is set in .goreleaser.yaml).
const dpkgListGlob = "/var/lib/dpkg/info/birdnet-go-remote-mic*.list"

// PackageOwns reports whether a dpkg file list of the .deb package names
// path. apt replaces such a binary on upgrade, so the root updater must not
// (update.DetectInstall) and an install has no reason to copy it.
func PackageOwns(path string) bool { return packageOwnsIn(dpkgListGlob, path) }

// packageOwnsIn is PackageOwns over the file lists matching glob.
func packageOwnsIn(glob, path string) bool {
	lists, _ := filepath.Glob(glob)
	for _, l := range lists {
		if fileListHas(l, path) {
			return true
		}
	}
	return false
}

func fileListHas(list, path string) bool {
	f, err := os.Open(list) //nolint:gosec // a dpkg file list matched by dpkgListGlob
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if sc.Text() == path {
			return true
		}
	}
	return false
}
