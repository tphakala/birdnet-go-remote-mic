package mgmtserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/tphakala/birdnet-go-remote-mic/internal/auth"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
)

const (
	testAuthToken   = "k7Qm3vX9pL2wR8nT"
	bearerRealm     = `Bearer realm="birdnet-go-remote-mic"`
	pathStatus      = "/api/v1/status"
	pathConfig      = "/api/v1/config"
	pathEvents      = "/api/v1/events"
	indexAsset      = "index.html"
	stylesAsset     = "styles.css"
	problemJSONType = "application/problem+json"
)

// authTestServer serves the full handler (API, SSE stub, static UI) behind
// the given guard, so the tests exercise exactly the routing production uses.
func authTestServer(t *testing.T, g *auth.Guard) *httptest.Server {
	t.Helper()
	memFS := fstest.MapFS{
		indexAsset:  &fstest.MapFile{Data: []byte("<!doctype html><title>ui</title>")},
		stylesAsset: &fstest.MapFile{Data: []byte("body{}")},
	}
	stream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	store, _ := tempStore(t)
	opts := []Option{WithStaticAssets(memFS), WithEventStream(stream), WithConfigStore(store)}
	if g != nil {
		opts = append(opts, WithAuth(g))
	}
	s := New(&fakeProvider{status: ApplianceStatus{Version: testVersion}}, opts...)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// reply is a fully-read HTTP response: the body is consumed and closed inside
// doReq so callers only look at the outcome.
type reply struct {
	status int
	header http.Header
	body   []byte
}

func doReq(t *testing.T, srv *httptest.Server, method, path, bearer string) reply {
	t.Helper()
	var body io.Reader
	if method == http.MethodPatch {
		body = strings.NewReader(`{"discovery":{"enabled":true}}`)
	}
	req, err := http.NewRequest(method, srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: data}
}

var securedRoutes = []struct{ method, path string }{
	{http.MethodGet, pathStatus},
	{http.MethodGet, "/api/v1/devices"},
	{http.MethodGet, "/api/v1/devices/available"},
	{http.MethodPost, "/api/v1/devices"},
	{http.MethodGet, "/api/v1/devices/mic-1"},
	{http.MethodDelete, "/api/v1/devices/mic-1"},
	{http.MethodGet, pathConfig},
	{http.MethodPatch, pathConfig},
	{http.MethodGet, "/api/v1/system"},
	{http.MethodPost, "/api/v1/system/restart"},
	{http.MethodGet, pathEvents},
}

func TestAuthDisabledGuardServesOpen(t *testing.T) {
	srv := authTestServer(t, auth.NewGuard(""))
	for _, r := range securedRoutes {
		if got := doReq(t, srv, r.method, r.path, ""); got.status == http.StatusUnauthorized {
			t.Errorf("%s %s: 401 with no token configured (must be open access)", r.method, r.path)
		}
	}
}

func TestAuthNoGuardServesOpen(t *testing.T) {
	srv := authTestServer(t, nil)
	if got := doReq(t, srv, http.MethodGet, pathStatus, ""); got.status != http.StatusOK {
		t.Errorf("status = %d, want 200 without a guard mounted", got.status)
	}
}

func TestAuthRejectsMissingBearer(t *testing.T) {
	srv := authTestServer(t, auth.NewGuard(testAuthToken))
	for _, r := range securedRoutes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			got := doReq(t, srv, r.method, r.path, "")
			if got.status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", got.status)
			}
			if h := got.header.Get("WWW-Authenticate"); h != bearerRealm {
				t.Errorf("WWW-Authenticate = %q, want %q", h, bearerRealm)
			}
			if ct := got.header.Get("Content-Type"); ct != problemJSONType {
				t.Errorf("Content-Type = %q, want %s", ct, problemJSONType)
			}
			var p mgmtapi.Problem
			if err := json.Unmarshal(got.body, &p); err != nil {
				t.Fatalf("decode problem: %v", err)
			}
			if p.Status == nil || *p.Status != http.StatusUnauthorized {
				t.Errorf("problem status = %v, want 401", p.Status)
			}
		})
	}
}

func TestAuthWrongBearerReportsInvalidToken(t *testing.T) {
	srv := authTestServer(t, auth.NewGuard(testAuthToken))
	got := doReq(t, srv, http.MethodGet, pathStatus, "not-the-token-1")
	if got.status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", got.status)
	}
	want := bearerRealm + `, error="invalid_token"`
	if h := got.header.Get("WWW-Authenticate"); h != want {
		t.Errorf("WWW-Authenticate = %q, want %q", h, want)
	}
}

// TestAuthNonBearerSchemeGetsBareChallenge proves error="invalid_token" is
// scoped to the Bearer scheme (RFC 6750 section 3.1): a Basic header, which did
// not fail bearer validation, gets the bare challenge instead of being
// misdirected with a bearer-specific error code. A wrong Bearer still carries
// the code (covered by TestAuthWrongBearerReportsInvalidToken).
func TestAuthNonBearerSchemeGetsBareChallenge(t *testing.T) {
	srv := authTestServer(t, auth.NewGuard(testAuthToken))
	req, err := http.NewRequest(http.MethodGet, srv.URL+pathStatus, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Basic bWljOnNlY3JldA==")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if h := resp.Header.Get("WWW-Authenticate"); h != bearerRealm {
		t.Errorf("WWW-Authenticate = %q, want the bare challenge %q (no invalid_token for a non-Bearer scheme)", h, bearerRealm)
	}
}

func TestAuthCorrectBearerPasses(t *testing.T) {
	srv := authTestServer(t, auth.NewGuard(testAuthToken))
	for _, path := range []string{pathStatus, pathEvents, pathConfig} {
		if got := doReq(t, srv, http.MethodGet, path, testAuthToken); got.status != http.StatusOK {
			t.Errorf("GET %s with the token: status = %d, want 200", path, got.status)
		}
	}
}

func TestAuthHealthzAndStaticStayOpen(t *testing.T) {
	srv := authTestServer(t, auth.NewGuard(testAuthToken))
	for _, path := range []string{"/api/v1/healthz", "/", "/" + stylesAsset, "/dashboard"} {
		if got := doReq(t, srv, http.MethodGet, path, ""); got.status != http.StatusOK {
			t.Errorf("GET %s without a token: status = %d, want 200 (must stay open)", path, got.status)
		}
	}
}

func TestAuthHotSwapAppliesToRequests(t *testing.T) {
	g := auth.NewGuard(testAuthToken)
	srv := authTestServer(t, g)
	g.Set("rotated-token-0001")
	if got := doReq(t, srv, http.MethodGet, pathStatus, testAuthToken); got.status != http.StatusUnauthorized {
		t.Errorf("old token after rotation: status = %d, want 401", got.status)
	}
	if got := doReq(t, srv, http.MethodGet, pathStatus, "rotated-token-0001"); got.status != http.StatusOK {
		t.Errorf("new token after rotation: status = %d, want 200", got.status)
	}
}

func TestGetStatusMapsAuthRequired(t *testing.T) {
	s := New(&fakeProvider{status: ApplianceStatus{Version: testVersion, AuthRequired: true}})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	got := doReq(t, srv, http.MethodGet, pathStatus, "")
	var st mgmtapi.ApplianceStatus
	if err := json.Unmarshal(got.body, &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !st.AuthRequired {
		t.Error("authRequired = false, want true")
	}
}
