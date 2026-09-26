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
