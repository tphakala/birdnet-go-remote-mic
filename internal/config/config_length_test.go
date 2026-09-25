package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// lengthTestDevice is a valid single-stream device the length tests stretch.
func lengthTestDevice() Device {
	return Device{
		Name:    "garden",
		Device:  "hw:1,0",
		Rate:    48000,
		Format:  formatS16,
		Streams: []Stream{{Path: "/garden", Mode: ModePCM, Channels: []int{1}}},
	}
}

// TestValidateLengthCaps pins the length caps at their boundary: the cap itself
// is accepted and one character more is rejected, counted in characters (a
// multi-byte name at the cap is accepted).
func TestValidateLengthCaps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		mutate    func(d *Device)
		wantField string // "" means valid
	}{
		{"name at cap", func(d *Device) { d.Name = strings.Repeat("n", MaxNameLen) }, ""},
		{"multi-byte name at cap", func(d *Device) { d.Name = strings.Repeat("ä", MaxNameLen) }, ""},
		{"name over cap", func(d *Device) { d.Name = strings.Repeat("n", MaxNameLen+1) }, "devices[0].name"},
		{"device at cap", func(d *Device) { d.Device = strings.Repeat("d", MaxDeviceIDLen) }, ""},
		{"device over cap", func(d *Device) { d.Device = strings.Repeat("d", MaxDeviceIDLen+1) }, "devices[0].device"},
		{"multi-byte device at cap", func(d *Device) { d.Device = strings.Repeat("ä", MaxDeviceIDLen) }, ""},
		{"path at cap", func(d *Device) { d.Streams[0].Path = "/" + strings.Repeat("p", MaxPathLen-1) }, ""},
		{"multi-byte path at cap", func(d *Device) { d.Streams[0].Path = "/" + strings.Repeat("ä", MaxPathLen-1) }, ""},
		{"path over cap", func(d *Device) { d.Streams[0].Path = "/" + strings.Repeat("p", MaxPathLen) }, "devices[0].streams[0].path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := validBase()
			d := lengthTestDevice()
			tc.mutate(&d)
			c.Devices = []Device{d}
			err := c.ValidateLengths()
			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("got %v, want nil", err)
				}
				return
			}
			verr, ok := errors.AsType[*ValidationError](err)
			if !ok || verr.Field != tc.wantField {
				t.Fatalf("got %v, want a %s ValidationError", err, tc.wantField)
			}
		})
	}
}

// TestLoadAcceptsOverCapStrings pins that the caps never apply at Load: a config
// that ran before the caps existed (or holds a long library-generated device id)
// must keep loading, or the appliance and its UI stay down after an upgrade.
func TestLoadAcceptsOverCapStrings(t *testing.T) {
	t.Parallel()
	c := validBase()
	d := lengthTestDevice()
	d.Name = strings.Repeat("n", MaxNameLen+1)
	d.Device = strings.Repeat("d", MaxDeviceIDLen+1)
	d.Streams[0].Path = "/" + strings.Repeat("p", MaxPathLen)
	c.Devices = []Device{d}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := Save(path, &c); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load of an over-cap config = %v, want it to load", err)
	}
	if loaded.Devices[0].Name != d.Name {
		t.Errorf("loaded name %q, want the over-cap name kept", loaded.Devices[0].Name)
	}
}
