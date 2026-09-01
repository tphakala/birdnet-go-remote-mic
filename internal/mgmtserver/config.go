package mgmtserver

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"sync"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
)

// ConfigStore reads and persists the appliance configuration for the config
// endpoints. When no store is mounted, GET and PATCH /config return 501. Its
// methods are called from HTTP handler goroutines and must be safe for
// concurrent use.
type ConfigStore interface {
	// Config returns the running configuration (defaults applied). The returned
	// value is the caller's own copy; mutating it does not affect the store.
	Config() config.Config
	// Update atomically applies mutate to a copy of the current configuration
	// and persists the result, so concurrent updates cannot race or interleave
	// a read-modify-write. If mutate returns an error nothing is persisted and
	// that error is returned unchanged (so a *config.ValidationError survives).
	Update(mutate func(config.Config) (config.Config, error)) error
}

// WithConfigStore mounts cs as the backing store for GET and PATCH /config.
// Without it those endpoints return 501.
func WithConfigStore(cs ConfigStore) Option {
	return func(s *Server) { s.configStore = cs }
}

// FileConfigStore is a ConfigStore backed by a YAML config file. It holds the
// running configuration in memory and persists every accepted update to disk.
type FileConfigStore struct {
	mu   sync.Mutex
	path string
	cfg  config.Config
}

// NewFileConfigStore returns a store seeded with cfg (already loaded and
// defaulted) that persists updates to path.
func NewFileConfigStore(path string, cfg *config.Config) *FileConfigStore {
	return &FileConfigStore{path: path, cfg: cfg.Clone()}
}

// Config returns a deep copy of the current configuration.
func (s *FileConfigStore) Config() config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Clone()
}

// Update runs mutate against a clone of the current config under the store lock,
// persists the result if mutate succeeds, then swaps in the new config. Passing
// a clone means a mutate that fails partway (for example after replacing the
// device list but before validation) cannot corrupt the retained config.
func (s *FileConfigStore) Update(mutate func(config.Config) (config.Config, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := mutate(s.cfg.Clone())
	if err != nil {
		return err
	}
	if err := config.Save(s.path, &next); err != nil {
		return err
	}
	s.cfg = next.Clone()
	return nil
}

// GetConfig handles GET /config. Without a mounted store it reports 501.
func (s *Server) GetConfig(_ context.Context, _ mgmtapi.GetConfigRequestObject) (mgmtapi.GetConfigResponseObject, error) {
	if s.configStore == nil {
		return mgmtapi.GetConfigdefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusNotImplemented,
			Body:       problem(http.StatusNotImplemented, "not implemented", "reading configuration is not available"),
		}, nil
	}
	cur := s.configStore.Config()
	return mgmtapi.GetConfig200JSONResponse(configToWire(&cur)), nil
}

// Reloader applies a persisted configuration to the running appliance without a
// restart. It returns nil once the change has been reconciled into the live
// capture pipeline (individual devices that then fail to open surface through
// GET /devices, exactly as at startup, and are not reload errors). A non-nil
// error means the change could not be hot-applied at all (for example the
// appliance is shutting down); the configuration is still persisted, so a
// restart will pick it up.
type Reloader func(ctx context.Context, cfg config.Config) error

// WithReloader mounts fn so a persisted PATCH /config is applied to the running
// pipeline in place. With a reloader mounted, a successful patch reports
// restartRequired=false. Without one, the change is persisted but reported as
// restart-required.
func WithReloader(fn Reloader) Option {
	return func(s *Server) { s.reloader = fn }
}

// PatchConfig handles PATCH /config. Without a mounted store it reports 501.
// Only discovery, the access token and the device list are patchable; an
// absent field is left unchanged, and a present devices array replaces the
// whole list. The merged
// configuration must validate as a whole. With a reloader mounted the change is
// applied to the running pipeline in place (restartRequired=false); without one
// it is persisted but takes effect only after a restart.
func (s *Server) PatchConfig(ctx context.Context, request mgmtapi.PatchConfigRequestObject) (mgmtapi.PatchConfigResponseObject, error) {
	if s.configStore == nil {
		return mgmtapi.PatchConfigdefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusNotImplemented,
			Body:       problem(http.StatusNotImplemented, "not implemented", "updating configuration is not available"),
		}, nil
	}
	patch := request.Body
	// An empty patch changes nothing: report the current config without
	// rewriting the file and without claiming a restart is pending.
	if patch == nil || (patch.Discovery == nil && patch.Auth == nil && patch.Devices == nil) {
		cur := s.configStore.Config()
		return mgmtapi.PatchConfig200JSONResponse{
			Config:          configToWire(&cur),
			RestartRequired: false,
		}, nil
	}

	// Serialize the persist-then-reload sequence: without this, two concurrent
	// patches could persist in one order but reconcile in the other, leaving the
	// file and the running pipeline disagreeing.
	s.patchMu.Lock()
	defer s.patchMu.Unlock()

	err := s.configStore.Update(func(cur config.Config) (config.Config, error) {
		if patch.Discovery != nil {
			cur.Discovery.Enabled = patch.Discovery.Enabled
		}
		// An auth block with no token field is a no-op; an empty string clears
		// the token (open access), which Validate accepts.
		if patch.Auth != nil && patch.Auth.Token != nil {
			cur.Auth.Token = *patch.Auth.Token
		}
		if patch.Devices != nil {
			devs := make([]config.Device, 0, len(*patch.Devices))
			for i := range *patch.Devices {
				devs = append(devs, wireDeviceToConfig(&(*patch.Devices)[i]))
			}
			cur.Devices = devs
		}
		cur.ApplyDefaults()
		if verr := cur.Validate(); verr != nil {
			return config.Config{}, verr
		}
		return cur, nil
	})
	if err != nil {
		var verr *config.ValidationError
		if errors.As(err, &verr) {
			return mgmtapi.PatchConfig422ApplicationProblemPlusJSONResponse(validationProblem(verr)), nil
		}
		return mgmtapi.PatchConfigdefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusInternalServerError,
			Body:       problem(http.StatusInternalServerError, "persist failed", err.Error()),
		}, nil
	}

	cur := s.configStore.Config()

	// With a reloader mounted, apply the persisted change to the running pipeline
	// in place. A reload error (not a per-device open failure, which surfaces via
	// GET /devices) means the change is persisted but not live, so fall back to
	// reporting that a restart is needed to apply it.
	restartRequired := true
	if s.reloader != nil {
		if err := s.reloader(ctx, cur); err != nil {
			log.Printf("mgmtserver: config persisted but hot reload failed: %v (a restart will apply it)", err)
		} else {
			restartRequired = false
		}
	}

	return mgmtapi.PatchConfig200JSONResponse{
		Config:          configToWire(&cur),
		RestartRequired: restartRequired,
	}, nil
}

