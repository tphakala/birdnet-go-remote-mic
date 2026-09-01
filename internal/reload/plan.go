// Package reload computes how to reconcile a running capture appliance to a new
// configuration without restarting the process. The planning here is pure and
// platform-neutral so it can be exhaustively unit-tested; the linux command
// executes the resulting plan against real ALSA hardware.
package reload

import (
	"slices"
	"sort"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
)

// Plan is the set of per-device actions that reconcile the currently running
// devices to a desired configuration. A device that is enabled, already running,
// and unchanged appears in none of the lists: it keeps serving and its RTSP
// clients stay connected, which is the whole point of hot reload.
type Plan struct {
	// Start opens and begins serving these devices: brand new entries, ones that
	// were just enabled, or a retry of an enabled device that is not currently
	// running (for example one that failed to open earlier).
	Start []config.Device
	// Restart stops the running device of this name and starts it again with these
	// parameters, because a capture-relevant field changed. The client on that one
	// device is dropped; every other device is untouched.
	Restart []config.Device
	// Stop closes and unregisters the running devices with these names: entries
	// removed from the config or newly disabled.
	Stop []string
}

// Empty reports whether the plan asks for no changes.
func (p Plan) Empty() bool {
	return len(p.Start) == 0 && len(p.Restart) == 0 && len(p.Stop) == 0
}

// Reconcile diffs the currently running devices (keyed by device name against the
// parameters they are running with) to the desired configuration and returns the
// actions that converge one to the other. The device name is the identity key
// (config.Validate guarantees names are unique), so a renamed device is a Stop of
// the old name plus a Start of the new one, not a Restart. Every list is sorted
// so execution order and tests are deterministic.
func Reconcile(running map[string]config.Device, desired *config.Config) Plan {
	var p Plan

	// The devices the config wants serving: enabled entries, keyed by name.
	desiredEnabled := make(map[string]config.Device, len(desired.Devices))
	for i := range desired.Devices {
		d := desired.Devices[i]
		if d.IsEnabled() {
			desiredEnabled[d.Name] = d
		}
	}

	// Stop anything running that the config no longer wants serving.
	for name := range running {
		if _, ok := desiredEnabled[name]; !ok {
			p.Stop = append(p.Stop, name)
		}
	}

	// Start or restart every device the config wants serving. Range over keys and
	// index the value: config.Device is large enough that a range-value copy is
	// wasteful (and gocritic-flagged).
	for name := range desiredEnabled {
		want := desiredEnabled[name]
		cur, isRunning := running[name]
		switch {
		case !isRunning:
			p.Start = append(p.Start, want)
		case !captureParamsEqual(&cur, &want):
			p.Restart = append(p.Restart, want)
		default:
			// Running with identical parameters: leave it alone.
		}
	}

	sort.Strings(p.Stop)
	sort.Slice(p.Start, func(i, j int) bool { return p.Start[i].Name < p.Start[j].Name })
	sort.Slice(p.Restart, func(i, j int) bool { return p.Restart[i].Name < p.Restart[j].Name })
	return p
}

// captureParamsEqual reports whether two device configurations open the same ALSA
// stream and build the same pipeline and RTSP track, so a running device need not
// be restarted. It compares every field that feeds the capture open, the pipeline
// stage, the SDP, or the RTSP mount. The name is the identity key and is equal by
// construction here; the enabled flag is handled by the caller.
func captureParamsEqual(a, b *config.Device) bool {
	return a.Device == b.Device &&
		a.Path == b.Path &&
		a.Mode == b.Mode &&
		a.Rate == b.Rate &&
		slices.Equal(a.Channels, b.Channels) &&
		a.Format == b.Format &&
		a.Opus.Bitrate == b.Opus.Bitrate
}
