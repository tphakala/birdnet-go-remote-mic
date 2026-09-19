// Package mgmtserver implements the appliance's management API: it adapts live
// runtime state, supplied by a Provider, into the generated mgmtapi contract.
// The generated client and types live in mgmtapi; this package is the server
// side and is never imported by an API consumer.
package mgmtserver

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"sync"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// BasePath is the API version prefix all routes are mounted under.
const BasePath = "/api/v1"

// DeviceState is a device's runtime lifecycle state.
type DeviceState string

const (
	// StateServing means the device is capturing and available over RTSP.
	StateServing DeviceState = "serving"
	// StateSkipped means the device could not be opened at startup.
	StateSkipped DeviceState = "skipped"
	// StateFailed means the device died after startup; its RTSP path 404s.
	StateFailed DeviceState = "failed"
	// StateDisabled means the device is configured but intentionally not opened
	// (its Enabled flag is false). It is not captured or streamed until it is
	// enabled, which a config reload applies at once, no restart needed.
	StateDisabled DeviceState = "disabled"
)

// ApplianceStatus is the appliance-level runtime state the API reports.
type ApplianceStatus struct {
	Version          string
	Uptime           time.Duration
	RTSPListen       string
	DiscoveryEnabled bool
	// AuthRequired is whether a shared access token is configured, so the API,
	// web UI and RTSP stream demand credentials.
	AuthRequired   bool
	DevicesServing int
	DevicesTotal   int
	// Overrides lists the config fields whose effective (running) value differs
	// from the persisted config because a serve CLI flag overrode it. Empty when
	// no serve overrides are active for this run.
	Overrides []ConfigOverride
}

// ConfigOverride is one config field a serve CLI flag overrode for this run:
// the value in force now (Effective) and the value the config file holds
// (Persisted), which a restart without the flag would use.
type ConfigOverride struct {
	Field     string
	Effective string
	Persisted string
}

// DeviceStatus is one device's configuration plus its runtime state. Config
// carries the configured parameters; the remaining fields are live. A
// negotiated value of 0 means the device never opened.
type DeviceStatus struct {
	Config             config.Device
	State              DeviceState
	NegotiatedRate     int
	NegotiatedChannels int
	ClientConnected    bool
	DroppedFrames      int64
	Error              string
	// FriendlyName is a human-facing label derived from the sound card name,
	// empty when the device id matches no enumerated hardware.
	FriendlyName string
	// SupportedRates is the set of sample rates the hardware accepts, probed at
	// startup. Empty when the device could not be probed (missing or busy).
	SupportedRates []int
	// SupportedChannels is the set of channel counts the hardware accepts (up to
	// 8), probed at startup via the same query as SupportedRates. Empty when the
	// device could not be probed (missing or busy).
	SupportedChannels []int
	// Streams is the per-stream live state, one entry per stream fanned out from
	// this device's shared capture, present only while serving. ClientConnected
	// and DroppedFrames above aggregate these (any client connected; summed
	// drops).
	Streams []StreamStatus
}

// StreamStatus is one stream's live state within a serving device.
type StreamStatus struct {
	Path            string
	ClientConnected bool
	DroppedFrames   int64
}

// AvailableDevice is a capture device the host exposes that the configuration
// does not list. It carries the probed capabilities the UI uses to offer the
// device for provisioning; it has no configuration until it is provisioned.
type AvailableDevice struct {
	ID                string
	FriendlyName      string
	SupportedRates    []int
	SupportedChannels []int
}

// Provider supplies live runtime state to the management API. Its methods are
// called from HTTP handler goroutines and must be safe for concurrent use.
type Provider interface {
	// Version is the immutable appliance build version. It is separate from
	// Status so the high-frequency /healthz probe need not snapshot every device.
	Version() string
	Status() ApplianceStatus
	Devices() []DeviceStatus
	// Device returns one device by name, avoiding a full snapshot for the
	// single-device lookup. ok is false when no device has that name.
	Device(name string) (DeviceStatus, bool)
	// AvailableDevices lists capture devices the host exposes that the
	// configuration does not list, for the UI to offer for provisioning.
	AvailableDevices() []AvailableDevice
	// DetectedDevice returns a host device's probed capabilities by id whether or
	// not it is configured, so provisioning can tell an unknown device (404) from
	// an already-configured one (409). ok is false when the host has no such
	// device in the last enumeration.
	DetectedDevice(id string) (AvailableDevice, bool)
}

