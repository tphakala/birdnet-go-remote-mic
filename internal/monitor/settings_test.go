package monitor

import (
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
)

// p returns a pointer to n, for building the presence-aware threshold fields.
func p(n int) *int { return &n }

func TestSettingsFromMapsEveryField(t *testing.T) {
	t.Parallel()
	off := false
	cfg := config.Config{
		Notifications: config.Notifications{
			// Enabled left nil: defaults on.
			Audio: config.AudioAlerts{QuietDbfs: p(-50), QuietSeconds: p(1200), ZeroSeconds: p(45), ClipPercent: p(30), ClipWindowSeconds: p(15)},
			Host:  config.HostAlerts{CPUPercent: p(85), CPUClearPercent: p(70), TempCelsius: p(75), TempClearCelsius: p(70), DiskPercent: p(88), DiskClearPercent: p(80), MemFreePercent: p(15), MemFreeMiB: p(128)},
		},
		Devices: []config.Device{
			{Name: nameGarden},              // no quiet_alert flag -> armed (default on)
			{Name: "bat", QuietAlert: &off}, // opted out
		},
	}
	s := SettingsFrom(&cfg)
	if !s.Enabled {
		t.Error("Enabled = false, want true (absent flag defaults on)")
	}
	if !s.UpdateCheck {
		t.Error("UpdateCheck = false, want true (absent updates.check defaults on)")
	}
	cfg.Updates.Check = &off
	if SettingsFrom(&cfg).UpdateCheck {
		t.Error("UpdateCheck = true with updates.check false")
	}
	on := true
	cfg.Updates.Check = &on
	if !SettingsFrom(&cfg).UpdateCheck {
		t.Error("UpdateCheck = false with updates.check true")
	}
	// Every threshold is carried through. SettingsFrom is called with a defaulted
	// config, so each pointer is non-nil; here they are all set explicitly.
	if *s.Audio.QuietDbfs != -50 || *s.Audio.QuietSeconds != 1200 || *s.Audio.ZeroSeconds != 45 ||
		*s.Audio.ClipPercent != 30 || *s.Audio.ClipWindowSeconds != 15 {
		t.Errorf("Audio = %+v, want the input thresholds", s.Audio)
	}
	if *s.Host.CPUPercent != 85 || *s.Host.CPUClearPercent != 70 || *s.Host.TempCelsius != 75 ||
		*s.Host.TempClearCelsius != 70 || *s.Host.DiskPercent != 88 || *s.Host.DiskClearPercent != 80 ||
		*s.Host.MemFreePercent != 15 || *s.Host.MemFreeMiB != 128 {
		t.Errorf("Host = %+v, want the input thresholds", s.Host)
	}
	if len(s.QuietAlert) != 2 {
		t.Fatalf("QuietAlert = %v, want two entries", s.QuietAlert)
	}
	if !s.QuietAlert[nameGarden] {
		t.Error("QuietAlert[garden] = false, want true (default on)")
	}
	if s.QuietAlert["bat"] {
		t.Error("QuietAlert[bat] = true, want false (explicitly opted out)")
	}
}

func TestSettingsFromEnabledExplicitOff(t *testing.T) {
	t.Parallel()
	off := false
	cfg := config.Config{Notifications: config.Notifications{Enabled: &off}}
	s := SettingsFrom(&cfg)
	if s.Enabled {
		t.Error("Enabled = true, want false (explicitly disabled)")
	}
	if len(s.QuietAlert) != 0 {
		t.Errorf("QuietAlert = %v, want empty (no devices)", s.QuietAlert)
	}
}

// recMonitors records how many times Apply was called and the last pointer it
// received, so a test can assert Group forwards the exact settings pointer.
type recMonitors struct {
	n    int
	last *Settings
}

func (r *recMonitors) Apply(s *Settings) {
	r.n++
	r.last = s
}

// TestGroupApplyFansOut checks Group.Apply forwards the settings to every non-nil
// member, skips nil members, and hands each the very pointer it was given.
func TestGroupApplyFansOut(t *testing.T) {
	a, b := &recMonitors{}, &recMonitors{}
	g := Group{a, nil, b}
	s := hostSettings()
	g.Apply(&s)
	if a.n != 1 || b.n != 1 {
		t.Errorf("Group.Apply calls = %d, %d; want 1, 1", a.n, b.n)
	}
	if a.last != &s || b.last != &s {
		t.Error("Group.Apply forwarded a different pointer than it was given")
	}
}
