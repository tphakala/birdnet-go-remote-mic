package mgmtserver

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
)

// ChannelProbe briefly captures from an unconfigured host device, opened at the
// given rate and channel count, and returns each channel's RMS level in dBFS
// (index 0 is channel 1). Provisioning uses it to default a new device to its
// loudest channel.
type ChannelProbe func(ctx context.Context, device string, rate, channels int) ([]float64, error)

// WithChannelProbe mounts fn so provisioning without an explicit channel
// selection defaults to the device's loudest channel. Without it, or when the
// probe fails, the default is channel 1.
func WithChannelProbe(fn ChannelProbe) Option {
	return func(s *Server) { s.channelProbe = fn }
}

// Loudest-channel selection. Levels within channelTieDb of the loudest count as
// equal, and levels at or below channelSilenceDbfs as silence, so two idle
// inputs whose noise floors differ slightly do not pick the higher-numbered one
// by chance: every tie resolves to the lowest channel number.
const (
	channelTieDb       = 1.0
	channelSilenceDbfs = -80.0
	channelProbeWidth  = config.MaxChannels // widest capture probed (the config channel maximum)
	channelProbeBudget = 3 * time.Second    // bounds the probe: its blocking open, the settle window, and the measurement
)

// loudestChannel returns the 1-based channel with the highest RMS level in
// levels (dBFS, index 0 is channel 1). Channels within channelTieDb of the
// loudest are ties and the lowest-numbered of them wins; when every channel is
// at or below channelSilenceDbfs, or levels is empty, it returns channel 1.
func loudestChannel(levels []float64) int {
	// Ignore any NaN reading. The RMS computation that feeds this cannot produce a
	// NaN, but the policy is made explicit here: slices.Max would propagate a NaN
	// into top, and every comparison below against a NaN top is false, so the
	// function would silently fall through to channel 1. Skipping NaN means a real
	// reading always decides the winner, and a channel with no real reading never
	// wins.
	top := math.Inf(-1)
	for _, l := range levels {
		if !math.IsNaN(l) && l > top {
			top = l
		}
	}
	if top <= channelSilenceDbfs {
		return 1
	}
	for i, l := range levels {
		if !math.IsNaN(l) && l >= top-channelTieDb {
			return i + 1
		}
	}
	return 1
}

// preferredChannel measures the device and returns its loudest channel, or 1
// when there is nothing to choose between (a single-channel device), no probe
// is mounted, the rate provisioning would pick falls outside the probe's band,
// or the measurement fails (the device is busy, say). It captures at the rate
// provisioning will pick (chooseParams), so the probe exercises the real open,
// and considers at most channelProbeWidth channels.
//
// The supported channel counts are device-wide, and some interfaces offer
// fewer channels at their higher rates, so the widest count can fail to open
// at the chosen rate. The probe then steps down through the narrower supported
// counts (still two or more) within the same budget before giving up.
func (s *Server) preferredChannel(ctx context.Context, d *AvailableDevice, req *mgmtapi.ProvisionDeviceRequest) int {
	widths := probeWidths(d.SupportedChannels)
	if s.channelProbe == nil || len(widths) == 0 {
		return 1
	}
	// Probe at the rate provisioning will actually pick so the open matches the
	// real one: chooseParams owns that derivation (Opus is always 48 kHz, PCM
	// takes the requested or preferred rate). preferred does not affect the rate,
	// so pass 1. Skip an implausible rate no capture would accept.
	_, rate, _ := chooseParams(d, req, 1)
	if rate < 8000 || rate > 384000 {
		return 1
	}
	pctx, cancel := context.WithTimeout(ctx, channelProbeBudget)
	defer cancel()
	// Keep every width's error: the widest attempt's is usually the telling one.
	var errs []error
	for _, width := range widths {
		levels, err := s.channelProbe(pctx, d.ID, rate, width)
		if err == nil {
			return loudestChannel(levels[:min(len(levels), width)])
		}
		errs = append(errs, fmt.Errorf("%d channels: %w", width, err))
		if pctx.Err() != nil {
			break
		}
	}
	log.Printf("mgmtserver: channel level probe on %s failed: %v (defaulting to channel 1)", d.ID, errors.Join(errs...))
	return 1
}

