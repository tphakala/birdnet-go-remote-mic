// Package monitor holds the condition monitors behind the notification center
// and the settings seam that feeds them from configuration.
//
// This first piece is pure plumbing: the immutable Settings value, the
// SettingsFrom builder that derives it from a config, and the small Monitors
// interface the appliance calls on every reload. The signal and host monitors
// (and their evaluation logic) arrive in later changes; nothing here evaluates a
// condition yet.
package monitor

import "github.com/tphakala/birdnet-go-remote-mic/internal/config"

// Settings is an immutable snapshot of the notification thresholds and the
// per-device quiet-alert opt-out. A monitor holds it in an atomic pointer and
// reads it once per evaluation tick, so a hot reload swaps the whole value
// rather than restarting the monitor goroutine.
type Settings struct {
	// Enabled reports whether the condition monitors run. When false the
	// monitors resolve their active conditions and stop evaluating; the discrete
	// emitters (device failures, client and config events) keep publishing.
	Enabled bool
	// Audio and Host carry the materialized thresholds from the config's
	// notifications block.
	Audio config.AudioAlerts
	Host  config.HostAlerts
	// QuietAlert maps a device name to whether its very-quiet audio condition is
	// armed (the per-device quiet_alert flag, default on). A device missing from
	// the map is treated as armed by the signal monitor.
	QuietAlert map[string]bool
}

// SettingsFrom builds a Settings from cfg: the materialized notifications block
// plus a quiet-alert entry for every configured device. It reads the config but
// never retains a reference to it, so the returned value is safe to hand across
// goroutines.
func SettingsFrom(cfg *config.Config) Settings {
	qa := make(map[string]bool, len(cfg.Devices))
	for i := range cfg.Devices {
		qa[cfg.Devices[i].Name] = cfg.Devices[i].QuietAlertEnabled()
	}
	return Settings{
		Enabled:    cfg.NotificationsEnabled(),
		Audio:      cfg.Notifications.Audio,
		Host:       cfg.Notifications.Host,
		QuietAlert: qa,
	}
}

// Monitors is the appliance's handle to the running condition monitors. The
// reconcile calls Apply at the end of every reload so a threshold or opt-out
// change re-arms the monitors in place, without restarting any device. A nil
// Monitors is a no-op, so the appliance can run before any monitor exists.
type Monitors interface {
	Apply(s *Settings)
}
