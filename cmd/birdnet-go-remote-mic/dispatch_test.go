//go:build linux

package main

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
)

// stubServe swaps the serve seam so dispatch routing can be tested without
// starting the real appliance (which opens hardware and blocks).
func stubServe(fn func([]string, io.Writer) error) func() {
	prev := serveFn
	serveFn = fn
	return func() { serveFn = prev }
}

func stubListDevices(fn func(io.Writer) error) func() {
	prev := listDevicesFn
	listDevicesFn = fn
	return func() { listDevicesFn = prev }
}

func TestDispatchVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		var out, errb bytes.Buffer
		if code := dispatch([]string{arg}, &out, &errb); code != 0 {
			t.Errorf("%q: exit %d, want 0", arg, code)
		}
		if !strings.Contains(out.String(), "birdnet-go-remote-mic") {
			t.Errorf("%q: version not printed: %q", arg, out.String())
		}
	}
}

func TestDispatchNoArgsServes(t *testing.T) {
	var called bool
	defer stubServe(func(args []string, _ io.Writer) error {
		called = true
		if args != nil {
			t.Errorf("bare invocation should pass nil serve args, got %v", args)
		}
		return nil
	})()
	if code := dispatch(nil, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !called {
		t.Fatal("bare invocation did not route to serve")
	}
}

func TestDispatchImplicitServeWithFlags(t *testing.T) {
	var got []string
	defer stubServe(func(args []string, _ io.Writer) error { got = args; return nil })()
	dispatch([]string{flagListen, listenAddr9}, &bytes.Buffer{}, &bytes.Buffer{})
	if len(got) != 2 || got[0] != flagListen || got[1] != listenAddr9 {
		t.Fatalf("implicit serve args = %v", got)
	}
}

func TestDispatchServeSubcommand(t *testing.T) {
	var got []string
	defer stubServe(func(args []string, _ io.Writer) error { got = args; return nil })()
	dispatch([]string{"serve", flagListen, listenAddr9}, &bytes.Buffer{}, &bytes.Buffer{})
	if len(got) != 2 || got[0] != flagListen {
		t.Fatalf("serve args = %v", got)
	}
}

func TestDispatchUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := dispatch([]string{"bogus"}, &out, &errb); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "unknown command") {
		t.Errorf("no unknown-command message: %q", errb.String())
	}
}

func TestDispatchServeErrorExits1(t *testing.T) {
	defer stubServe(func([]string, io.Writer) error { return errServeStub })()
	if code := dispatch([]string{"serve"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
		t.Fatalf("serve error exit %d, want 1", code)
	}
}

func TestDispatchInitRoutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	var out, errb bytes.Buffer
	if code := dispatch([]string{"init", flagConfig, path, "-quiet"}, &out, &errb); code != 0 {
		t.Fatalf("init exit %d, want 0; stderr=%q", code, errb.String())
	}
	if _, err := config.Load(path); err != nil {
		t.Fatalf("init did not write a valid config: %v", err)
	}
}

func TestDispatchListDevicesRoutes(t *testing.T) {
	var called bool
	defer stubListDevices(func(io.Writer) error { called = true; return nil })()
	dispatch([]string{"list-devices"}, &bytes.Buffer{}, &bytes.Buffer{})
	if !called {
		t.Fatal("list-devices did not route to listDevicesFn")
	}
}

func TestDispatchDeprecatedListDevicesFlag(t *testing.T) {
	for _, spelling := range []string{"-list-devices", "--list-devices"} {
		var called bool
		restore := stubListDevices(func(io.Writer) error { called = true; return nil })
		var errb bytes.Buffer
		dispatch([]string{spelling}, &bytes.Buffer{}, &errb)
		restore()
		if !called {
			t.Errorf("%q did not route to list-devices", spelling)
		}
		if !strings.Contains(errb.String(), "deprecated") {
			t.Errorf("%q: no deprecation notice: %q", spelling, errb.String())
		}
	}
}

// TestDispatchLegacySelectorAnyOrder asserts the deprecated selector flags still
// work when they follow another flag, matching the old flat parser (e.g.
// `-config x.yaml -list-devices`).
func TestDispatchLegacySelectorAnyOrder(t *testing.T) {
	var listed bool
	restore := stubListDevices(func(io.Writer) error { listed = true; return nil })
	var out, errb bytes.Buffer
	dispatch([]string{flagConfig, cfgPathX, "-list-devices"}, &out, &errb)
	restore()
	if !listed {
		t.Error("-config x -list-devices did not list devices")
	}
	out.Reset()
	errb.Reset()
	dispatch([]string{flagConfig, cfgPathX, "-version"}, &out, &errb)
	if !strings.Contains(out.String(), "birdnet-go-remote-mic") {
		t.Errorf("-config x -version did not print version: %q", out.String())
	}
}

var errServeStub = stubError("serve failed")

type stubError string

func (e stubError) Error() string { return string(e) }