// probeWidths returns the channel counts to probe, widest first: every
// supported count of two or more, capped at channelProbeWidth. With no probed
// counts it returns the cap alone, and with nothing to choose between (a
// single-channel device) it returns none.
func probeWidths(supported []int) []int {
	if len(supported) == 0 {
		return []int{channelProbeWidth}
	}
	var widths []int
	for _, c := range supported {
		if w := min(c, channelProbeWidth); w >= 2 && !slices.Contains(widths, w) {
			widths = append(widths, w)
		}
	}
	slices.SortFunc(widths, func(a, b int) int { return cmp.Compare(b, a) })
	return widths
}

// errDeviceExists and errDeviceNotFound distinguish a provisioning conflict and
// a missing delete target from a validation failure, so the handlers can map
// them to 409 and 404 rather than 422 or 500.
var (
	errDeviceExists   = errors.New("device already configured")
	errDeviceNotFound = errors.New("device not found")
)

// ListAvailableDevices handles GET /devices/available. It reports the host's
// capture devices that the configuration does not list, so the UI can offer
// them for provisioning.
func (s *Server) ListAvailableDevices(_ context.Context, _ mgmtapi.ListAvailableDevicesRequestObject) (mgmtapi.ListAvailableDevicesResponseObject, error) {
	devs := s.provider.AvailableDevices()
	out := make(mgmtapi.ListAvailableDevices200JSONResponse, 0, len(devs))
	for i := range devs {
		out = append(out, mapAvailableDevice(&devs[i]))
	}
	return out, nil
}

