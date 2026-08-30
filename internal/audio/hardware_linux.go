//go:build linux

package audio

import (
	"errors"

	capture "github.com/tphakala/go-audio-capture"
)

// enumerateDevices is a package var so tests can inject a fake device list.
var enumerateDevices = capture.Devices

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

// ProbeRates returns the subset of candidates the device accepts, found by
// opening the device at each candidate rate and closing it immediately. It must
// run while the device is free (before the real capture open), because hw:
// devices are exclusive and cannot be re-probed once serving.
//
// A candidate is kept only when the open succeeds and the hardware negotiates
// that exact rate (the honest-rate policy). A *BadRateError means the rate is
// unsupported, so probing continues. Any other open failure means the device
// itself is missing or busy: probing stops and returns nil so the caller falls
// back to the static rate list rather than reporting a misleading empty set.
func ProbeRates(deviceID string, channels int, candidates []int) []int {
	supported := make([]int, 0, len(candidates))
	for _, r := range candidates {
		s, err := openStream(capture.Config{
			Device:   deviceID,
			Rate:     r,
			Channels: channels,
			Format:   capture.FormatS16LE,
		})
		if err != nil {
			var bre *capture.BadRateError
			if errors.As(err, &bre) {
				continue
			}
			return nil
		}
		neg := s.Negotiated()
		_ = s.Close()
		if neg.Rate == r {
			supported = append(supported, r)
		}
	}
	return supported
}
