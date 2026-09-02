//go:build linux

package audio

import (
	"errors"

	capture "github.com/tphakala/go-audio-capture"
)

// enumerateDevices is a package var so tests can inject a fake device list.
var enumerateDevices = capture.Devices

// supportedRatesFn is a package var so tests can inject a fake capability query.
// It backs ProbeChannels and DeviceInUse, which only need the fast refine-based
// query (channel support and busy detection do not require a rate commit).
var supportedRatesFn = capture.SupportedRates

// verifiedRatesFn backs ProbeRates. It is the HW_PARAMS-committing probe, which
// (unlike refine-only SupportedRates) rejects rates a USB Audio Class device
// advertises but cannot actually deliver, so the UI never offers a rate the
// hardware silently refuses at open. It is a package var so tests can inject a
// fake.
var verifiedRatesFn = capture.SupportedRatesVerified

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

// DetectDevices enumerates the capture devices the host exposes and probes each
// one's supported channel counts and sample rates, so the web UI can list the
// hardware an operator may enable without any prior configuration. Rates are
// verified with a real HW_PARAMS commit (see ProbeRates), so a device never
// advertises a rate it cannot deliver.
//
// Every host device is listed (id and friendly name; enumeration opens nothing),
// but a device whose id is in skip is NOT probed for capabilities: those are the
// ids the configuration already owns, so openAndStart probes them for the
// configured-device UI, and probing a serving device here would only fail busy.
// Listing them without caps still lets a caller tell a device that is present but
// configured (skipped here, hidden from the available view by the provider's
// filter) from one that is genuinely absent from the host. Probing opens
// hardware, so this must run off the capture run-loop goroutine.
//
// Rates are probed at a supported channel count (mono when available, since that
// is the common provisioning default) so a stereo-only device still reports its
// rates.
func DetectDevices(skip map[string]bool) ([]DetectedDevice, error) {
	devs, err := enumerateDevices()
	if err != nil {
		return nil, err
	}
	out := make([]DetectedDevice, 0, len(devs))
	for i := range devs {
		id := devs[i].ID
		d := DetectedDevice{ID: id, FriendlyName: FriendlyName(devs[i].Name)}
		if !skip[id] {
			d.SupportedChannels = ProbeChannels(id, candidateChannels)
			d.SupportedRates = ProbeRates(id, rateProbeChannel(d.SupportedChannels), candidateRates)
		}
		out = append(out, d)
	}
	return out, nil
}

// rateProbeChannel picks the channel count to probe rates at: mono when the
// device supports it (the common case and the provisioning default), else the
// first supported count, else mono as a last resort. Rates can depend on the
// channel count, so probing a stereo-only device at mono would wrongly report no
// rates.
func rateProbeChannel(supported []int) int {
	if len(supported) == 0 {
		return 1
	}
	for _, ch := range supported {
		if ch == 1 {
			return 1
		}
	}
	return supported[0]
}

// ProbeRates returns the subset of candidates the device accepts, for the config
// UI's rate dropdown. It uses go-audio-capture's SupportedRatesVerified, which
// refines to filter the advertised rates and then commits HW_PARAMS on a fresh
// O_NONBLOCK open per candidate to VERIFY each one. This rejects rates a USB Audio
// Class device advertises at refine time but cannot actually deliver (the refine
// lie), which HW_REFINE alone reported as supported. The opens are O_NONBLOCK, so
// the probe still fails fast on a busy device rather than blocking, and it may run
// while the device is free.
//
// It queries both capture formats (S16LE and S32LE) and keeps the union,
// because OpenCaptureAt negotiates the capture format automatically (S16
// preferred, S32 fallback), so a rate a device offers only in S32 is still
// usable. Any query error (device busy or gone, or a format the device rejects)
// simply contributes no rates. When nothing in candidates is supported it
// returns nil so the caller falls back to the static rate list rather than
// reporting a misleading empty set.
func ProbeRates(deviceID string, channels int, candidates []int) []int {
	supported := make(map[int]bool)
	for _, f := range captureFormats {
		rs, err := verifiedRatesFn(deviceID, channels, f)
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

// ProbeChannels returns the subset of candidate channel counts the device accepts,
// for the config UI's Channels control. It reuses the same non-blocking HW_REFINE
// capability query as ProbeRates (no exclusive open), querying each candidate
// against both capture formats and keeping any count at least one format accepts.
//
// A channel count is accepted when SupportedRates returns a nil error: the
// unconstrained refine pins the channel count and succeeds, which proves the
// hardware supports that count for that format, independently of whether any
// standard rate falls in the device's window (so a count usable only at a
// non-standard rate is still reported). A count the device rejects at every
// format yields *BadFormatError from each and is omitted. When nothing is
// determinable (device busy or gone, or capability queries unsupported) it
// returns nil, so the caller falls back to the static [1, 2] list rather than
// reporting a misleading empty set.
func ProbeChannels(deviceID string, candidates []int) []int {
	out := make([]int, 0, len(candidates))
	for _, ch := range candidates {
		for _, f := range captureFormats {
			if _, err := supportedRatesFn(deviceID, ch, f); err == nil {
				out = append(out, ch)
				break
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// DeviceInUse reports whether the device is currently held exclusively by another
// process. It uses the same non-blocking capability query as the probes, which
// opens the device with O_NONBLOCK and so fails promptly with ErrDeviceInUse when
// the device is busy, rather than blocking the way a streaming open can. The
// caller uses it to skip a contended device without stalling on the exclusive
// open.
//
// It probes the configured channel count across both capture formats. A nil
// error, or any error other than ErrDeviceInUse, means the device is not held
// exclusively by another process (a *BadFormatError is only reachable after a
// successful O_NONBLOCK open, and ErrDeviceGone means the device is missing, not
// busy), so the real open should proceed and surface any failure. Only when every
// attempt reports ErrDeviceInUse is the device treated as busy.
func DeviceInUse(deviceID string, channels int) bool {
	busy := false
	for _, f := range captureFormats {
		_, err := supportedRatesFn(deviceID, channels, f)
		if err == nil {
			return false
		}
		if errors.Is(err, capture.ErrDeviceInUse) {
			busy = true
			continue
		}
		// Any other error (BadFormatError, ErrDeviceGone, ErrCapabilitiesUnsupported)
		// means the device is not held exclusively; let the real open decide.
		return false
	}
	return busy
}
