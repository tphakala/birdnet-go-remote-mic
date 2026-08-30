// Package mgmtserver implements the appliance's management API: it adapts live
// runtime state, supplied by a Provider, into the generated mgmtapi contract.
// The generated client and types live in mgmtapi; this package is the server
// side and is never imported by an API consumer.
package mgmtserver

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
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
)

// ApplianceStatus is the appliance-level runtime state the API reports.
type ApplianceStatus struct {
	Version          string
	Uptime           time.Duration
	RTSPListen       string
	DiscoveryEnabled bool
	DevicesServing   int
	DevicesTotal     int
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
}

// Provider supplies live runtime state to the management API. Its methods are
// called from HTTP handler goroutines and must be safe for concurrent use.
type Provider interface {
	// Version is the immutable appliance build version. It is separate from
	// Status so the high-frequency /healthz probe need not snapshot every device.
	Version() string
	Status() ApplianceStatus
	Devices() []DeviceStatus
}

// Server implements mgmtapi.StrictServerInterface over a Provider.
type Server struct {
	provider Provider
}

// New returns a Server backed by p.
func New(p Provider) *Server {
	return &Server{provider: p}
}

var _ mgmtapi.StrictServerInterface = (*Server)(nil)

// Handler returns the HTTP handler for the management API, with every route
// mounted under /api/v1. Request-binding and body-decode failures, which the
// generated code would otherwise report as text/plain, are rendered as RFC 9457
// problem+json so every error response matches the contract.
func (s *Server) Handler() http.Handler {
	strict := mgmtapi.NewStrictHandlerWithOptions(s, nil, mgmtapi.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeProblem(w, http.StatusBadRequest, "bad request", err.Error())
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeProblem(w, http.StatusInternalServerError, "internal error", err.Error())
		},
	})
	return mgmtapi.HandlerWithOptions(strict, mgmtapi.StdHTTPServerOptions{
		BaseURL:    BasePath,
		BaseRouter: http.NewServeMux(),
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeProblem(w, http.StatusBadRequest, "invalid parameter", err.Error())
		},
	})
}

// writeProblem sends an RFC 9457 problem detail.
func writeProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem(status, title, detail))
}

// GetHealth handles GET /healthz.
func (s *Server) GetHealth(_ context.Context, _ mgmtapi.GetHealthRequestObject) (mgmtapi.GetHealthResponseObject, error) {
	return mgmtapi.GetHealth200JSONResponse{
		Status:  mgmtapi.Ok,
		Version: s.provider.Version(),
	}, nil
}

// GetStatus handles GET /status.
func (s *Server) GetStatus(_ context.Context, _ mgmtapi.GetStatusRequestObject) (mgmtapi.GetStatusResponseObject, error) {
	st := s.provider.Status()
	return mgmtapi.GetStatus200JSONResponse{
		Version:          st.Version,
		UptimeSeconds:    int64(st.Uptime.Seconds()),
		RtspListen:       st.RTSPListen,
		DiscoveryEnabled: st.DiscoveryEnabled,
		DevicesServing:   st.DevicesServing,
		DevicesTotal:     st.DevicesTotal,
	}, nil
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
	devs := s.provider.Devices()
	for i := range devs {
		if devs[i].Config.Name == request.Name {
			return mgmtapi.GetDevice200JSONResponse(mapDevice(&devs[i])), nil
		}
	}
	return mgmtapi.GetDevice404ApplicationProblemPlusJSONResponse{
		ProblemApplicationProblemPlusJSONResponse: mgmtapi.ProblemApplicationProblemPlusJSONResponse(
			problem(http.StatusNotFound, "device not found", "no device named "+request.Name),
		),
	}, nil
}

// GetConfig handles GET /config. Not implemented until a later change.
func (s *Server) GetConfig(_ context.Context, _ mgmtapi.GetConfigRequestObject) (mgmtapi.GetConfigResponseObject, error) {
	return mgmtapi.GetConfigdefaultApplicationProblemPlusJSONResponse{
		StatusCode: http.StatusNotImplemented,
		Body:       problem(http.StatusNotImplemented, "not implemented", "reading configuration is not available yet"),
	}, nil
}

// PatchConfig handles PATCH /config. Not implemented until a later change.
func (s *Server) PatchConfig(_ context.Context, _ mgmtapi.PatchConfigRequestObject) (mgmtapi.PatchConfigResponseObject, error) {
	return mgmtapi.PatchConfigdefaultApplicationProblemPlusJSONResponse{
		StatusCode: http.StatusNotImplemented,
		Body:       problem(http.StatusNotImplemented, "not implemented", "updating configuration is not available yet"),
	}, nil
}

// StreamEvents handles GET /events. Not implemented until a later change.
func (s *Server) StreamEvents(_ context.Context, _ mgmtapi.StreamEventsRequestObject) (mgmtapi.StreamEventsResponseObject, error) {
	return mgmtapi.StreamEventsdefaultApplicationProblemPlusJSONResponse{
		StatusCode: http.StatusNotImplemented,
		Body:       problem(http.StatusNotImplemented, "not implemented", "the event stream is not available yet"),
	}, nil
}

// mapDevice converts a runtime DeviceStatus into the generated wire type.
func mapDevice(d *DeviceStatus) mgmtapi.Device {
	out := mgmtapi.Device{
		Name:            d.Config.Name,
		Device:          d.Config.Device,
		Path:            d.Config.Path,
		Mode:            mapMode(d.Config.Mode),
		Format:          mgmtapi.DeviceFormat(d.Config.Format),
		Rate:            d.Config.Rate,
		Channels:        d.Config.Channels,
		State:           mgmtapi.DeviceState(d.State),
		ClientConnected: d.ClientConnected,
		DroppedFrames:   d.DroppedFrames,
	}
	if d.NegotiatedRate > 0 {
		out.NegotiatedRate = ptr(d.NegotiatedRate)
	}
	if d.NegotiatedChannels > 0 {
		out.NegotiatedChannels = ptr(d.NegotiatedChannels)
	}
	if d.Config.Mode == config.ModeOpus {
		out.Opus = &mgmtapi.OpusSettings{Bitrate: ptr(d.Config.Opus.Bitrate)}
	}
	if d.Error != "" {
		out.Error = ptr(d.Error)
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
