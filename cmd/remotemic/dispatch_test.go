//go:build linux

package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
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
		if !strings.Contains(out.String(), "remotemic") {
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

// Command group and help spellings, named once so goconst does not flag the
// repeated literals in the routing tables.
const (
	cmdDevices = "devices"
	cmdToken   = "token"
	cmdHelp    = "help"
)

func TestDispatchDevicesListRoutes(t *testing.T) {
	var called bool
	defer stubListDevices(func(io.Writer) error { called = true; return nil })()
	if code := dispatch([]string{cmdDevices, "list"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("devices list exit %d, want 0", code)
	}
	if !called {
		t.Fatal("devices list did not route to listDevicesFn")
	}
}

// TestDispatchDevicesUsageErrors asserts a bare `devices`, an unknown action,
// and a stray argument to `devices list` exit 2 (usage) or 1 without listing.
func TestDispatchDevicesUsageErrors(t *testing.T) {
	var called bool
	defer stubListDevices(func(io.Writer) error { called = true; return nil })()
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{cmdDevices}, 2},
		{[]string{cmdDevices, "show"}, 2},
		{[]string{cmdDevices, "list", "extra"}, 1},
	} {
		if code := dispatch(tc.args, &bytes.Buffer{}, &bytes.Buffer{}); code != tc.code {
			t.Errorf("%v: exit %d, want %d", tc.args, code, tc.code)
		}
	}
	if called {
		t.Fatal("a usage error still listed devices")
	}
}

// TestDispatchRemovedCommandsAreUnknown pins the noun-verb scheme: the old
// flat spellings are unknown commands rather than silent aliases.
func TestDispatchRemovedCommandsAreUnknown(t *testing.T) {
	for _, arg := range []string{"init", "list-devices"} {
		var errb bytes.Buffer
		if code := dispatch([]string{arg}, &bytes.Buffer{}, &errb); code != 2 {
			t.Errorf("%q: exit %d, want 2", arg, code)
		}
		if !strings.Contains(errb.String(), "unknown command") {
			t.Errorf("%q: stderr = %q, want an unknown-command error", arg, errb.String())
		}
	}
}

// TestDispatchVersionFlagAnyPosition asserts the version flag works after
// another flag instead of starting the appliance.
func TestDispatchVersionFlagAnyPosition(t *testing.T) {
	defer stubServe(func([]string, io.Writer) error {
		t.Error("version flag after --config started serve")
		return nil
	})()
	var out bytes.Buffer
	dispatch([]string{flagConfig, cfgPathX, "-version"}, &out, &bytes.Buffer{})
	if !strings.Contains(out.String(), "remotemic") {
		t.Errorf("-config x -version did not print version: %q", out.String())
	}
}

// TestDispatchHelpPaths asserts every help spelling exits 0 with usage on
// stdout and never starts the appliance.
func TestDispatchHelpPaths(t *testing.T) {
	defer stubServe(func([]string, io.Writer) error {
		t.Error("a help request routed to serve")
		return nil
	})()
	for _, args := range [][]string{
		{cmdHelp}, {"-h"}, {"--help"},
		{cmdToken, "-h"}, {cmdToken, cmdHelp},
		{cmdDevices, "-h"}, {cmdDevices, cmdHelp},
	} {
		var out, errb bytes.Buffer
		if code := dispatch(args, &out, &errb); code != 0 {
			t.Errorf("%v: exit %d stderr %q, want 0", args, code, errb.String())
		}
		if out.Len() == 0 {
			t.Errorf("%v: no usage on stdout", args)
		}
	}
}

// TestParseServeFlagsUsesConfigEnv asserts a bare serve invocation takes its
// config path from $REMOTEMIC_CONFIG.
func TestParseServeFlagsUsesConfigEnv(t *testing.T) {
	const p = "/etc/remotemic/from-env.yaml"
	t.Setenv(configEnv, p)
	cfgPath, _, check, err := parseServeFlags(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseServeFlags: %v", err)
	}
	if cfgPath != p {
		t.Fatalf("cfgPath = %q, want %q from $%s", cfgPath, p, configEnv)
	}
	if check {
		t.Fatal("check should default to false for a bare invocation")
	}
}

var errServeStub = stubError("serve failed")

type stubError string

func (e stubError) Error() string { return string(e) }
