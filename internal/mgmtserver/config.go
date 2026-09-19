package mgmtserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sync"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// configRestartKey is the notification-center condition key for a persisted
// configuration change that could not be hot-applied and needs a restart. It has
// no matching clear: a restart empties the ring, so the condition simply does not
// return after the next boot.
const configRestartKey = "config:restart"

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
// Only discovery, the access token, the notifications block and the device list
// are patchable; an absent field is left unchanged, and a present devices array
// replaces the whole list.
// The merged configuration must validate as a whole. With a reloader mounted the
// change is applied to the running pipeline in place (restartRequired=false);
// without one it is persisted but takes effect only after a restart.
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
	if patch == nil || (patch.Discovery == nil && patch.Auth == nil && patch.Notifications == nil && patch.Devices == nil) {
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
		// A discovery block with no enabled field is a no-op: copying the patch's
		// nil pointer straight in would reset the flag, and a nil discovery flag
		// defaults ON, so discovery:{} would silently re-enable advertisement an
		// operator explicitly turned off. Copy a present value into fresh storage
		// so the persisted config never aliases the request body (parity with the
		// device and auth branches).
		if patch.Discovery != nil && patch.Discovery.Enabled != nil {
			v := *patch.Discovery.Enabled
			cur.Discovery.Enabled = &v
		}
		// An auth block with no token field is a no-op; an empty string clears
		// the token (open access), which Validate accepts.
		if patch.Auth != nil && patch.Auth.Token != nil {
			cur.Auth.Token = *patch.Auth.Token
		}
		// A notifications block merges field by field: only the present fields (and
		// present nested objects) change, so {"host":{"cpuPercent":95}} touches one
		// threshold and leaves the rest as stored. mergeNotifications preserves
		// presence, so ApplyDefaults below fills only fields left absent while an
		// explicitly supplied out-of-range value is kept for Validate to reject.
		if patch.Notifications != nil {
			mergeNotifications(&cur.Notifications, patch.Notifications)
		}
		if patch.Devices != nil {
			devs, derr := patchedDevices(cur.Devices, *patch.Devices)
			if derr != nil {
				return config.Config{}, derr
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

	// Enforce the persisted token immediately, before the reload round trip.
	// Persistence and enforcement must not be separated: GET /config returns
	// the token, so a token that is stored and readable but not yet enforced
	// (or one whose reload below fails) would leave the API answering as if it
	// were still open access. Set is idempotent, so cmd's own reconcile guard
	// applying the same token again is harmless.
	//
	// The auth-changed notification is derived here, not in the reconcile: Set
	// advances the generation only on an actual token change, and this handler's
	// Set runs before the reconcile's idempotent one, so comparing the generation
	// around this Set is the only place that can observe the transition.
	if s.guard != nil {
		// Snapshot reads (enabled, generation) as one consistent atomic pair, so the
		// before/after comparison cannot tear across a concurrent token change. The
		// generation advances only on an actual token change, so an unchanged token
		// emits nothing.
		wasEnabled, genBefore := s.guard.Snapshot()
		s.guard.Set(cur.Auth.Token)
		nowEnabled, genAfter := s.guard.Snapshot()
		if genAfter != genBefore && s.notifier != nil {
			s.notifier.Publish(authChangedNotification(wasEnabled, nowEnabled))
		}
	}

	// With a reloader mounted, apply the persisted change to the running pipeline
	// in place. A reload error (not a per-device open failure, which surfaces via
	// GET /devices) means the change is persisted but not live, so fall back to
	// reporting that a restart is needed to apply it.
	restartRequired := true
	// reloadIndeterminate marks a reload error caused by the request being canceled
	// (the PATCH client disconnected) or timing out. The reload request is enqueued
	// before the wait, so the run loop may have applied the change anyway: the
	// outcome is unknown, and it is not a config defect. Such an error must not
	// raise a "reload failed" event or a "restart required" condition that a later,
	// still-connected client would read as a real fault. restartRequired still
	// reflects it in the (already-abandoned) response.
	reloadIndeterminate := false
	if s.reloader != nil {
		if err := s.reloader(ctx, cur); err != nil {
			log.Printf("mgmtserver: config persisted but hot reload failed: %v (a restart will apply it)", err)
			reloadIndeterminate = errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
			if s.notifier != nil && !reloadIndeterminate {
				s.notifier.Publish(notify.Notification{
					Severity: notify.SeverityError,
					Category: notify.CategoryConfig,
					Kind:     notify.KindEvent,
					Title:    "Config reload failed",
					Message:  "Configuration was saved but could not be applied live: " + err.Error() + "; a restart is needed to apply it",
				})
			}
		} else {
			restartRequired = false
			// The running pipeline now matches the persisted config, so clear any
			// restart-required condition an earlier failed hot reload raised. This is
			// a normal condition end (the change applied), not a vanished subject, so
			// Clear carries a specific message rather than Resolve's generic one.
			// Clear is a no-op when the condition is not active.
			if s.notifier != nil {
				s.notifier.Clear(configRestartKey, notify.Notification{
					Severity: notify.SeverityInfo,
					Title:    "Restart no longer required",
					Message:  "The configuration was applied live, so the pending restart is no longer needed.",
				})
			}
		}
	}
	// A change that could not be hot-applied (no reloader, or a genuine reload
	// failure, but not an indeterminate cancellation) leaves the running pipeline
	// out of sync with the persisted config until a restart. Onset is idempotent,
	// so repeated restart-needing patches raise the condition once.
	if restartRequired && !reloadIndeterminate && s.notifier != nil {
		s.notifier.Onset(notify.Notification{
			Severity: notify.SeverityWarning,
			Category: notify.CategoryConfig,
			Key:      configRestartKey,
			Title:    "Restart required",
			Message:  "A configuration change was saved but needs a restart to take effect",
		})
	}

	return mgmtapi.PatchConfig200JSONResponse{
		Config:          configToWire(&cur),
		RestartRequired: restartRequired,
	}, nil
}

// authChangedNotification builds the config.auth_changed event from the guard's
// enabled state before and after a token change. The generation moved, so the
// token is not unchanged: it was enabled (open to protected), disabled (protected
// to open), or rotated (a new token while it stayed protected).
func authChangedNotification(wasEnabled, nowEnabled bool) notify.Notification {
	var msg string
	switch {
	case !wasEnabled && nowEnabled:
		msg = "Access token enabled; the RTSP stream and the management API now require it"
	case wasEnabled && !nowEnabled:
		msg = "Access token disabled; the RTSP stream and the management API are now open on the network"
	default:
		msg = "Access token rotated; streaming clients must reconnect with the new token"
	}
	return notify.Notification{
		Severity: notify.SeverityInfo,
		Category: notify.CategoryConfig,
		Kind:     notify.KindEvent,
		Title:    "Access control changed",
		Message:  msg,
	}
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
		Auth:          mgmtapi.AuthSettings{Token: ptr(c.Auth.Token)},
		Notifications: notificationsToWire(c),
		Devices:       devs,
	}
	if c.Management.Listen != "" {
		out.Management.Listen = ptr(c.Management.Listen)
	}
	if c.Management.CertDir != "" {
		out.Management.CertDir = ptr(c.Management.CertDir)
	}
	return out
}

// notificationsToWire maps the notifications block to the generated wire type,
// materializing every field (the enabled flag to its effective boolean, each
// threshold to its stored value) so the web UI sees concrete values rather than
// nulls it must reinterpret. configToWire is only called on a config from the
// store, which is always defaulted, so every threshold pointer is non-nil; each
// is copied into fresh wire storage so the response never aliases the config.
func notificationsToWire(c *config.Config) mgmtapi.NotificationSettings {
	n := &c.Notifications
	return mgmtapi.NotificationSettings{
		Enabled: ptr(c.NotificationsEnabled()),
		Audio: &mgmtapi.AudioAlertSettings{
			QuietDbfs:         ptr(*n.Audio.QuietDbfs),
			QuietSeconds:      ptr(*n.Audio.QuietSeconds),
			ZeroSeconds:       ptr(*n.Audio.ZeroSeconds),
			ClipPercent:       ptr(*n.Audio.ClipPercent),
			ClipWindowSeconds: ptr(*n.Audio.ClipWindowSeconds),
		},
		Host: &mgmtapi.HostAlertSettings{
			CpuPercent:       ptr(*n.Host.CPUPercent),
			CpuClearPercent:  ptr(*n.Host.CPUClearPercent),
			TempCelsius:      ptr(*n.Host.TempCelsius),
			TempClearCelsius: ptr(*n.Host.TempClearCelsius),
			DiskPercent:      ptr(*n.Host.DiskPercent),
			DiskClearPercent: ptr(*n.Host.DiskClearPercent),
			MemFreePercent:   ptr(*n.Host.MemFreePercent),
			MemFreeMiB:       ptr(*n.Host.MemFreeMiB),
		},
	}
}

// mergeNotifications applies the present fields of a notifications patch onto the
// current config block, leaving absent fields (and absent nested objects)
// unchanged. Every wire field is optional (a pointer), so a partial patch merges
// only what it carries. A present value is copied into fresh storage (so the
// config never aliases the request body) and its presence is preserved: an
// explicitly supplied out-of-range value such as 0 is kept rather than treated
// as unset, so ApplyDefaults leaves it and Validate rejects it with a 422.
func mergeNotifications(dst *config.Notifications, p *mgmtapi.NotificationSettings) {
	if p.Enabled != nil {
		v := *p.Enabled
		dst.Enabled = &v
	}
	if a := p.Audio; a != nil {
		if a.QuietDbfs != nil {
			dst.Audio.QuietDbfs = ptr(*a.QuietDbfs)
		}
		if a.QuietSeconds != nil {
			dst.Audio.QuietSeconds = ptr(*a.QuietSeconds)
		}
		if a.ZeroSeconds != nil {
			dst.Audio.ZeroSeconds = ptr(*a.ZeroSeconds)
		}
		if a.ClipPercent != nil {
			dst.Audio.ClipPercent = ptr(*a.ClipPercent)
		}
		if a.ClipWindowSeconds != nil {
			dst.Audio.ClipWindowSeconds = ptr(*a.ClipWindowSeconds)
		}
	}
	if h := p.Host; h != nil {
		if h.CpuPercent != nil {
			dst.Host.CPUPercent = ptr(*h.CpuPercent)
		}
		if h.CpuClearPercent != nil {
			dst.Host.CPUClearPercent = ptr(*h.CpuClearPercent)
		}
		if h.TempCelsius != nil {
			dst.Host.TempCelsius = ptr(*h.TempCelsius)
		}
		if h.TempClearCelsius != nil {
			dst.Host.TempClearCelsius = ptr(*h.TempClearCelsius)
		}
		if h.DiskPercent != nil {
			dst.Host.DiskPercent = ptr(*h.DiskPercent)
		}
		if h.DiskClearPercent != nil {
			dst.Host.DiskClearPercent = ptr(*h.DiskClearPercent)
		}
		if h.MemFreePercent != nil {
			dst.Host.MemFreePercent = ptr(*h.MemFreePercent)
		}
		if h.MemFreeMiB != nil {
			dst.Host.MemFreeMiB = ptr(*h.MemFreeMiB)
		}
	}
}

// deviceConfigToWire maps one configured device to the generated wire type. The
// flat path/mode/channels/opus fields mirror the device's first stream so a
// client that predates fan-out still sees a usable single-stream device; every
// stream is also carried in the streams array.
func deviceConfigToWire(d *config.Device) mgmtapi.DeviceConfig {
	out := mgmtapi.DeviceConfig{
		Name:   d.Name,
		Device: d.Device,
		Format: mgmtapi.DeviceConfigFormat(d.Format),
		Rate:   d.Rate,
	}
	if len(d.Streams) > 0 {
		s0 := &d.Streams[0]
		out.Path = s0.Path
		out.Mode = mapMode(s0.Mode)
		out.Channels = s0.Channels
		if s0.Mode == config.ModeOpus {
			out.Opus = &mgmtapi.OpusSettings{Bitrate: ptr(s0.Opus.Bitrate)}
		}
	}
	streams := make([]mgmtapi.StreamConfig, 0, len(d.Streams))
	for i := range d.Streams {
		streams = append(streams, streamConfigToWire(&d.Streams[i]))
	}
	out.Streams = &streams
	// Materialize the default-on Enabled and QuietAlert flags to concrete booleans
	// so the web UI sees definite values rather than nulls it must reinterpret.
	out.Enabled = ptr(d.IsEnabled())
	out.QuietAlert = ptr(d.QuietAlertEnabled())
	return out
}

// streamConfigToWire maps one configured stream to the generated wire type.
func streamConfigToWire(s *config.Stream) mgmtapi.StreamConfig {
	out := mgmtapi.StreamConfig{
		Path:     s.Path,
		Mode:     mapMode(s.Mode),
		Channels: s.Channels,
	}
	if s.Mode == config.ModeOpus {
		out.Opus = &mgmtapi.OpusSettings{Bitrate: ptr(s.Opus.Bitrate)}
	}
	return out
}

// wireDeviceToConfig maps one device from a config patch back to the appliance
// type. A body carrying streams is authoritative; a body omitting streams
// defines a single stream from the flat path/mode/channels/opus fields (today's
// semantics for a client that predates fan-out). Missing fields become zero
// values; config.Validate rejects them with a per-field reason, which surfaces
// as a 422.
func wireDeviceToConfig(d *mgmtapi.DeviceConfig) config.Device {
	out := config.Device{
		Name:   d.Name,
		Device: d.Device,
		Format: string(d.Format),
		Rate:   d.Rate,
	}
	if d.Streams != nil {
		out.Streams = make([]config.Stream, 0, len(*d.Streams))
		for i := range *d.Streams {
			out.Streams = append(out.Streams, wireStreamToConfig(&(*d.Streams)[i]))
		}
	} else {
		out.Streams = []config.Stream{wireFlatToStream(d)}
	}
	// An absent enabled flag leaves the device enabled (the default); a present
	// one is copied into fresh storage so the persisted config does not alias the
	// request body.
	if d.Enabled != nil {
		v := *d.Enabled
		out.Enabled = &v
	}
	// QuietAlert follows the same rule as Enabled: absent defaults on, a present
	// value is copied into fresh storage rather than aliasing the request body.
	if d.QuietAlert != nil {
		v := *d.QuietAlert
		out.QuietAlert = &v
	}
	return out
}

// patchedDevices maps a wire device list to the appliance device list that
// replaces cur. A wire entry that omits streams defines a single stream from its
// flat fields; that is rejected when it would silently collapse an existing
// multi-stream device, so a client that predates fan-out cannot drop streams it
// cannot see. The existing device is looked up by BOTH its name and its device
// id, and either match being multi-stream rejects the entry: a rename keeps the
// id, a rebind keeps the name, and a name rotation (one device's name paired with
// another's id) is caught because the id still resolves to the multi-stream
// device. Checking only one identifier leaves the mirror bypass open. When both
// matches are multi-stream the name match is reported, since the name is the
// device's identity everywhere else (the reload plan, the runtime records,
// notifications). A wire entry that carries streams is authoritative and may
// legitimately reduce the count.
func patchedDevices(cur []config.Device, wire []mgmtapi.DeviceConfig) ([]config.Device, error) {
	devs := make([]config.Device, 0, len(wire))
	for i := range wire {
		wd := &wire[i]
		if wd.Streams == nil {
			var collapse *config.Device
			if byName, ok := deviceByName(cur, wd.Name); ok && len(byName.Streams) > 1 {
				collapse = byName
			} else if byID, ok := deviceByID(cur, wd.Device); ok && len(byID.Streams) > 1 {
				collapse = byID
			}
			if collapse != nil {
				return nil, &config.ValidationError{
					Field:  fmt.Sprintf("devices[%d].streams", i),
					Reason: fmt.Sprintf("device %q has %d streams; include streams to update it", collapse.Name, len(collapse.Streams)),
				}
			}
		}
		devs = append(devs, wireDeviceToConfig(wd))
	}
	return devs, nil
}

// deviceByID returns a pointer to the device with the given device id in devs,
// if present. The collapse guard also matches by id (unchanged by a rename), so
// a flat patch cannot slip a collapse past it by changing one identifier.
func deviceByID(devs []config.Device, id string) (*config.Device, bool) {
	for i := range devs {
		if devs[i].Device == id {
			return &devs[i], true
		}
	}
	return nil, false
}

// deviceByName returns a pointer to the device named name in devs, if present.
// The collapse guard also matches by name, so rebinding a multi-stream device to
// other hardware in a flat patch, which keeps the name, is still caught.
func deviceByName(devs []config.Device, name string) (*config.Device, bool) {
	for i := range devs {
		if devs[i].Name == name {
			return &devs[i], true
		}
	}
	return nil, false
}

// wireStreamToConfig maps one stream from a config patch back to the appliance
// type, cloning the channel selection into fresh storage so the persisted config
// does not alias the request body.
func wireStreamToConfig(s *mgmtapi.StreamConfig) config.Stream {
	out := config.Stream{
		Path:     s.Path,
		Mode:     config.Mode(s.Mode),
		Channels: slices.Clone(s.Channels),
	}
	if s.Opus != nil && s.Opus.Bitrate != nil {
		out.Opus.Bitrate = *s.Opus.Bitrate
	}
	return out
}

// wireFlatToStream builds the single stream implied by a device patch that omits
// the streams array, from its flat path/mode/channels/opus fields.
func wireFlatToStream(d *mgmtapi.DeviceConfig) config.Stream {
	out := config.Stream{
		Path:     d.Path,
		Mode:     config.Mode(d.Mode),
		Channels: slices.Clone(d.Channels),
	}
	if d.Opus != nil && d.Opus.Bitrate != nil {
		out.Opus.Bitrate = *d.Opus.Bitrate
	}
	return out
}
