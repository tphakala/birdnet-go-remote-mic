package mgmtserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
)

// tokenStore returns a FileConfigStore seeded with baseConfig plus token.
func tokenStore(t *testing.T, token string) (store *FileConfigStore, path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "config.yaml")
	c := baseConfig()
	c.Auth.Token = token
	return NewFileConfigStore(path, &c), path
}

func patchAuth(t *testing.T, s *Server, token *string) mgmtapi.PatchConfigResponseObject {
	t.Helper()
	resp, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{
		Body: &mgmtapi.ConfigPatch{Auth: &mgmtapi.AuthSettings{Token: token}},
	})
	if err != nil {
		t.Fatalf("PatchConfig: %v", err)
	}
	return resp
}

func TestGetConfigCarriesAuthToken(t *testing.T) {
	store, _ := tokenStore(t, testAuthToken)
	s := New(&fakeProvider{}, WithConfigStore(store))
	resp, err := s.GetConfig(context.Background(), mgmtapi.GetConfigRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := resp.(mgmtapi.GetConfig200JSONResponse)
	if !ok {
		t.Fatalf("response type %T, want 200", resp)
	}
	if got.Auth.Token == nil || *got.Auth.Token != testAuthToken {
		t.Errorf("auth.token = %v, want %q", got.Auth.Token, testAuthToken)
	}
}

func TestGetConfigOpenAccessReportsEmptyToken(t *testing.T) {
	store, _ := tokenStore(t, "")
	s := New(&fakeProvider{}, WithConfigStore(store))
	resp, err := s.GetConfig(context.Background(), mgmtapi.GetConfigRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := resp.(mgmtapi.GetConfig200JSONResponse)
	if !ok {
		t.Fatalf("response type %T, want 200", resp)
	}
	if got.Auth.Token == nil || *got.Auth.Token != "" {
		t.Errorf("auth.token = %v, want an empty string for open access", got.Auth.Token)
	}
}

func TestPatchConfigAuthOnlyPersistsAndReloads(t *testing.T) {
	store, path := tokenStore(t, "")
	var reloaded config.Config
	reloader := func(_ context.Context, cfg config.Config) error {
		reloaded = cfg
		return nil
	}
	s := New(&fakeProvider{}, WithConfigStore(store), WithReloader(reloader))

	resp := patchAuth(t, s, ptr(testAuthToken))
	got, ok := resp.(mgmtapi.PatchConfig200JSONResponse)
	if !ok {
		t.Fatalf("response type %T, want 200", resp)
	}
	if got.RestartRequired {
		t.Error("a token change must hot-apply (restartRequired=false)")
	}
	if got.Config.Auth.Token == nil || *got.Config.Auth.Token != testAuthToken {
		t.Errorf("response auth.token = %v, want %q", got.Config.Auth.Token, testAuthToken)
	}
	if store.Config().Auth.Token != testAuthToken {
		t.Errorf("store token = %q, want %q", store.Config().Auth.Token, testAuthToken)
	}
	if reloaded.Auth.Token != testAuthToken {
		t.Errorf("reloader received token %q, want %q", reloaded.Auth.Token, testAuthToken)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "token: "+testAuthToken) {
		t.Errorf("persisted file lacks the token:\n%s", data)
	}
}

// TestPatchConfigAuthEnforcedEvenWhenReloadFails proves the persisted token is
// enforced on the guard immediately, before (and independently of) the reload
// round trip. A reload that fails still leaves the API requiring the token, so
// a token that is readable via GET /config is never left unenforced.
func TestPatchConfigAuthEnforcedEvenWhenReloadFails(t *testing.T) {
	store, _ := tokenStore(t, "")
	guard := auth.NewGuard("")
	failingReload := func(_ context.Context, _ config.Config) error {
		return errors.New("shutting down")
	}
	s := New(&fakeProvider{}, WithConfigStore(store), WithAuth(guard), WithReloader(failingReload))

	resp := patchAuth(t, s, ptr(testAuthToken))
	got, ok := resp.(mgmtapi.PatchConfig200JSONResponse)
	if !ok {
		t.Fatalf("response type %T, want 200", resp)
	}
	if !got.RestartRequired {
		t.Error("a failed reload must report restartRequired=true")
	}
	if !guard.Enabled() {
		t.Error("the token must be enforced on the guard even though the reload failed")
	}
}

func TestPatchConfigAuthShortTokenYields422(t *testing.T) {
	store, _ := tokenStore(t, "")
	s := New(&fakeProvider{}, WithConfigStore(store))
	resp := patchAuth(t, s, ptr("short"))
	got, ok := resp.(mgmtapi.PatchConfig422ApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("response type %T, want 422", resp)
	}
	if got.Errors == nil || len(*got.Errors) != 1 || (*got.Errors)[0].Field != "auth.token" {
		t.Errorf("errors = %+v, want one auth.token entry", got.Errors)
	}
	if store.Config().Auth.Token != "" {
		t.Error("a rejected token must not be persisted")
	}
}

func TestPatchConfigAuthEmptyDisables(t *testing.T) {
	store, _ := tokenStore(t, testAuthToken)
	s := New(&fakeProvider{}, WithConfigStore(store))
	resp := patchAuth(t, s, ptr(""))
	if _, ok := resp.(mgmtapi.PatchConfig200JSONResponse); !ok {
		t.Fatalf("response type %T, want 200", resp)
	}
	if store.Config().Auth.Token != "" {
		t.Errorf("store token = %q, want cleared", store.Config().Auth.Token)
	}
}

func TestPatchConfigAbsentAuthKeepsToken(t *testing.T) {
	store, _ := tokenStore(t, testAuthToken)
	s := New(&fakeProvider{}, WithConfigStore(store))
	_, err := s.PatchConfig(context.Background(), mgmtapi.PatchConfigRequestObject{
		Body: &mgmtapi.ConfigPatch{Discovery: &mgmtapi.DiscoverySettings{Enabled: ptr(false)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.Config().Auth.Token != testAuthToken {
		t.Errorf("store token = %q, want %q unchanged", store.Config().Auth.Token, testAuthToken)
	}
}