// Server implements mgmtapi.StrictServerInterface over a Provider.
type Server struct {
	provider      Provider
	eventStream   http.Handler
	configStore   ConfigStore
	system        SystemProvider
	cert          CertProvider
	certMgr       CertManager
	notifications Snapshotter
	notifier      notify.Publisher
	restartFn     func()
	reloader      Reloader
	channelProbe  ChannelProbe
	staticFS      fs.FS
	// guard gates the API routes with the shared bearer token; nil or disabled
	// means open access.
	guard *auth.Guard

	// patchMu serializes a config PATCH's persist-then-reload sequence end to
	// end, so two concurrent patches cannot persist in one order and hot-reload
	// in the other (which would leave disk and the live pipeline disagreeing).
	patchMu sync.Mutex
}

// Option configures a Server.
type Option func(*Server)

// WithEventStream mounts h as the hand-written SSE handler for GET /events,
// beside the generated handlers. The generated streaming stub is a buffered
// response object and cannot stream, so the real event stream lives outside it.
func WithEventStream(h http.Handler) Option {
	return func(s *Server) { s.eventStream = h }
}

// New returns a Server backed by p, applying opts.
func New(p Provider, opts ...Option) *Server {
	s := &Server{provider: p}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

var _ mgmtapi.StrictServerInterface = (*Server)(nil)

// Handler returns the HTTP handler for the management API, with every route
// mounted under /api/v1. Request-binding and body-decode failures, which the
// generated code would otherwise report as text/plain, are rendered as RFC 9457
// problem+json so every error response matches the contract. When staticFS is
// provided, static assets and SPA fallback routing are mounted at /.
func (s *Server) Handler() http.Handler {
	strict := mgmtapi.NewStrictHandlerWithOptions(s, nil, mgmtapi.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeProblem(w, http.StatusBadRequest, "bad request", err.Error())
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeProblem(w, http.StatusInternalServerError, "internal error", err.Error())
		},
	})
	generated := mgmtapi.HandlerWithOptions(strict, mgmtapi.StdHTTPServerOptions{
		BaseURL:    BasePath,
		BaseRouter: http.NewServeMux(),
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeProblem(w, http.StatusBadRequest, "invalid parameter", err.Error())
		},
	})
	// The bearer gate wraps the API subtree only (the generated routes and the
	// hand-written event stream); static assets below stay open. With no guard
	// mounted requireBearer is a no-op passthrough.
	api := requireBearer(s.guard, generated)
	if s.eventStream == nil && s.staticFS == nil {
		return api
	}
	mux := http.NewServeMux()
	if s.eventStream != nil {
		mux.Handle("GET "+BasePath+"/events", requireBearer(s.guard, s.eventStream))
	}
	if s.staticFS != nil {
		mux.Handle(BasePath+"/", api)
		// Without this, the bare base path falls through to the "/" SPA handler
		// and returns index.html 200; redirect it into the API subtree instead.
		mux.Handle(BasePath, http.RedirectHandler(BasePath+"/", http.StatusPermanentRedirect))
		mux.Handle("/", newStaticHandler(s.staticFS))
	} else {
		mux.Handle("/", api)
	}
	return mux
}

// writeProblem sends an RFC 9457 problem detail.
func writeProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem(status, title, detail))
}

// GetHealth handles GET /healthz.
func (s *Server) GetHealth(_ context.Context, _ mgmtapi.GetHealthRequestObject) (mgmtapi.GetHealthResponseObject, error) {
	// Snapshot is nil-safe: without a guard the appliance is open.
	required, _ := s.guard.Snapshot()
	return mgmtapi.GetHealth200JSONResponse{
		Status:       mgmtapi.Ok,
		Version:      s.provider.Version(),
		AuthRequired: required,
	}, nil
}

// GetStatus handles GET /status.
func (s *Server) GetStatus(_ context.Context, _ mgmtapi.GetStatusRequestObject) (mgmtapi.GetStatusResponseObject, error) {
	st := s.provider.Status()
	resp := mgmtapi.GetStatus200JSONResponse{
		Version:          st.Version,
		UptimeSeconds:    int64(st.Uptime.Seconds()),
		RtspListen:       st.RTSPListen,
		DiscoveryEnabled: st.DiscoveryEnabled,
		AuthRequired:     st.AuthRequired,
		DevicesServing:   st.DevicesServing,
		DevicesTotal:     st.DevicesTotal,
	}
	if len(st.Overrides) > 0 {
		ovs := make([]mgmtapi.ConfigOverride, 0, len(st.Overrides))
		for i := range st.Overrides {
			ovs = append(ovs, mgmtapi.ConfigOverride{
				Field:     st.Overrides[i].Field,
				Effective: st.Overrides[i].Effective,
				Persisted: st.Overrides[i].Persisted,
			})
		}
		resp.Overrides = &ovs
	}
	return resp, nil
}

