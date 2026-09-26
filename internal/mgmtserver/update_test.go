package mgmtserver

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
	"github.com/tphakala/birdnet-go-remote-mic/internal/update"
)

// vLatest is the newer release the update tests report.
const vLatest = "v0.3.0"

type fakeUpdates struct {
	st       update.Status
	checkErr error
	applyErr error
	checks   int
	applies  int
}

func (f *fakeUpdates) Status() update.Status { return f.st }

func (f *fakeUpdates) CheckNow(context.Context) (update.Status, error) {
	f.checks++
	return f.st, f.checkErr
}

func (f *fakeUpdates) StartApply() (update.Status, error) {
	f.applies++
	return f.st, f.applyErr
}

func availableStatus() update.Status {
	return update.Status{
		Current:      "v0.2.0",
		Supported:    true,
		CheckEnabled: true,
		Latest:       vLatest,
		NotesURL:     "https://github.com/tphakala/birdnet-go-remote-mic/releases/tag/" + vLatest,
		Available:    true,
		LastCheck:    time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
		Install:      update.Install{Method: update.MethodDeb, Hint: "apt install"},
		Phase:        update.PhaseIdle,
	}
}

func TestGetSystemIncludesUpdate(t *testing.T) {
	t.Parallel()
	s := New(&fakeProvider{}, WithSystemInfo(&fakeSystem{}), WithUpdates(&fakeUpdates{st: availableStatus()}))
	resp, err := s.GetSystem(t.Context(), mgmtapi.GetSystemRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := resp.(mgmtapi.GetSystem200JSONResponse)
	if !ok || got.Update == nil {
		t.Fatalf("GetSystem returned %T with update %v", resp, got.Update)
	}
	u := got.Update
	if u.CurrentVersion != "v0.2.0" || !u.Available || *u.LatestVersion != vLatest || u.InstallMethod != mgmtapi.Deb ||
		u.CanApply || *u.UpgradeHint != "apt install" || u.Phase != mgmtapi.UpdateStatusPhaseIdle || u.LastError != nil || u.PhaseMessage != nil {
		t.Errorf("update %+v", u)
	}
	if !u.LastCheck.Equal(availableStatus().LastCheck) {
		t.Errorf("lastCheck %v", u.LastCheck)
	}

	// Without an update provider the field is omitted.
	s = New(&fakeProvider{}, WithSystemInfo(&fakeSystem{}))
	resp, _ = s.GetSystem(t.Context(), mgmtapi.GetSystemRequestObject{})
	if got := resp.(mgmtapi.GetSystem200JSONResponse); got.Update != nil {
		t.Errorf("update %+v without a provider", got.Update)
	}
}

// TestUpdateToWireOmitsEmpty pins that a status before any check carries no
// empty optional strings.
func TestUpdateToWireOmitsEmpty(t *testing.T) {
	t.Parallel()
	w := updateToWire(&update.Status{Current: "dev", Install: update.Install{Method: update.MethodManual}, Phase: update.PhaseIdle})
	if w.LatestVersion != nil || w.NotesUrl != nil || w.LastCheck != nil || w.LastError != nil || w.UpgradeHint != nil || w.PhaseMessage != nil {
		t.Errorf("empty optional fields present: %+v", w)
	}
}

func TestPostSystemUpdate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		status int
	}{
		{"started", nil, http.StatusAccepted},
		{"no update", update.ErrNoUpdate, http.StatusConflict},
		{"cannot apply", update.ErrCannotApply, http.StatusConflict},
		{"busy", update.ErrBusy, http.StatusConflict},
		{"other", errors.New("disk full"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		f := &fakeUpdates{st: availableStatus(), applyErr: tt.err}
		s := New(&fakeProvider{}, WithUpdates(f))
		resp, err := s.PostSystemUpdate(t.Context(), mgmtapi.PostSystemUpdateRequestObject{})
		if err != nil {
			t.Fatal(err)
		}
		var code int
		switch r := resp.(type) {
		case mgmtapi.PostSystemUpdate202JSONResponse:
			code = http.StatusAccepted
		case mgmtapi.PostSystemUpdate409ApplicationProblemPlusJSONResponse:
			code = *r.Status
			if !strings.Contains(*r.Detail, tt.err.Error()) {
				t.Errorf("%s: detail %q", tt.name, *r.Detail)
			}
		case mgmtapi.PostSystemUpdatedefaultApplicationProblemPlusJSONResponse:
			code = r.StatusCode
		}
		if code != tt.status || f.applies != 1 {
			t.Errorf("%s: status %d (applies %d), want %d", tt.name, code, f.applies, tt.status)
		}
	}
}

func TestPostSystemUpdateCheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		status int
	}{
		{"checked", nil, http.StatusOK},
		{"checks off", update.ErrChecksDisabled, http.StatusConflict},
		{"dev build", update.ErrNotRelease, http.StatusConflict},
		{"other", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		f := &fakeUpdates{st: availableStatus(), checkErr: tt.err}
		s := New(&fakeProvider{}, WithUpdates(f))
		resp, err := s.PostSystemUpdateCheck(t.Context(), mgmtapi.PostSystemUpdateCheckRequestObject{})
		if err != nil {
			t.Fatal(err)
		}
		var code int
		switch r := resp.(type) {
		case mgmtapi.PostSystemUpdateCheck200JSONResponse:
			code = http.StatusOK
			if r.LatestVersion == nil || *r.LatestVersion != vLatest {
				t.Errorf("%s: body %+v", tt.name, r)
			}
		case mgmtapi.PostSystemUpdateCheck409ApplicationProblemPlusJSONResponse:
			code = *r.Status
		case mgmtapi.PostSystemUpdateCheckdefaultApplicationProblemPlusJSONResponse:
			code = r.StatusCode
		}
		if code != tt.status || f.checks != 1 {
			t.Errorf("%s: status %d (checks %d), want %d", tt.name, code, f.checks, tt.status)
		}
	}
}

