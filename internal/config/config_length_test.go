package config

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateLengthCaps pins the length caps on the free-form device strings at
// their boundary: the cap itself is accepted and one character more is
// rejected, counted in characters (a multi-byte name at the cap is accepted).
func TestValidateLengthCaps(t *testing.T) {
	t.Parallel()
	dev := func() Device {
		return Device{
			Name:    "garden",
			Device:  "hw:1,0",
			Rate:    48000,
			Format:  formatS16,
			Streams: []Stream{{Path: "/garden", Mode: ModePCM, Channels: []int{1}}},
		}
	}
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
		{"path at cap", func(d *Device) { d.Streams[0].Path = "/" + strings.Repeat("p", MaxPathLen-1) }, ""},
		{"path over cap", func(d *Device) { d.Streams[0].Path = "/" + strings.Repeat("p", MaxPathLen) }, "devices[0].streams[0].path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := validBase()
			d := dev()
			tc.mutate(&d)
			c.Devices = []Device{d}
			err := c.Validate()
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
