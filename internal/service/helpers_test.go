//go:build linux

package service

import (
	"strings"
	"testing"
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
	got := f.lines()
	if len(got) != len(want) {
		t.Fatalf("call count = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, got[i], want[i])
		}
	}
}
