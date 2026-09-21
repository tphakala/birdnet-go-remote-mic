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

// resolveFn is a package var so tests can inject a fake resolution.
var resolveFn = capture.Resolve

// Enumerate lists every capture device the host currently exposes with its
// current identity. Enumeration opens nothing. The id offered for each device is
// its reported ID, except that two identical USB units reporting the same serial
// share one serial-form ID; offeredIDs swaps such a duplicate to its PortID so
// each unit is distinguishable and provisionable (see offeredIDs).
func Enumerate() ([]Hardware, error) {
	devs, err := enumerateDevices()
	if err != nil {
		return nil, err
	}
	ids := offeredIDs(devs)
	out := make([]Hardware, 0, len(devs))
	for i := range devs {
		h := hardwareFrom(&devs[i])
		h.ID = ids[i]
		out = append(out, h)
	}
	return out, nil
}

// offeredIDs returns the id to offer for each enumerated device: its reported
// ID, except when that ID is shared by more than one enumerated device (two
// identical USB units reporting the same serial get the same serial-form ID) and
// the device has a non-empty PortID, in which case the PortID. The capture
// library resolves a PortID and it names the physical port rather than the unit,
// so it distinguishes the twins where the shared serial cannot, and it is
// openable. A duplicate with no derivable PortID keeps the shared ID (there is
// nothing better to offer). A unique device always keeps its own ID.
//
// The offered id is therefore a function of what is plugged in right now: a lone
// unit is offered its serial id, but plugging in a twin makes the same unit be
// offered its port id instead. A caller that persists an offered id (the config)
// must treat both forms as the same hardware; refreshHardware does, claiming a
// resolved unit's PortID and an ambiguous entry's matches alongside its id.
func offeredIDs(devs []capture.DeviceInfo) []string {
	counts := make(map[string]int, len(devs))
	for i := range devs {
		counts[devs[i].ID]++
	}
	ids := make([]string, len(devs))
	for i := range devs {
		ids[i] = devs[i].ID
		if counts[devs[i].ID] > 1 && devs[i].PortID != "" {
			ids[i] = devs[i].PortID
		}
	}
	return ids
}

// Resolve reports which physical device a configured device id names right now,
// without opening it. It accepts every id form the capture library opens: a
// stable id, or a current-boot card index in "hw:N,D", "hw:N", or "N,D" form.
// The error is the library's own, so a caller can tell an absent device
// (*capture.DeviceNotFoundError, which also satisfies errors.Is(err,
// capture.ErrDeviceGone)) from an ambiguous one (*capture.AmbiguousDeviceError)
// and a malformed id (*capture.BadDeviceError). A wholesale enumeration failure
// (no readable device listing) comes back wrapped in capture.ErrDeviceGone and
// matches none of those typed errors.
func Resolve(id string) (Hardware, error) {
	info, err := resolveFn(id)
	if err != nil {
		return Hardware{}, err
	}
	return hardwareFrom(&info), nil
}

// hardwareFrom maps the library's device record to the app's view of it.
func hardwareFrom(d *capture.DeviceInfo) Hardware {
	return Hardware{ID: d.ID, HWAddr: d.HWAddr, Label: FriendlyName(d.Name), IDStable: d.IDStable, PortID: d.PortID}
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
	ids := offeredIDs(devs)
	out := make([]DetectedDevice, 0, len(devs))
	for i := range devs {
		id := ids[i]
		d := DetectedDevice{ID: id, HWAddr: devs[i].HWAddr, IDStable: devs[i].IDStable, FriendlyName: FriendlyName(devs[i].Name)}
		if !skip[id] {
			// Probe by the current-boot address this enumeration just reported, not
			// the stable id: each probe call re-resolves a stable id (a full host
			// enumeration) before opening, so probing by id would re-enumerate the
			// host once per candidate for capabilities the UI only displays. The
			// address names the same device the id enumerated to. Fall back to the id
			// when the device reports no address (no stable form and no card index).
			probeID := devs[i].HWAddr
			if probeID == "" {
				probeID = id
			}
			d.SupportedChannels = ProbeChannels(probeID, candidateChannels)
			d.SupportedRates = ProbeRates(probeID, rateProbeChannel(d.SupportedChannels), candidateRates)
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
// It queries every format in captureFormats (S16LE, S32LE, and the 24-bit
// S24LE / S24_3LE) and keeps the union, because OpenCaptureAt negotiates the
// capture format automatically (S16 preferred, the wider formats as fallbacks),
// so a rate a device offers only in a wider format is still usable. Any query
// error (device busy or gone, or a format the device rejects)
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
// against every format in captureFormats and keeping any count at least one
// format accepts.
//
// A channel count is accepted when SupportedRates returns a nil error: the
// unconstrained refine pins the channel count and succeeds, which proves the
// hardware supports that count for that format, independently of whether any
// standard rate falls in the device's window (so a count usable only at a
// non-standard rate is still reported). A count the device rejects at every
// format in captureFormats yields *BadFormatError from each and is omitted.
// When nothing is determinable (device busy or gone, or capability queries
// unsupported) it returns nil, so the caller falls back to the static [1, 2]
// list rather than reporting a misleading empty set.
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
// It probes the configured channel count across every format in captureFormats.
// A nil error, or any error other than ErrDeviceInUse, means the device is not held
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