func TestUpdateRoutesNotImplementedWithoutProvider(t *testing.T) {
	t.Parallel()
	s := New(&fakeProvider{})
	r1, _ := s.PostSystemUpdate(t.Context(), mgmtapi.PostSystemUpdateRequestObject{})
	if p, ok := r1.(mgmtapi.PostSystemUpdatedefaultApplicationProblemPlusJSONResponse); !ok || p.StatusCode != http.StatusNotImplemented {
		t.Errorf("POST /system/update: %T %+v", r1, r1)
	}
	r2, _ := s.PostSystemUpdateCheck(t.Context(), mgmtapi.PostSystemUpdateCheckRequestObject{})
	if p, ok := r2.(mgmtapi.PostSystemUpdateCheckdefaultApplicationProblemPlusJSONResponse); !ok || p.StatusCode != http.StatusNotImplemented {
		t.Errorf("POST /system/update/check: %T %+v", r2, r2)
	}
}

// TestPatchConfigUpdatesCheck pins that updates.check round-trips through a
// PATCH, reaches the reloader and the file, and that an empty updates block
// changes nothing.
func TestPatchConfigUpdatesCheck(t *testing.T) {
	t.Parallel()
	store, path := tempStore(t)
	var reloaded config.Config
	s := New(&fakeProvider{}, WithConfigStore(store), WithReloader(func(_ context.Context, c config.Config) error {
		reloaded = c
		return nil
	}))
	resp, err := s.PatchConfig(t.Context(), mgmtapi.PatchConfigRequestObject{Body: &mgmtapi.ConfigPatch{Updates: &mgmtapi.UpdateSettings{Check: ptr(false)}}})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := resp.(mgmtapi.PatchConfig200JSONResponse)
	if !ok || got.Config.Updates.Check == nil || *got.Config.Updates.Check {
		t.Fatalf("PatchConfig: %T %+v", resp, resp)
	}
	if reloaded.UpdateCheckEnabled() {
		t.Error("the reloader saw updates.check on")
	}
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), "check: false") {
		t.Errorf("config file %q, %v", b, err)
	}

	resp, err = s.PatchConfig(t.Context(), mgmtapi.PatchConfigRequestObject{Body: &mgmtapi.ConfigPatch{Updates: &mgmtapi.UpdateSettings{}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.(mgmtapi.PatchConfig200JSONResponse); *got.Config.Updates.Check {
		t.Error("an empty updates block turned checks back on")
	}
}

func TestGetConfigMaterializesUpdatesCheck(t *testing.T) {
	t.Parallel()
	store, _ := tempStore(t)
	s := New(&fakeProvider{}, WithConfigStore(store))
	resp, _ := s.GetConfig(t.Context(), mgmtapi.GetConfigRequestObject{})
	got := resp.(mgmtapi.GetConfig200JSONResponse)
	if got.Updates.Check == nil || !*got.Updates.Check {
		t.Errorf("updates.check = %v, want true by default", got.Updates.Check)
	}
}