// ProvisionDevice handles POST /devices. It enables a detected but unconfigured
// device: the appliance derives a name, a hard-to-guess RTSP path, and stream
// parameters from the device's capabilities (any of which the request may
// override), appends it to the configuration, and hot-applies the change.
func (s *Server) ProvisionDevice(ctx context.Context, request mgmtapi.ProvisionDeviceRequestObject) (mgmtapi.ProvisionDeviceResponseObject, error) {
	if s.configStore == nil {
		return mgmtapi.ProvisionDevicedefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusNotImplemented,
			Body:       problem(http.StatusNotImplemented, "not implemented", "provisioning devices is not available"),
		}, nil
	}
	req := request.Body
	if req != nil {
		// Compare and persist the id exactly as the host reports it, so stray
		// whitespace cannot slip a second entry for one device past the
		// duplicate checks below.
		trimmed := *req
		trimmed.Device = strings.TrimSpace(req.Device)
		req = &trimmed
	}
	if req == nil || req.Device == "" {
		return mgmtapi.ProvisionDevice422ApplicationProblemPlusJSONResponse(mgmtapi.ValidationProblem{
			Status: ptr(http.StatusUnprocessableEntity),
			Title:  ptr("invalid request"),
			Detail: ptr("device is required"),
			Errors: &[]struct {
				Field  string `json:"field"`
				Reason string `json:"reason"`
			}{{Field: "device", Reason: "must not be empty"}},
		}), nil
	}

	// The device must be one the host currently exposes. Look it up in the full
	// detected set (not the available-only view), so a device that IS present but
	// already configured falls through to the 409 conflict below rather than being
	// misreported as a 404. A miss here means the host genuinely has no such id.
	dd, ok := s.provider.DetectedDevice(req.Device)
	if !ok {
		return deviceGone(req.Device), nil
	}
	detected := &dd

	// A device that is already configured needs no probe: opening it would
	// disturb its running capture. The authoritative duplicate rejection is the
	// check inside Update below, which holds the store lock and matches the EXACT
	// id, so a concurrent provision cannot slip past it. The two checks here run
	// before that lock and are best effort: they catch the common cases early and
	// skip the wasted, disruptive probe.
	//
	// First, the exact id is already in the config.
	if slices.ContainsFunc(s.configStore.Config().Devices, func(dev config.Device) bool {
		return dev.Device == req.Device
	}) {
		return mgmtapi.ProvisionDevice409ApplicationProblemPlusJSONResponse(
			problem(http.StatusConflict, "already configured", "device "+req.Device+" is already configured"),
		), nil
	}
	// Second, the host lists the device but the available view hides it, which
	// means the config already owns it under ANOTHER id (for example a card-index
	// id that resolves to it); provisioning it again would open one device from
	// two entries. This is re-checked under patchMu below.
	if resp := s.unavailable(req.Device); resp != nil {
		return resp, nil
	}

	// With no channel selection in the request, default to the loudest channel.
	// Measure before taking patchMu: the probe captures for up to a second and
	// must not hold up concurrent config changes.
	preferred := 1
	if req.Channels == nil || len(*req.Channels) == 0 {
		preferred = s.preferredChannel(ctx, detected, req)
	}

	// If the client disconnected during the probe, do not persist a device it can
	// no longer learn about; a 503 lets it retry. It is checked after taking
	// patchMu, which can also wait (behind another request's reload), so both
	// waits are covered.
	//
	// Serialize the persist-then-reload sequence exactly like PatchConfig, so a
	// provision and a concurrent patch cannot interleave persist and reload.
	s.patchMu.Lock()
	defer s.patchMu.Unlock()
	if ctx.Err() != nil {
		return mgmtapi.ProvisionDevicedefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusServiceUnavailable,
			Body:       problem(http.StatusServiceUnavailable, "request cancelled", "the provisioning request was cancelled before the device was saved"),
		}, nil
	}
	// Repeat the alias check now that patchMu is held. A PATCH or another
	// provision that ran during the probe has finished its persist AND its hot
	// reload by the time this lock is ours (the reloader blocks until the
	// reconcile replies, unless that request's own client gave up first), and a
	// reconcile republishes the ids the config owns, resolved aliases included,
	// before it opens anything. So a device claimed meanwhile under another id is
	// hidden from the available view here; with only the pre-lock check, a
	// concurrent alias could persist two entries for one device.
	if resp := s.unavailable(req.Device); resp != nil {
		return resp, nil
	}

	var created config.Device
	err := s.configStore.Update(func(cur config.Config) (config.Config, error) {
		prev := cur.Clone() // the stored config, for validateWrite's length caps
		for i := range cur.Devices {
			if cur.Devices[i].Device == req.Device {
				return config.Config{}, errDeviceExists
			}
		}
		dev := buildProvisionedDevice(&cur, detected, req, preferred)
		cur.Devices = append(cur.Devices, dev)
		cur.ApplyDefaults()
		if verr := validateWrite(&cur, &prev); verr != nil {
			return config.Config{}, verr
		}
		created = dev
		return cur, nil
	})
	if err != nil {
		switch {
		case errors.Is(err, errDeviceExists):
			return mgmtapi.ProvisionDevice409ApplicationProblemPlusJSONResponse(
				problem(http.StatusConflict, "already configured", "device "+req.Device+" is already configured"),
			), nil
		case isValidationError(err):
			var verr *config.ValidationError
			errors.As(err, &verr)
			return mgmtapi.ProvisionDevice422ApplicationProblemPlusJSONResponse(validationProblem(verr)), nil
		default:
			return mgmtapi.ProvisionDevicedefaultApplicationProblemPlusJSONResponse{
				StatusCode: http.StatusInternalServerError,
				Body:       problem(http.StatusInternalServerError, "persist failed", err.Error()),
			}, nil
		}
	}

	cur := s.configStore.Config()
	if s.reloader != nil {
		if rerr := s.reloader(ctx, cur); rerr != nil {
			log.Printf("mgmtserver: device provisioned but hot reload failed: %v (a restart will apply it)", rerr)
		}
	}

	// Report the new device's live state when the reload has published it,
	// otherwise its persisted configuration.
	if st, ok := s.provider.Device(created.Name); ok {
		return mgmtapi.ProvisionDevice201JSONResponse(mapDevice(&st)), nil
	}
	return mgmtapi.ProvisionDevice201JSONResponse(configDeviceToWireDevice(&created)), nil
}

