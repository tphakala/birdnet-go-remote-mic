package update

import (
	"errors"
	"testing"
)

func TestBaseVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
		ok       bool
	}{
		{vOld, vOld, true},
		{vDescribe, vOld, true},
		{"v0.2.0-41-gf26d800-dirty", vOld, true},
		{"v0.2.0-dirty", vOld, true},
		{vRC, vRC, true},
		{"v1.0.0-rc.1-3-gabc1234", vRC, true},
		{"dev", "", false},
		{"", "", false},
		{"0.2.0", "", false},
	}
	for _, tt := range tests {
		got, ok := BaseVersion(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("BaseVersion(%q) = %q, %t; want %q, %t", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestNewer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		latest, running string
		want            bool
	}{
		{vNew, vOld, true},
		{"v0.2.1", vOld, true},
		{vOld, vOld, false},
		{"v0.1.9", vOld, false},
		// A build past a tag is newer than the tag, never offered it again.
		{vOld, vDescribe, false},
		{"v0.2.1", vDescribe, true},
		// A prerelease is never an update, even a newer one.
		{"v0.3.0-rc.1", vOld, false},
		// A release is an update for its own prerelease.
		{"v1.0.0", vRC, true},
	}
	for _, tt := range tests {
		got, err := Newer(tt.latest, tt.running)
		if err != nil {
			t.Errorf("Newer(%q, %q): %v", tt.latest, tt.running, err)
			continue
		}
		if got != tt.want {
			t.Errorf("Newer(%q, %q) = %t, want %t", tt.latest, tt.running, got, tt.want)
		}
	}
}

func TestNewerRefusesUnusableVersions(t *testing.T) {
	t.Parallel()
	if _, err := Newer(vNew, "dev"); !errors.Is(err, ErrNotRelease) {
		t.Errorf("dev build: got %v, want ErrNotRelease", err)
	}
	if _, err := Newer("0.3.0", vOld); err == nil {
		t.Error("unprefixed latest: got nil error")
	}
}

func TestAhead(t *testing.T) {
	t.Parallel()
	const newer, describe = "v0.4.0", "v0.3.0-25-gf26d800"
	tests := []struct {
		installed, running string
		want               bool
	}{
		{newer, vNew, true},
		{"v0.4.0-rc.1", vNew, true}, // a prerelease is still ahead of an older release
		{vNew, vNew, false},
		{vNew, newer, false},
		{"v0.3.0-rc.1", vNew, false},
		{describe, vNew, false}, // a describe build derives from its tag
		{newer, describe, true},
	}
	for _, tt := range tests {
		got, err := Ahead(tt.installed, tt.running)
		if err != nil || got != tt.want {
			t.Errorf("Ahead(%q, %q) = %t, %v; want %t", tt.installed, tt.running, got, err, tt.want)
		}
	}
	for _, bad := range [][2]string{{"not-a-version", vNew}, {vNew, "not-a-version"}, {"", vNew}} {
		if _, err := Ahead(bad[0], bad[1]); err == nil {
			t.Errorf("Ahead(%q, %q) = nil error, want one", bad[0], bad[1])
		}
	}
}

func TestShellQuote(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"/usr/local/bin/remote-mic": "/usr/local/bin/remote-mic",
		"/home/pi/Remote Mic/rm":    "'/home/pi/Remote Mic/rm'",
		"/tmp/it's":                 `'/tmp/it'\''s'`,
		"/tmp/$(id)":                "'/tmp/$(id)'",
		"":                          "''",
	}
	for in, want := range tests {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
