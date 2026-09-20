//go:build linux

package service

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// Family is the packaging family of the host, derived from /etc/os-release. It
// exists so the install path can vary the few things that actually differ
// between deb and rpm systems (today only the nologin shell path) without
// forking the whole installer. Both families use systemd and useradd, so the
// seam stays thin.
type Family int

const (
	FamilyUnknown Family = iota
	FamilyDebian
	FamilyRHEL
)

func (f Family) String() string {
	switch f {
	case FamilyDebian:
		return "debian"
	case FamilyRHEL:
		return "rhel"
	default:
		return "unknown"
	}
}

// Platform is the detected host identity. ID is the raw os-release ID, kept for
// operator-facing messages.
type Platform struct {
	Family Family
	ID     string
}

// osReleasePath and fileExists are seams so detection and shell selection are
// testable without depending on the host's real /etc.
var (
	osReleasePath                   = "/etc/os-release"
	fileExists    func(string) bool = func(p string) bool { _, err := os.Stat(p); return err == nil }
)

// debianIDs and rhelIDs classify an os-release ID or ID_LIKE token. They are
// the common members of each family; an unlisted derivative still classifies
// through ID_LIKE, which every derivative sets to its parent.
var (
	debianIDs = map[string]bool{"debian": true, "ubuntu": true, "raspbian": true, "linuxmint": true, "pop": true}
	rhelIDs   = map[string]bool{"rhel": true, "fedora": true, "centos": true, "rocky": true, "almalinux": true, "ol": true}
)

// Detect reads /etc/os-release and classifies the host. A missing or unreadable
// file yields FamilyUnknown rather than an error: the installer still works
// (its useradd and systemctl paths are family-independent), it just loses the
// family-specific nologin shell hint.
func Detect() Platform {
	f, err := os.Open(osReleasePath)
	if err != nil {
		return Platform{Family: FamilyUnknown}
	}
	defer func() { _ = f.Close() }()
	return parseOSRelease(f)
}

// parseOSRelease classifies from os-release content. It reads ID first, then
// falls back to the ID_LIKE token list, so a derivative that BirdNET-Go does not
// list by name (e.g. a niche Debian remix) still resolves to its parent family.
func parseOSRelease(r io.Reader) Platform {
	var id, idLike string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		val = unquote(strings.TrimSpace(val))
		switch strings.TrimSpace(key) {
		case "ID":
			id = strings.ToLower(val)
		case "ID_LIKE":
			idLike = strings.ToLower(val)
		}
	}
	p := Platform{ID: id}
	switch {
	case debianIDs[id]:
		p.Family = FamilyDebian
	case rhelIDs[id]:
		p.Family = FamilyRHEL
	default:
		p.Family = familyFromLike(idLike)
	}
	return p
}

// familyFromLike classifies from the space-separated ID_LIKE list, checking
// debian before rhel so a token list mentioning both resolves deterministically.
func familyFromLike(idLike string) Family {
	for _, tok := range strings.Fields(idLike) {
		if debianIDs[tok] {
			return FamilyDebian
		}
	}
	for _, tok := range strings.Fields(idLike) {
		if rhelIDs[tok] {
			return FamilyRHEL
		}
	}
	return FamilyUnknown
}

// unquote strips a single pair of surrounding double or single quotes, matching
// how os-release quotes values that contain spaces.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// NologinShell returns the account shell to give the service user. rpm systems
// ship nologin at /sbin/nologin, deb systems at /usr/sbin/nologin; it probes for
// the family's path and falls back to the other so a wrong guess never sets a
// nonexistent shell.
func (p Platform) NologinShell() string {
	if p.Family == FamilyRHEL {
		return firstExisting("/sbin/nologin", "/usr/sbin/nologin")
	}
	return firstExisting("/usr/sbin/nologin", "/sbin/nologin")
}

func firstExisting(paths ...string) string {
	for _, p := range paths {
		if fileExists(p) {
			return p
		}
	}
	return paths[0]
}
