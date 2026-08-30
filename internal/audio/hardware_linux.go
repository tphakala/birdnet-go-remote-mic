//go:build linux

package audio

import (
	capture "github.com/tphakala/go-audio-capture"
)

// enumerateDevices is a package var so tests can inject a fake device list.
var enumerateDevices = capture.Devices

// supportedRatesFn is a package var so tests can inject a fake capability query.
var supportedRatesFn = capture.SupportedRates

// HardwareNames returns a map from ALSA device id to a friendly label for every
// capture device the host currently exposes. It lets the UI default a device's
// display name from the sound card when the config leaves it blank.
func HardwareNames() (map[string]string, error) {
	devs, err := enumerateDevices()
	if err != nil {
		return nil, err
	}
	return hardwareNamesFrom(devs), nil
}

// hardwareNamesFrom is the pure mapping half of HardwareNames, split out so the
// id-to-label derivation is testable without reading /proc.
func hardwareNamesFrom(devs []capture.DeviceInfo) map[string]string {
	names := make(map[string]string, len(devs))
	for i := range devs {
		names[devs[i].ID] = FriendlyName(devs[i].Name)
	}
	return names
}

// ProbeRates returns the subset of candidates the device accepts, for the config
// UI's rate dropdown. It uses go-audio-capture's HW_REFINE-based SupportedRates,
// which opens the device once per format with O_NONBLOCK and issues a refine
// ioctl per rate without any state transition. That replaces the old open/close
// probe (about a dozen exclusive opens per device at startup) and the
// EBUSY-after-close race it risked, and it may run while the device is free.
//
// It queries both capture formats (S16LE and S32LE) and keeps the union,
// because OpenCapture negotiates the capture format automatically (S16
// preferred, S32 fallback), so a rate a device offers only in S32 is still
// usable. Any query error (device busy or gone, or a format the device rejects)
// simply contributes no rates. When nothing in candidates is supported it
// returns nil so the caller falls back to the static rate list rather than
// reporting a misleading empty set.
func ProbeRates(deviceID string, channels int, candidates []int) []int {
	supported := make(map[int]bool)
	for _, f := range captureFormats {
		rs, err := supportedRatesFn(deviceID, channels, f)
		if err != nil {
			continue
		}
		for _, r := range rs.Rates {
			supported[r] = true
		}
	}
	out := make([]int, 0, len(candidates))
	for _, r := range candidates {
		if supported[r] {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