// validationProblem renders a *config.ValidationError as an RFC 9457
// ValidationProblem carrying the single offending field.
func validationProblem(verr *config.ValidationError) mgmtapi.ValidationProblem {
	return mgmtapi.ValidationProblem{
		Status: ptr(http.StatusUnprocessableEntity),
		Title:  ptr("invalid configuration"),
		Detail: ptr(verr.Error()),
		Errors: &[]struct {
			Field  string `json:"field"`
			Reason string `json:"reason"`
		}{
			{Field: verr.Field, Reason: verr.Reason},
		},
	}
}

// configToWire maps the appliance configuration to the generated wire type. The
// discovery and management "enabled" flags are materialized to their effective
// boolean (both default on when absent) so the web UI sees a concrete value
// rather than a null it would have to reinterpret.
func configToWire(c *config.Config) mgmtapi.Config {
	devs := make([]mgmtapi.DeviceConfig, 0, len(c.Devices))
	for i := range c.Devices {
		devs = append(devs, deviceConfigToWire(&c.Devices[i]))
	}
	out := mgmtapi.Config{
		Listen:    c.Listen,
		Discovery: mgmtapi.DiscoverySettings{Enabled: ptr(c.DiscoveryEnabled())},
		Management: mgmtapi.ManagementSettings{
			Enabled: ptr(c.ManagementEnabled()),
		},
		// The token is materialized even when empty so the UI sees a definite
		// "open access" rather than a null it must reinterpret. Returning it to
		// an authenticated caller (who already holds it) is not an escalation, and
		// the Access Control card needs it to show what to paste into BirdNET-Go.
		Auth:    mgmtapi.AuthSettings{Token: ptr(c.Auth.Token)},
		Devices: devs,
	}
	if c.Management.Listen != "" {
		out.Management.Listen = ptr(c.Management.Listen)
	}
	if c.Management.CertDir != "" {
		out.Management.CertDir = ptr(c.Management.CertDir)
	}
	return out
}

// deviceConfigToWire maps one configured device to the generated wire type.
func deviceConfigToWire(d *config.Device) mgmtapi.DeviceConfig {
	out := mgmtapi.DeviceConfig{
		Name:     d.Name,
		Device:   d.Device,
		Path:     d.Path,
		Mode:     mapMode(d.Mode),
		Format:   mgmtapi.DeviceConfigFormat(d.Format),
		Rate:     d.Rate,
		Channels: d.Channels,
	}
	if d.Mode == config.ModeOpus {
		out.Opus = &mgmtapi.OpusSettings{Bitrate: ptr(d.Opus.Bitrate)}
	}
	// Materialize the default-on Enabled flag to a concrete boolean so the web UI
	// sees a definite value rather than a null it must reinterpret.
	out.Enabled = ptr(d.IsEnabled())
	return out
}

// wireDeviceToConfig maps one device from a config patch back to the appliance
// type. Missing fields become zero values; config.Validate rejects them with a
// per-field reason, which surfaces as a 422.
func wireDeviceToConfig(d *mgmtapi.DeviceConfig) config.Device {
	out := config.Device{
		Name:   d.Name,
		Device: d.Device,
		Path:   d.Path,
		Mode:   config.Mode(d.Mode),
		Format: string(d.Format),
		Rate:   d.Rate,
		// Clone the selection into fresh storage so the persisted config does not
		// alias the request body (parity with the Enabled handling below).
		Channels: slices.Clone(d.Channels),
	}
	if d.Opus != nil && d.Opus.Bitrate != nil {
		out.Opus.Bitrate = *d.Opus.Bitrate
	}
	// An absent enabled flag leaves the device enabled (the default); a present
	// one is copied into fresh storage so the persisted config does not alias the
	// request body.
	if d.Enabled != nil {
		v := *d.Enabled
		out.Enabled = &v
	}
	return out
}
