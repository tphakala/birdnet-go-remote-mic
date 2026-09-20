//go:build linux

package service

import (
	"strings"
	"testing"
)

func TestParseOSRelease(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Family
	}{
		{"debian", "ID=debian\nVERSION_ID=12\n", FamilyDebian},
		{"ubuntu", "ID=ubuntu\nID_LIKE=debian\n", FamilyDebian},
		{"raspbian", "ID=raspbian\nID_LIKE=debian\n", FamilyDebian},
		{"fedora", "ID=fedora\n", FamilyRHEL},
		{"rocky via id_like", "ID=rocky\nID_LIKE=\"rhel centos fedora\"\n", FamilyRHEL},
		{"derivative via id_like debian", "ID=someremix\nID_LIKE=\"ubuntu debian\"\n", FamilyDebian},
		{"unknown", "ID=plan9\n", FamilyUnknown},
		{"empty", "", FamilyUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseOSRelease(strings.NewReader(c.body))
			if got.Family != c.want {
				t.Fatalf("family = %v, want %v", got.Family, c.want)
			}
		})
	}
}

func TestNologinShell(t *testing.T) {
	// Pin fileExists so the test does not depend on the host's real /sbin layout.
	orig := fileExists
	t.Cleanup(func() { fileExists = orig })

	fileExists = func(p string) bool { return p == "/usr/sbin/nologin" || p == "/sbin/nologin" }
	if got := (Platform{Family: FamilyDebian}).NologinShell(); got != "/usr/sbin/nologin" {
		t.Errorf("debian nologin = %q, want /usr/sbin/nologin", got)
	}
	if got := (Platform{Family: FamilyRHEL}).NologinShell(); got != "/sbin/nologin" {
		t.Errorf("rhel nologin = %q, want /sbin/nologin", got)
	}

	// When the family's preferred path is absent, fall back to the other.
	fileExists = func(p string) bool { return p == "/usr/sbin/nologin" }
	if got := (Platform{Family: FamilyRHEL}).NologinShell(); got != "/usr/sbin/nologin" {
		t.Errorf("rhel fallback nologin = %q, want /usr/sbin/nologin", got)
	}

	// When neither exists, return the family's preferred path anyway.
	fileExists = func(string) bool { return false }
	if got := (Platform{Family: FamilyDebian}).NologinShell(); got != "/usr/sbin/nologin" {
		t.Errorf("debian none-exist nologin = %q, want /usr/sbin/nologin", got)
	}
}
