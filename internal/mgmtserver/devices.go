package mgmtserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
)

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
	if req == nil || strings.TrimSpace(req.Device) == "" {
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
		return mgmtapi.ProvisionDevice404ApplicationProblemPlusJSONResponse(
			problem(http.StatusNotFound, "device not found", "no capture device with id "+req.Device+" is present on the host"),
		), nil
	}
	detected := &dd

	// Serialize the persist-then-reload sequence exactly like PatchConfig, so a
	// provision and a concurrent patch cannot interleave persist and reload.
	s.patchMu.Lock()
	defer s.patchMu.Unlock()

	var created config.Device
	err := s.configStore.Update(func(cur config.Config) (config.Config, error) {
		for i := range cur.Devices {
			if cur.Devices[i].Device == req.Device {
				return config.Config{}, errDeviceExists
			}
		}
		dev := buildProvisionedDevice(&cur, detected, req)
		cur.Devices = append(cur.Devices, dev)
		cur.ApplyDefaults()
		if verr := cur.Validate(); verr != nil {
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
func buildProvisionedDevice(cur *config.Config, d *AvailableDevice, req *mgmtapi.ProvisionDeviceRequest) config.Device {
	names := make(map[string]bool, len(cur.Devices))
	paths := make(map[string]bool, len(cur.Devices))
	for i := range cur.Devices {
		names[cur.Devices[i].Name] = true
		paths[cur.Devices[i].Path] = true
	}

	name := ""
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if name == "" {
		name = deriveName(d.FriendlyName, d.ID, names)
	}

	mode, rate, channels := chooseParams(d, req)

	return config.Device{
		Name:     name,
		Device:   d.ID,
		Path:     randomPath(paths),
		Mode:     mode,
		Rate:     rate,
		Channels: channels,
		Format:   "s16",
	}
}

// deriveName turns a hardware label into a unique, config-safe device name. It
// slugifies the friendly name (falling back to the device id, then "device"),
// then appends a numeric suffix on collision. It deliberately never encodes the
// channel count or mode: one device is one entry, named for the hardware.
func deriveName(friendly, id string, taken map[string]bool) string {
	base := slug(friendly)
	if base == "" {
		base = slug(id)
	}
	if base == "" {
		base = "device"
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

// randomPath returns an RTSP path of the form /<16 hex chars> that is not already
// in taken. The 64 bits of entropy make the stream URL impractical to guess
// (security by obscurity for an unauthenticated stream) and collisions vanishing
// rare; the loop retries the astronomically unlikely collision. A crypto/rand
// read failure is a fatal environment fault, so it panics rather than returning a
// guessable fallback.
func randomPath(taken map[string]bool) string {
	for {
		var buf [8]byte
		if _, err := rand.Read(buf[:]); err != nil {
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
// mode is unspecified it defaults to Opus (48 kHz mono) if the device is known to
// support that, otherwise raw PCM at the device's best rate. Opus forces 48 kHz
// mono per its contract.
func chooseParams(d *AvailableDevice, req *mgmtapi.ProvisionDeviceRequest) (mode config.Mode, rate int, channels []int) {
	if req.Mode != nil {
		mode = config.Mode(string(*req.Mode))
	}
	if req.Rate != nil {
		rate = *req.Rate
	}
	if req.Channels != nil {
		channels = config.NormalizeChannels(*req.Channels)
	}
	if len(channels) == 0 {
		channels = defaultSelection(d.SupportedChannels)
	}

	switch mode {
	case config.ModeOpus:
		return config.ModeOpus, 48000, opusSelection(channels)
	case config.ModePCM:
		if rate == 0 {
			rate = preferRate(d.SupportedRates)
		}
		return config.ModePCM, rate, channels
	default:
		// Auto: prefer Opus, but only when the request did not ask for something
		// Opus cannot honor. Opus is fixed at 48 kHz mono (exactly one selected
		// channel), so an explicit rate other than 48 kHz or a multi-channel
		// selection means the operator wants PCM; silently returning Opus would
		// discard their request (the contract says a set field overrides the
		// default).
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

// opusSelection reduces a channel selection to the single channel Opus streams.
// A single-channel request (mono from any channel, e.g. [3]) is honored verbatim
// rather than silently rewritten to channel 1; a multi-channel selection collapses
// to its first channel, since Opus is single-channel and config.Validate would
// otherwise reject len != 1.
func opusSelection(channels []int) []int {
	if len(channels) == 1 {
		return channels
	}
	return []int{channels[0]}
}

// defaultSelection picks a channel selection for a newly provisioned device:
// mono (channel 1) when the device can open a single channel or its capability is
// unknown (the common default, and the one Opus accepts), otherwise the device's
// full native width so no channel is silently dropped on a stereo-only (or wider)
// interface that cannot open mono directly.
func defaultSelection(supported []int) []int {
	if len(supported) == 0 || slices.Contains(supported, 1) {
		return []int{1}
	}
	maxCh := slices.Max(supported)
	sel := make([]int, maxCh)
	for i := range sel {
		sel[i] = i + 1
	}
	return sel
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
		Device: d.ID,
		State:  mgmtapi.Available,
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
		Path:     d.Path,
		Mode:     mapMode(d.Mode),
		Format:   mgmtapi.DeviceFormat(d.Format),
		Rate:     d.Rate,
		Channels: d.Channels,
		State:    mgmtapi.Skipped,
	}
	if d.Mode == config.ModeOpus {
		out.Opus = &mgmtapi.OpusSettings{Bitrate: ptr(d.Opus.Bitrate)}
	}
	return out
}
