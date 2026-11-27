package mgmtserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
)

// jsonBodyRoutes are the API routes whose request body is a required JSON
// object, decoded by the generated strict handlers before the Server method
// runs.
var jsonBodyRoutes = []struct{ method, path string }{
	{http.MethodPatch, BasePath + "/config"},
	{http.MethodPost, BasePath + "/devices"},
	{http.MethodPut, BasePath + "/system/certificate"},
	{http.MethodPost, BasePath + "/system/certificate/regenerate"},
}

// bodyTestHandler serves the full API with a writable config store and a
// certificate manager, so every JSON body route reaches its real handler.
func bodyTestHandler(t *testing.T) (http.Handler, *fakeCertManager) {
	t.Helper()
	store, _ := tempStore(t)
	mgr := &fakeCertManager{}
	return New(&fakeProvider{}, WithConfigStore(store), WithCertificateManager(mgr)).Handler(), mgr
}

func serveBody(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) mgmtapi.Problem {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != problemJSONType {
		t.Fatalf("content-type = %q, want %s (body %s)", ct, problemJSONType, rec.Body.String())
	}
	var p mgmtapi.Problem
	if err := json.NewDecoder(rec.Body).Decode(&p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	return p
}

// TestRequiredJSONBodyRefused pins that a missing body on a route whose body is
// required fails in the generated decoder, before any handler runs, rather than
// reaching the handler as a zero-value or nil body. The detail check tells the
// decoder's 400 apart from PutSystemCertificate's own nil-body 400.
func TestRequiredJSONBodyRefused(t *testing.T) {
	t.Parallel()
	for _, route := range jsonBodyRoutes {
		for _, tc := range []struct {
			name, body string
			want       int
			detail     string
		}{
			{"empty", "", http.StatusBadRequest, "can't decode JSON body"},
			{"whitespace only", " \n\t ", http.StatusBadRequest, "can't decode JSON body"},
			{"whitespace past the cap", strings.Repeat(" ", maxRequestBody+1), http.StatusRequestEntityTooLarge, "KiB limit"},
		} {
			t.Run(route.method+" "+route.path+" "+tc.name, func(t *testing.T) {
				t.Parallel()
				h, mgr := bodyTestHandler(t)
				rec := serveBody(t, h, route.method, route.path, tc.body)
				if rec.Code != tc.want {
					t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
				}
				p := decodeProblem(t, rec)
				if p.Detail == nil || !strings.Contains(*p.Detail, tc.detail) {
					t.Errorf("problem detail = %v, want it to contain %q", p.Detail, tc.detail)
				}
				if mgr.gotRegen || mgr.installedCert != nil {
					t.Errorf("certificate manager called (regen %v, install %q), want no call", mgr.gotRegen, mgr.installedCert)
				}
			})
		}
	}
}

// TestJSONBodyReachesHandler pins that a valid body is decoded into the request
// object each handler acts on: every row checks a field the handler can only
// see if decoding kept it.
func TestJSONBodyReachesHandler(t *testing.T) {
	t.Parallel()

	t.Run("PATCH config", func(t *testing.T) {
		t.Parallel()
		h, _ := bodyTestHandler(t)
		rec := serveBody(t, h, http.MethodPatch, BasePath+"/config", `{"discovery":{"enabled":false}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		var got mgmtapi.ConfigUpdateResult
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// The base config leaves discovery unset, which means on, so only a
		// decoded patch turns it off.
		if got.Config.Discovery.Enabled == nil || *got.Config.Discovery.Enabled {
			t.Errorf("discovery.enabled = %v, want false", got.Config.Discovery.Enabled)
		}
	})

	t.Run("POST devices", func(t *testing.T) {
		t.Parallel()
		h, _ := bodyTestHandler(t)
		const id = "usb:ghost"
		rec := serveBody(t, h, http.MethodPost, BasePath+"/devices", `{"device":"`+id+`"}`)
		// A body decoded without its device id is refused with 422 instead.
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
		}
		if p := decodeProblem(t, rec); p.Detail == nil || !strings.Contains(*p.Detail, id) {
			t.Errorf("problem detail = %v, want it to name %q", p.Detail, id)
		}
	})

	t.Run("PUT system certificate", func(t *testing.T) {
		t.Parallel()
		h, mgr := bodyTestHandler(t)
		rec := serveBody(t, h, http.MethodPut, BasePath+"/system/certificate",
			`{"certPem":"`+testCertPEM+`","keyPem":"`+testKeyPEM+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		if string(mgr.installedCert) != testCertPEM || string(mgr.installedKey) != testKeyPEM {
			t.Errorf("installed cert %q key %q, want %q and %q", mgr.installedCert, mgr.installedKey, testCertPEM, testKeyPEM)
		}
	})

	t.Run("POST system certificate regenerate", func(t *testing.T) {
		t.Parallel()
		h, mgr := bodyTestHandler(t)
		const san = "regen.example"
		rec := serveBody(t, h, http.MethodPost, BasePath+"/system/certificate/regenerate", `{"extraSans":["`+san+`"]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		if want := []string{san}; !slices.Equal(mgr.gotExtras, want) {
			t.Errorf("forwarded extras = %v, want %v", mgr.gotExtras, want)
		}
	})
}