// ListDevices handles GET /devices.
func (s *Server) ListDevices(_ context.Context, _ mgmtapi.ListDevicesRequestObject) (mgmtapi.ListDevicesResponseObject, error) {
	devs := s.provider.Devices()
	out := make(mgmtapi.ListDevices200JSONResponse, 0, len(devs))
	for i := range devs {
		out = append(out, mapDevice(&devs[i]))
	}
	return out, nil
}

// GetDevice handles GET /devices/{name}.
func (s *Server) GetDevice(_ context.Context, request mgmtapi.GetDeviceRequestObject) (mgmtapi.GetDeviceResponseObject, error) {
	if dev, ok := s.provider.Device(request.Name); ok {
		return mgmtapi.GetDevice200JSONResponse(mapDevice(&dev)), nil
	}
	return mgmtapi.GetDevice404ApplicationProblemPlusJSONResponse{
		ProblemApplicationProblemPlusJSONResponse: mgmtapi.ProblemApplicationProblemPlusJSONResponse(
			problem(http.StatusNotFound, "device not found", "no device named "+request.Name),
		),
	}, nil
}

// StreamEvents handles GET /events. Not implemented until a later change.
func (s *Server) StreamEvents(_ context.Context, _ mgmtapi.StreamEventsRequestObject) (mgmtapi.StreamEventsResponseObject, error) {
	return mgmtapi.StreamEventsdefaultApplicationProblemPlusJSONResponse{
		StatusCode: http.StatusNotImplemented,
		Body:       problem(http.StatusNotImplemented, "not implemented", "the event stream is not available yet"),
	}, nil
}

// mapDevice converts a runtime DeviceStatus into the generated wire type. The
// flat path/mode/channels/opus fields project the device's first stream so a
// client that predates fan-out still reads a usable single-stream device; the
// full set is in streams (config) and the per-stream runtime state in the status
// streams array. streamedChannels is the union of every stream's channels, so a
// client can tell which captured channels are carried without walking streams.
func mapDevice(d *DeviceStatus) mgmtapi.Device {
	out := mgmtapi.Device{
		Name:            d.Config.Name,
		Device:          d.Config.Device,
		Format:          mgmtapi.DeviceFormat(d.Config.Format),
		Rate:            d.Config.Rate,
		State:           mgmtapi.DeviceState(d.State),
		ClientConnected: d.ClientConnected,
		DroppedFrames:   d.DroppedFrames,
	}
	if len(d.Config.Streams) > 0 {
		s0 := &d.Config.Streams[0]
		out.Path = s0.Path
		out.Mode = mapMode(s0.Mode)
		out.Channels = s0.Channels
		if s0.Mode == config.ModeOpus {
			out.Opus = &mgmtapi.OpusSettings{Bitrate: ptr(s0.Opus.Bitrate)}
		}
		out.StreamedChannels = ptr(d.Config.StreamChannelUnion())
	}
	if len(d.Streams) > 0 {
		ss := make([]mgmtapi.StreamStatus, 0, len(d.Streams))
		for i := range d.Streams {
			ss = append(ss, mgmtapi.StreamStatus{
				Path:            d.Streams[i].Path,
				ClientConnected: d.Streams[i].ClientConnected,
				DroppedFrames:   d.Streams[i].DroppedFrames,
			})
		}
		out.Streams = &ss
	}
	if d.NegotiatedRate > 0 {
		out.NegotiatedRate = ptr(d.NegotiatedRate)
	}
	if d.NegotiatedChannels > 0 {
		out.NegotiatedChannels = ptr(d.NegotiatedChannels)
	}
	if d.Error != "" {
		out.Error = ptr(d.Error)
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

// mapMode converts a config stream mode to the wire enum.
func mapMode(m config.Mode) mgmtapi.StreamMode {
	if m == config.ModeOpus {
		return mgmtapi.Opus
	}
	return mgmtapi.Pcm
}

// problem builds an RFC 9457 problem detail.
func problem(status int, title, detail string) mgmtapi.Problem {
	return mgmtapi.Problem{
		Status: ptr(status),
		Title:  ptr(title),
		Detail: ptr(detail),
	}
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }
