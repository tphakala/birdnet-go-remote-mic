package mgmtserver

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
)

// TestPatchConfigRejectsOverCapName pins that the API applies the length caps
// that Load does not: a PATCH carrying an over-long device name is refused with
// a 422 naming the field, and nothing is persisted.
func TestPatchConfigRejectsOverCapName(t *testing.T) {
	t.Parallel()
	store, _ := twoStreamConfig(t)
	s := New(&fakeProvider{}, WithConfigStore(store))
	streams := []mgmtapi.StreamConfig{
		{Path: pathNorth, Mode: mgmtapi.Pcm, Channels: []int{1}},
		{Path: pathSouth, Mode: mgmtapi.Pcm, Channels: []int{2}},
	}
	devs := []mgmtapi.DeviceConfig{{
		Name: strings.Repeat("n", config.MaxNameLen+1), Device: devIface, Rate: 48000,
		Format: mgmtapi.DeviceConfigFormatS16, Path: pathNorth, Mode: mgmtapi.Pcm,
		Channels: []int{1}, Streams: &streams,
	}}
	resp, err := s.PatchConfig(t.Context(), mgmtapi.PatchConfigRequestObject{Body: &mgmtapi.ConfigPatch{Devices: &devs}})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	vp, ok := resp.(mgmtapi.PatchConfig422ApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("PatchConfig returned %T, want a 422 validation problem", resp)
	}
	if vp.Errors == nil || len(*vp.Errors) != 1 || (*vp.Errors)[0].Field != "devices[0].name" {
		t.Errorf("got errors %+v, want one on devices[0].name", vp.Errors)
	}
	if got := store.Config().Devices[0].Name; got != nameIface {
		t.Errorf("persisted name %q, want the old name kept", got)
	}
}

// TestPatchConfigAllowsUnrelatedWriteWithStoredOverCapName pins that an over-cap
// name already stored (loadable, since Load does not cap) does not block a write
// that adds no device string, such as a token rotation through the API.
func TestPatchConfigAllowsUnrelatedWriteWithStoredOverCapName(t *testing.T) {
	t.Parallel()
	c := config.Config{
		Listen: rtspAddr,
		Devices: []config.Device{{
			Name: strings.Repeat("n", config.MaxNameLen+1), Device: devIface, Rate: 48000, Format: fmtS16,
			Streams: []config.Stream{{Path: pathNorth, Mode: config.ModePCM, Channels: []int{1}}},
		}},
	}
	c.ApplyDefaults()
	store := NewFileConfigStore(filepath.Join(t.TempDir(), "config.yaml"), &c)
	s := New(&fakeProvider{}, WithConfigStore(store))
	token := "rotated-token-1234"
	resp, err := s.PatchConfig(t.Context(), mgmtapi.PatchConfigRequestObject{Body: &mgmtapi.ConfigPatch{Auth: &mgmtapi.AuthSettings{Token: &token}}})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	if _, ok := resp.(mgmtapi.PatchConfig200JSONResponse); !ok {
		t.Fatalf("PatchConfig returned %T, want 200 for a token-only patch", resp)
	}
	if got := store.Config().Auth.Token; got != token {
		t.Errorf("persisted token %q, want %q", got, token)
	}
}

// TestDeriveNameStaysWithinCap pins that a derived name never exceeds the cap
// provisioning enforces: a friendly name with no ASCII letters or digits falls
// back to the device id, which can be far longer (an escaped USB serial).
func TestDeriveNameStaysWithinCap(t *testing.T) {
	t.Parallel()
	longID := "usb:16d0:06f3:s=" + strings.Repeat("%E2%80%94", 60) + ":if=0,0"
	base := deriveName("録音機", longID, map[string]bool{})
	if len(base) > config.MaxNameLen {
		t.Fatalf("derived name has %d characters, want at most %d", len(base), config.MaxNameLen)
	}
	if strings.HasSuffix(base, "-") {
		t.Errorf("derived name %q ends in a separator after truncation", base)
	}
	// An id whose slug has a separator exactly at the cut: the truncated name
	// must not end in it.
	cut := config.MaxNameLen - 8
	sepAtCut := strings.Repeat("a", cut-1) + ":" + strings.Repeat("b", 50)
	if got := deriveName("", sepAtCut, map[string]bool{}); got != strings.Repeat("a", cut-1) {
		t.Errorf("deriveName with a separator at the cut = %q, want the %d letters before it", got, cut-1)
	}
	next := deriveName("録音機", longID, map[string]bool{base: true})
	if len(next) > config.MaxNameLen || next == base {
		t.Errorf("uniqued name %q, want a distinct name within the cap", next)
	}
}
