//go:build linux

package service

import (
	"strings"
	"testing"
)

// Repeated fixture strings, factored out to satisfy goconst and keep the
// expected event sequences readable.
const (
	evReload    = "reload"
	nologinPath = "/usr/sbin/nologin"
)

// call records one Runner invocation for assertions.
type call struct {
	name string
	args []string
}

func (c call) line() string { return strings.TrimSpace(c.name + " " + strings.Join(c.args, " ")) }

// fakeRunner is a recording Runner. resp, when set, supplies the output and
// error for a call keyed on its name and arguments; otherwise the call succeeds
// with no output.
type fakeRunner struct {
	calls []call
	resp  func(name string, args []string) ([]byte, error)
}

func (f *fakeRunner) run(name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{name: name, args: append([]string(nil), args...)})
	if f.resp != nil {
		return f.resp(name, args)
	}
	return nil, nil
}

// lines returns each recorded call as a single "name arg arg" string, in order.
func (f *fakeRunner) lines() []string {
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.line()
	}
	return out
}

// wantSeq asserts the recorded call lines exactly match want, in order.
func (f *fakeRunner) wantSeq(t *testing.T, want ...string) {
	t.Helper()
	wantSeq(t, f.lines(), want)
}

// wantSeq asserts got matches want exactly, in order, with a readable diff.
func wantSeq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("event count = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// fakeInit is a recording InitSystem that logs its lifecycle calls into a
// shared event slice, so a test can assert the ordering of systemd actions
// relative to filesystem and user operations.
type fakeInit struct {
	events  *[]string
	present bool
	enabled bool
	active  bool
}

func (f *fakeInit) log(s string)  { *f.events = append(*f.events, s) }
func (f *fakeInit) Present() bool { return f.present }
func (f *fakeInit) DaemonReload() error {
	f.log(evReload)
	return nil
}

func (f *fakeInit) Enable(unit string, now bool) error {
	if now {
		f.log("enable --now " + unit)
	} else {
		f.log("enable " + unit)
	}
	return nil
}

func (f *fakeInit) Disable(unit string) error {
	f.log("disable " + unit)
	return nil
}

func (f *fakeInit) Stop(unit string) error {
	f.log("stop " + unit)
	return nil
}

func (f *fakeInit) IsEnabled(string) (bool, error) { return f.enabled, nil }
func (f *fakeInit) IsActive(string) (bool, error)  { return f.active, nil }
