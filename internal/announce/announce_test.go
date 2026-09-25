package announce

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/brutella/dnssd"
)

const (
	testCodec   = "L16"
	testPath    = "/garden"
	testVersion = "1.2.3"
	testMic     = "mic"
)

func TestTXTRecords(t *testing.T) {
	txt := txtRecords(&Info{Name: "garden-mic", Path: testPath, Codec: testCodec, Rate: 256000, Channels: 1, Version: testVersion})
	want := map[string]string{
		"txtvers": "1",
		"model":   "birdnet-go-remote-mic",
		"version": testVersion,
		"codec":   testCodec,
		"rate":    "256000",
		"ch":      "1",
		"path":    testPath,
		"auth":    "none",
	}
	if len(txt) != len(want) {
		t.Fatalf("txt has %d keys, want %d: %v", len(txt), len(want), txt)
	}
	for k, v := range want {
		if txt[k] != v {
			t.Errorf("txt[%q] = %q, want %q", k, txt[k], v)
		}
	}
}

func TestTXTRecordsAdvertiseTokenAuth(t *testing.T) {
	txt := txtRecords(&Info{Name: "garden-mic", Path: testPath, Codec: testCodec, Rate: 48000, Channels: 1, Version: testVersion, AuthRequired: true})
	if txt["auth"] != "token" {
		t.Errorf("txt[auth] = %q, want token when a token is required", txt["auth"])
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []Info{{Name: "test-mic-cancel", Path: "/stream", Port: 18999, Codec: testCodec, Rate: 48000, Channels: 1, Version: "test"}})
	}()
	// No readiness sleep: Run returns nil for a cancelled context whether the
	// cancel lands during responder setup or while it is blocked in Respond
	// (dnssd maps context.Canceled to a clean stop), so cancelling immediately
	// exercises the same "stops on cancel" contract without a flaky timing race.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v on cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop within 5s of cancel")
	}
}

func TestRunRejectsNoServices(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := Run(ctx, nil); err == nil {
		t.Fatal("Run with no services should error, not advertise nothing silently")
	}
}

func TestDistinctNames(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", NameBudget)
	umlauts := strings.Repeat("ä", NameBudget/2)
	const (
		g  = "garden"
		g2 = g + " #2"
		g3 = g + " #3"
	)
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{"distinct kept", []string{g, "pond"}, []string{g, "pond"}},
		{"duplicate numbered", []string{g, g, g}, []string{g, g2, g3}},
		{"case folded", []string{"Garden", g}, []string{"Garden", g2}},
		{"numbered name taken", []string{g, g2, g}, []string{g, g2, g3}},
		{"cut to budget", []string{long, long}, []string{long, long[:NameBudget-3] + " #2"}},
		{"cut on a rune", []string{umlauts, umlauts}, []string{umlauts, strings.Repeat("ä", (NameBudget-3)/2) + " #2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			infos := make([]Info, len(tc.in))
			for i, n := range tc.in {
				infos[i] = Info{Name: n}
			}
			got := distinctNames(infos)
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			for _, n := range got {
				if len(n) > NameBudget || !utf8.ValidString(n) {
					t.Errorf("name %q: got %d bytes (budget %d) and valid UTF-8 %v, want within budget and valid", n, len(n), NameBudget, utf8.ValidString(n))
				}
			}
		})
	}
}

// fakeResponders builds fake responders: the first fails of them fail at
// once, the rest block until their context ends. It records every name
// registered with them.
type fakeResponders struct {
	mu    sync.Mutex
	made  int
	fails int
	names []string
}

// fakeResponder is one responder built by fakeResponders.
type fakeResponder struct {
	dnssd.Responder
	r    *fakeResponders
	fail bool
}

//nolint:gocritic // dnssd.Responder fixes the by-value signature.
func (f *fakeResponder) Add(srv dnssd.Service) (dnssd.ServiceHandle, error) {
	f.r.mu.Lock()
	defer f.r.mu.Unlock()
	f.r.names = append(f.r.names, srv.Name)
	return nil, nil //nolint:nilnil // the caller ignores the handle
}

func (f *fakeResponder) Respond(ctx context.Context) error {
	if f.fail {
		return errTestNoInterface
	}
	<-ctx.Done()
	return ctx.Err()
}

var errTestNoInterface = errors.New("no usable interface")

func (r *fakeResponders) built() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.made
}

// withResponders swaps the responder seam for fakes whose first fails
// responders fail. A test using it does not run in parallel: the seam is
// package state.
func withResponders(t *testing.T, fails int) *fakeResponders {
	t.Helper()
	r := &fakeResponders{fails: fails}
	prev := newResponder
	newResponder = func() (dnssd.Responder, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.made++
		return &fakeResponder{r: r, fail: r.made <= r.fails}, nil
	}
	t.Cleanup(func() { newResponder = prev })
	return r
}

// TestRunRestartsFailedResponder pins that a responder that cannot start is
// restarted on the backoff until one runs, so an appliance booted before its
// network is up becomes discoverable without a rebuild of its advertisement,
// and that the services it registers carry distinct names.
func TestRunRestartsFailedResponder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := withResponders(t, 2)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		info := Info{Name: testMic, Path: "/a", Port: 18999, Codec: testCodec, Rate: 48000, Channels: 1}
		go func() { done <- Run(ctx, []Info{info, info}) }()
		synctest.Wait()
		if got := r.built(); got != 1 {
			t.Fatalf("got %d responders at start, want 1", got)
		}
		time.Sleep(responderBackoff[0])
		synctest.Wait()
		if got := r.built(); got != 2 {
			t.Fatalf("got %d responders after the first delay, want 2", got)
		}
		time.Sleep(responderBackoff[1])
		synctest.Wait()
		if got := r.built(); got != 3 {
			t.Fatalf("got %d responders after the second delay, want 3", got)
		}
		// The third runs, and nothing is rebuilt while it does.
		time.Sleep(time.Hour)
		synctest.Wait()
		if got := r.built(); got != 3 {
			t.Errorf("got %d responders while one runs, want 3", got)
		}
		cancel()
		if err := <-done; err != nil {
			t.Errorf("got Run error %v, want nil on cancel", err)
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if want := []string{testMic, testMic + " #2"}; len(r.names) < 2 || !slices.Equal(r.names[len(r.names)-2:], want) {
			t.Errorf("got registered names %q, want the running responder's to be %q", r.names, want)
		}
	})
}

// TestRunCancelDuringBackoff pins that a cancel while Run waits to restart a
// failed responder ends it cleanly.
func TestRunCancelDuringBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		withResponders(t, 1<<30)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			done <- Run(ctx, []Info{{Name: testMic, Path: "/a", Port: 18999, Codec: testCodec, Rate: 48000, Channels: 1}})
		}()
		synctest.Wait()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("got Run error %v, want nil on cancel", err)
		}
	})
}
