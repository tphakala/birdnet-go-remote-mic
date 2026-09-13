package monitor

import (
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
)

func TestSettingsFromMapsEveryField(t *testing.T) {
	t.Parallel()
	off := false
	cfg := config.Config{
		Notifications: config.Notifications{
			// Enabled left nil: defaults on.
			Audio: config.AudioAlerts{QuietDbfs: -50, QuietSeconds: 1200, ZeroSeconds: 45, ClipPercent: 30, ClipWindowSeconds: 15},
			Host:  config.HostAlerts{CPUPercent: 85, CPUClearPercent: 70, TempCelsius: 75, TempClearCelsius: 70, DiskPercent: 88, DiskClearPercent: 80, MemFreePercent: 15, MemFreeMiB: 128},
		},
		Devices: []config.Device{
			{Name: "garden"},                // no quiet_alert flag -> armed (default on)
			{Name: "bat", QuietAlert: &off}, // opted out
		},
	}
	s := SettingsFrom(&cfg)
	if !s.Enabled {
		t.Error("Enabled = false, want true (absent flag defaults on)")
	}
	if s.Audio != cfg.Notifications.Audio {
		t.Errorf("Audio = %+v, want %+v", s.Audio, cfg.Notifications.Audio)
	}
	if s.Host != cfg.Notifications.Host {
		t.Errorf("Host = %+v, want %+v", s.Host, cfg.Notifications.Host)
	}
	if len(s.QuietAlert) != 2 {
		t.Fatalf("QuietAlert = %v, want two entries", s.QuietAlert)
	}
	if !s.QuietAlert["garden"] {
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