// unavailable returns the response for a host device id missing from the
// available view, or nil when it is available (detected and not owned by the
// config under any id). The view hides both a device the config owns under
// another id and one that has left the host, so the detected view tells them
// apart: gone (404), not owned under another id (409). Both views come from
// the last background enumeration, so a device unplugged moments ago may still
// read as available and be provisioned; it then shows as not connected, like
// any configured device that is unplugged (and, bound by a stable id, starts
// when plugged back in).
func (s *Server) unavailable(id string) mgmtapi.ProvisionDeviceResponseObject {
	if slices.ContainsFunc(s.provider.AvailableDevices(), func(ad AvailableDevice) bool { return ad.ID == id }) {
		return nil
	}
	if _, detected := s.provider.DetectedDevice(id); !detected {
		return deviceGone(id)
	}
	return aliasConflict(id)
}

// aliasConflict is the 409 for a detected device the config already owns under
// another id.
func aliasConflict(id string) mgmtapi.ProvisionDeviceResponseObject {
	return mgmtapi.ProvisionDevice409ApplicationProblemPlusJSONResponse(
		problem(http.StatusConflict, "already configured", "device "+id+" is already set up as another entry"),
	)
}

// deviceGone is the 404 for an id the host does not (or no longer) offer: it
// was unplugged, or its offered id changed when an identical unit appeared.
func deviceGone(id string) mgmtapi.ProvisionDeviceResponseObject {
	return mgmtapi.ProvisionDevice404ApplicationProblemPlusJSONResponse(
		problem(http.StatusNotFound, "device not found", "no capture device with id "+id+
			" is present; it may have been unplugged or re-detected under a new id"),
	)
}

// DeleteDevice handles DELETE /devices/{name}. It removes the named device from
// the configuration (stopping it if serving) and hot-applies the change, so its
// hardware returns to the pool of available devices.
func (s *Server) DeleteDevice(ctx context.Context, request mgmtapi.DeleteDeviceRequestObject) (mgmtapi.DeleteDeviceResponseObject, error) {
	if s.configStore == nil {
		return mgmtapi.DeleteDevicedefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusNotImplemented,
			Body:       problem(http.StatusNotImplemented, "not implemented", "removing devices is not available"),
		}, nil
	}

	s.patchMu.Lock()
	defer s.patchMu.Unlock()

	err := s.configStore.Update(func(cur config.Config) (config.Config, error) {
		kept := make([]config.Device, 0, len(cur.Devices))
		for i := range cur.Devices {
			if cur.Devices[i].Name == request.Name {
				continue
			}
			kept = append(kept, cur.Devices[i])
		}
		if len(kept) == len(cur.Devices) {
			return config.Config{}, errDeviceNotFound
		}
		cur.Devices = kept
		cur.ApplyDefaults()
		// Plain Validate, not validateWrite: a removal adds no string, so an
		// over-cap value elsewhere (kept loadable on disk) must not block it.
		if verr := cur.Validate(); verr != nil {
			return config.Config{}, verr
		}
		return cur, nil
	})
	if err != nil {
		if errors.Is(err, errDeviceNotFound) {
			return mgmtapi.DeleteDevice404ApplicationProblemPlusJSONResponse{
				ProblemApplicationProblemPlusJSONResponse: mgmtapi.ProblemApplicationProblemPlusJSONResponse(
					problem(http.StatusNotFound, "device not found", "no device named "+request.Name),
				),
			}, nil
		}
		return mgmtapi.DeleteDevicedefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusInternalServerError,
			Body:       problem(http.StatusInternalServerError, "persist failed", err.Error()),
		}, nil
	}

	cur := s.configStore.Config()
	if s.reloader != nil {
		if rerr := s.reloader(ctx, cur); rerr != nil {
			log.Printf("mgmtserver: device removed but hot reload failed: %v (a restart will apply it)", rerr)
		}
	}
	return mgmtapi.DeleteDevice204Response{}, nil
}

// buildProvisionedDevice materializes a full config.Device for a detected device:
// a unique name derived from the hardware label (never a channel-mode suffix), a
// random hard-to-guess RTSP path, and stream parameters chosen from the device's
// capabilities. Request fields override the derived defaults. config.Validate
// (run by the caller) still guards the result.
func buildProvisionedDevice(cur *config.Config, d *AvailableDevice, req *mgmtapi.ProvisionDeviceRequest, preferred int) config.Device {
	names := make(map[string]bool, len(cur.Devices))
	paths := make(map[string]bool, len(cur.Devices))
	for i := range cur.Devices {
		names[cur.Devices[i].Name] = true
		for j := range cur.Devices[i].Streams {
			paths[cur.Devices[i].Streams[j].Path] = true
		}
	}

	name := ""
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if name == "" {
		name = deriveName(d.FriendlyName, d.ID, names)
	}

	mode, rate, channels := chooseParams(d, req, preferred)

	// Provisioning always creates a single-stream device: one capture, one RTSP
	// path, the chosen channels. Fan-out is added later by editing the device.
	return config.Device{
		Name:   name,
		Device: d.ID,
		Rate:   rate,
		Format: "s16",
		Streams: []config.Stream{{
			Path:     randomPath(paths),
			Mode:     mode,
			Channels: channels,
		}},
	}
}

