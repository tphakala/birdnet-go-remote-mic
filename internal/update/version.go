// Package update is the appliance side of the release update path: the
// periodic check for a newer signed release, the install-method detection
// that decides whether a one-button update is possible, the unprivileged
// staging of a verified release binary, and the root updater that installs a
// staged binary, restarts the service, and rolls back when the new version
// does not come up.
//
// Trust flows only from the release signing keys compiled into each binary
// (internal/releasemanifest). The appliance verifies a manifest before acting
// on it, and the root updater verifies it again with its own keys: the files
// the appliance stages are never trusted on the appliance's word.
package update

import (
	"errors"
	"regexp"
	"strings"

	"github.com/tphakala/birdnet-go-remote-mic/internal/releasemanifest"
)

// ErrNotRelease reports a running version that is not a release, such as a
// "dev" build, so it has nothing to compare a release against.
var ErrNotRelease = errors.New("the running build is not a release")

// describeSuffix matches what `git describe --dirty` appends to a build made
// after a tag (-<commits>-g<hash>, optionally -dirty) or on a tag with local
// changes (-dirty).
var describeSuffix = regexp.MustCompile(`(?:-\d+-g[0-9a-f]+)?(?:-dirty)?$`)

// BaseVersion returns the release a running build derives from: the version
// itself for a release build, or the tag a `git describe` build was made from.
// ok is false for a build that names no release (for example "dev").
//
// A describe suffix reads as a SemVer prerelease (v0.2.0-41-gf26d800 sorts
// before v0.2.0), but such a build is newer than its tag, so comparing it
// as-is would offer the tag it was built from as an update.
func BaseVersion(running string) (base string, ok bool) {
	base = describeSuffix.ReplaceAllString(running, "")
	if !releasemanifest.ValidVersion(base) {
		return "", false
	}
	return base, true
}

// Newer reports whether latest is an update for the running build: a stable
// release strictly newer than the release the running build derives from. A
// prerelease is never an update, since the operator has no way to opt in to
// one, and a signed manifest can still be replayed, so an equal or older
// version is never offered either.
func Newer(latest, running string) (bool, error) {
	base, ok := BaseVersion(running)
	if !ok {
		return false, ErrNotRelease
	}
	if !releasemanifest.ValidVersion(latest) {
		return false, releasemanifest.ErrInvalid
	}
	if strings.ContainsRune(latest, '-') {
		return false, nil
	}
	c, err := releasemanifest.CompareVersions(latest, base)
	if err != nil {
		return false, err
	}
	return c > 0, nil
}

// Ahead reports whether the installed build is a later release than the
// running one, for a decision that must not replace a newer binary with an
// older one. Unlike Newer it compares raw SemVer, so an installed prerelease
// (v0.4.0-rc.1) is ahead of a running v0.3.0. Both sides are reduced to the
// release they derive from (see BaseVersion), and a build that names no
// release (such as "dev") on either side is an error, not an answer.
func Ahead(installed, running string) (bool, error) {
	ib, ok := BaseVersion(installed)
	if !ok {
		return false, ErrNotRelease
	}
	rb, ok := BaseVersion(running)
	if !ok {
		return false, ErrNotRelease
	}
	c, err := releasemanifest.CompareVersions(ib, rb)
	if err != nil {
		return false, err
	}
	return c > 0, nil
}

// ShellQuote quotes s for a POSIX shell so a copyable command keeps a path
// with spaces or metacharacters as one argument. A plain path is returned
// as it is.
func ShellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+=:,@%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
