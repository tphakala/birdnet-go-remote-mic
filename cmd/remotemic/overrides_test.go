//go:build linux

package main

import (
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
)

// TestBuildOverrides pins buildOverrides: an entry appears only for a flag that
// was given AND changed the value, and the paired boolean toggles render through
// strconv.FormatBool. It builds running/store the way run() does (splitServeConfig)
// so the test exercises the real pairing.
func TestBuildOverrides(t *testing.T) {
	tests := []struct {
		name string
		ov   serveOverrides
		want []mgmtserver.ConfigOverride
	}{
		{
			name: "listen override differs",
			ov:   serveOverrides{listen: listenAddr9, set: map[string]bool{keyListen: true}},
			want: []mgmtserver.ConfigOverride{{Field: "listen", Effective: listenAddr9, Persisted: testRTSP8554}},
		},
		{
			name: "flag set but value unchanged yields nothing",
			ov:   serveOverrides{listen: testRTSP8554, set: map[string]bool{keyListen: true}},
			want: nil,
		},
		{
			name: "discovery override formats booleans",
			ov:   serveOverrides{discovery: false, set: map[string]bool{keyDiscovery: true}},
			want: []mgmtserver.ConfigOverride{{Field: "discovery.enabled", Effective: "false", Persisted: "true"}},
		},
		{
			name: "flag not set yields nothing even when values differ",
			ov:   serveOverrides{listen: listenAddr9, set: map[string]bool{}},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			loaded := config.Default()
			loaded.Listen = testRTSP8554
			running, store := splitServeConfig(&loaded, tc.ov)
			got := buildOverrides(&running, &store, tc.ov)
			if len(got) != len(tc.want) {
				t.Fatalf("buildOverrides = %+v, want %+v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("override[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