// nameSuffixReserve is the room deriveName keeps for a "-N" collision suffix
// when it shortens a base name to fit config.MaxNameLen.
const nameSuffixReserve = 8

// deriveName turns a hardware label into a unique, config-safe device name. It
// slugifies the friendly name (falling back to the device id, then "device"),
// shortens it to fit config.MaxNameLen with room for a suffix, then appends a
// numeric suffix on collision. It deliberately never encodes the channel count
// or mode: one device is one entry, named for the hardware.
func deriveName(friendly, id string, taken map[string]bool) string {
	base := slug(friendly)
	if base == "" {
		base = slug(id)
	}
	if base == "" {
		base = "device"
	}
	// A slug is ASCII, so a byte cut is a character cut. The room kept for a
	// "-N" suffix means no derived name exceeds config.MaxNameLen, which
	// provisioning enforces: a slug of a long device id would otherwise make the
	// device unprovisionable.
	if limit := config.MaxNameLen - nameSuffixReserve; len(base) > limit {
		base = strings.TrimRight(base[:limit], "-")
	}
	name := base
	for i := 2; taken[name]; i++ {
		name = base + "-" + strconv.Itoa(i)
	}
	return name
}

// slug lowercases s and collapses every run of characters that are not ASCII
// letters or digits into a single hyphen, trimming leading and trailing hyphens.
func slug(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// randRead is the entropy source randomPath draws from; a test swaps it to force
// a collision and prove the retry loop.
var randRead = rand.Read

// randomPath returns an RTSP path of the form /<16 hex chars> that is not already
// in taken. The 64 bits of entropy make the stream URL impractical to guess,
// which is the stream's only protection when no token is configured, and
// collisions vanishingly rare; the loop retries the astronomically unlikely
// collision. A crypto/rand read failure is a fatal environment fault, so it
// panics rather than returning a guessable fallback.
func randomPath(taken map[string]bool) string {
	for {
		var buf [8]byte
		if _, err := randRead(buf[:]); err != nil {
			panic(fmt.Sprintf("mgmtserver: crypto/rand failed: %v", err))
		}
		p := "/" + hex.EncodeToString(buf[:])
		if !taken[p] {
			return p
		}
	}
}

// chooseParams selects the stream mode, sample rate and channel count for a newly
// provisioned device. A request field overrides its derived default. When the
// mode is unspecified it defaults to Opus if the device is known to support 48
// kHz, otherwise raw PCM at the device's best rate. Opus is fixed at 48 kHz, with
// a deliberate asymmetry between the two overridable stream parameters: an
// explicit rate is snapped to 48000 (a non-48k rate in auto mode is instead read
// as "the operator wants PCM"), while an explicit channel selection is passed
// through unchanged so config.Validate accepts one or two channels and rejects
// three or more with a 422 rather than this silently narrowing the request.
func chooseParams(d *AvailableDevice, req *mgmtapi.ProvisionDeviceRequest, preferred int) (mode config.Mode, rate int, channels []int) {
	if req.Mode != nil {
		mode = config.Mode(string(*req.Mode))
	}
	if req.Rate != nil {
		rate = *req.Rate
	}
	if req.Channels != nil {
		channels = config.NormalizeChannels(*req.Channels)
	}
	// An omitted field and an explicit empty array (channels: []) both leave
	// nothing after normalization and are treated the same: the selection is
	// derived, and a derived selection is always a single channel (preferred,
	// normally the loudest). The selecting source extracts it from whatever
	// contiguous count the device opens, so this holds on a stereo-only
	// interface too.
	if len(channels) == 0 {
		channels = []int{max(1, preferred)}
	}

	switch mode {
	case config.ModeOpus:
		// Opus accepts one channel (mono) or two (stereo). An explicit selection
		// is kept as asked, so config.Validate accepts one or two and rejects
		// three or more with a 422 instead of this silently discarding part of
		// the request.
		return config.ModeOpus, 48000, channels
	case config.ModePCM:
		if rate == 0 {
			rate = preferRate(d.SupportedRates)
		}
		return config.ModePCM, rate, channels
	default:
		// Auto: prefer Opus, but only when the request did not ask for something
		// Opus cannot honor. Auto prefers 48 kHz mono Opus when a single channel is
		// selected; an explicit rate other than 48 kHz, or an explicit selection of
		// more than one channel, means the operator wants PCM, so silently returning Opus would discard
		// their request (the contract says a set field overrides the default). Stereo
		// Opus is reachable only by asking for it explicitly (mode opus, two channels).
		opusOK := canOpus(d) &&
			(req.Rate == nil || *req.Rate == 48000) &&
			len(channels) == 1
		if opusOK {
			return config.ModeOpus, 48000, channels
		}
		if rate == 0 {
			rate = preferRate(d.SupportedRates)
		}
		return config.ModePCM, rate, channels
	}
}

// canOpus reports whether the device is known to support the 48 kHz Opus needs.
// A single-channel (mono) stream is always achievable by selecting one channel
// (the selecting source extracts it from whatever contiguous count the device
// opens), so only 48 kHz support is load-bearing here. It requires positive
// evidence: an unprobed device (empty rate list) is not assumed to support 48
// kHz, since a wrong Opus guess is rejected at open, whereas PCM at the fallback
// rate is more forgiving.
func canOpus(d *AvailableDevice) bool {
	return slices.Contains(d.SupportedRates, 48000)
}

// preferRate picks a sample rate: 48 kHz when supported (normal audio), otherwise
// the highest supported rate (an ultrasonic-only device), falling back to 48 kHz
// when the rates are unknown.
func preferRate(supported []int) int {
	if len(supported) == 0 || slices.Contains(supported, 48000) {
		return 48000
	}
	best := supported[0]
	for _, r := range supported[1:] {
		if r > best {
			best = r
		}
	}
	return best
}

func isValidationError(err error) bool {
	var verr *config.ValidationError
	return errors.As(err, &verr)
}

// mapAvailableDevice converts a runtime AvailableDevice into the generated wire
// type.
func mapAvailableDevice(d *AvailableDevice) mgmtapi.AvailableDevice {
	out := mgmtapi.AvailableDevice{
		Device:   d.ID,
		State:    mgmtapi.Available,
		IdStable: ptr(d.IDStable),
	}
	if d.HWAddr != "" {
		out.HwAddr = ptr(d.HWAddr)
	}
	if d.FriendlyName != "" {
		out.FriendlyName = ptr(d.FriendlyName)
	}
	if len(d.SupportedRates) > 0 {
		rates := append([]int(nil), d.SupportedRates...)
		out.SupportedRates = &rates
	}
	if len(d.SupportedChannels) > 0 {
		chans := append([]int(nil), d.SupportedChannels...)
		out.SupportedChannels = &chans
	}
	return out
}

// configDeviceToWireDevice builds a wire Device from a configured device with
// zeroed runtime fields. It is the fallback for the 201 response only when the
// provider has NOT published the just-provisioned device, which happens when the
// hot reload failed and the change awaits a restart. It therefore reports the
// device as "skipped" (persisted but not currently serving), not "serving"; the
// next GET /devices carries the true live state once the device opens.
func configDeviceToWireDevice(d *config.Device) mgmtapi.Device {
	out := mgmtapi.Device{
		Name:     d.Name,
		Device:   d.Device,
		Format:   mgmtapi.DeviceFormat(d.Format),
		Rate:     d.Rate,
		State:    mgmtapi.DeviceStateSkipped,
		IdStable: ptr(!config.IsCardIndexID(d.Device)),
	}
	// A freshly provisioned device is single-stream, so the flat projection of its
	// first stream is complete; no per-stream runtime status exists yet.
	projectFirstStream(&out, d)
	return out
}
