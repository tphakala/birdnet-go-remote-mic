//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
	"github.com/tphakala/birdnet-go-remote-mic/internal/runlock"
)

// patchLiveToken sends a token change to the running appliance described by st
// through PATCH /config, authenticating with bearer (the token currently in
// force; empty for open access). It reports whether the appliance says a
// restart is still needed to finish applying the change.
func patchLiveToken(ctx context.Context, st runlock.State, bearer, token string) (restartRequired bool, err error) {
	hc, err := pinnedClient(st.CertPath)
	if err != nil {
		return false, err
	}
	server := "https://" + dialAddr(st.MgmtAddr) + mgmtserver.BasePath
	client, err := mgmtapi.NewClientWithResponses(server,
		mgmtapi.WithHTTPClient(hc),
		mgmtapi.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			if bearer != "" {
				req.Header.Set("Authorization", "Bearer "+bearer)
			}
			return nil
		}))
	if err != nil {
		return false, err
	}
	resp, err := client.PatchConfigWithResponse(ctx, mgmtapi.ConfigPatch{
		Auth: &mgmtapi.AuthSettings{Token: &token},
	})
	if err != nil {
		return false, err
	}
	if resp.JSON200 != nil {
		return resp.JSON200.RestartRequired, nil
	}
	return false, patchError(resp)
}

// patchError turns a non-200 PATCH /config answer into an operator-facing error.
func patchError(resp *mgmtapi.PatchConfigResponse) error {
	status := resp.StatusCode()
	if status == http.StatusUnauthorized {
		return errors.New("it rejected the token stored in the config file, so the file and the running appliance disagree; restart the appliance, or change the token in the web UI")
	}
	var detail string
	switch {
	case resp.ApplicationproblemJSON422 != nil && resp.ApplicationproblemJSON422.Errors != nil:
		for _, e := range *resp.ApplicationproblemJSON422.Errors {
			detail += fmt.Sprintf("; %s: %s", e.Field, e.Reason)
		}
	case resp.ApplicationproblemJSON400 != nil && resp.ApplicationproblemJSON400.Detail != nil:
		detail = "; " + *resp.ApplicationproblemJSON400.Detail
	case resp.ApplicationproblemJSONDefault != nil && resp.ApplicationproblemJSONDefault.Detail != nil:
		detail = "; " + *resp.ApplicationproblemJSONDefault.Detail
	}
	return fmt.Errorf("HTTP %d%s", status, detail)
}

// dialAddr turns the bound listener address into one a local client can dial:
// a wildcard bind (":8443" binds as "[::]:8443" or "0.0.0.0:8443") is reached
// over IPv4 loopback, which a Go wildcard listener accepts on either family. A
// specific bind address is dialed as-is.
func dialAddr(bound string) string {
	host, port, err := net.SplitHostPort(bound)
	if err != nil {
		return bound
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// pinnedClient returns an HTTP client that trusts exactly the certificate the
// running appliance published: the leaf it presents must equal the one in
// certPath. Pinning, rather than chain and name verification, works for the
// self-signed default and for an installed certificate whose names need not
// cover the loopback address this client dials.
func pinnedClient(certPath string) (*http.Client, error) {
	pemBytes, err := os.ReadFile(certPath) //nolint:gosec // path published by the running appliance
	if err != nil {
		return nil, withPermHint(fmt.Errorf("read management certificate: %w", err))
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("management certificate %s holds no PEM block", certPath)
	}
	pinned, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse management certificate %s: %w", certPath, err)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // a loopback call must never go through a proxy
	tr.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Standard verification is replaced, not skipped: VerifyConnection below
		// accepts only the pinned leaf.
		InsecureSkipVerify: true, //nolint:gosec // pinned via VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !cs.PeerCertificates[0].Equal(pinned) {
				return fmt.Errorf("the management API presented a certificate other than %s", certPath)
			}
			return nil
		},
	}
	return &http.Client{Transport: tr}, nil
}
