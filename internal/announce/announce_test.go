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
		{"non-ASCII case kept", []string{"Äänikortti", "äänikortti"}, []string{"Äänikortti", "äänikortti"}},
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

// fakeResponders stands in for dnssd. Building a responder fails while
// buildFails is positive (a socket that cannot be opened); a built
// responder's Respond fails at once while respondFails is positive (a
// registration that gives up), otherwise runs until its context ends, or, with
// runFor set, fails after running that long. It records every build, every
// Respond, and every name registered.
type fakeResponders struct {
	mu           sync.Mutex
	made         int
	responds     int
	buildFails   int
	respondFails int
	runFor       time.Duration
	addFails     bool
	names        []string
}

// fakeResponder is one responder built by fakeResponders.
type fakeResponder struct {
	dnssd.Responder
	r *fakeResponders
}

//nolint:gocritic // dnssd.Responder fixes the by-value signature.
func (f *fakeResponder) Add(srv dnssd.Service) (dnssd.ServiceHandle, error) {
	f.r.mu.Lock()
	defer f.r.mu.Unlock()
	if f.r.addFails {
		return nil, errTestAdd
	}
	f.r.names = append(f.r.names, srv.Name)
	return nil, nil //nolint:nilnil // the caller ignores the handle
}

func (f *fakeResponder) Respond(ctx context.Context) error {
	f.r.mu.Lock()
	f.r.responds++
	fail := f.r.respondFails > 0
	if fail {
		f.r.respondFails--
	}
	runFor := f.r.runFor
	f.r.mu.Unlock()
	if fail {
		return errTestProbe
	}
	if runFor > 0 {
		t := time.NewTimer(runFor)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return errTestProbe
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

var (
	errTestProbe  = errors.New("probe gave up")
	errTestSocket = errors.New("no socket")
	errTestAdd    = errors.New("bad service")
)

// TestRunReturnsRefusedService pins that a service the responder refuses ends
// Run with an error instead of retrying: no retry can fix it, so the caller
// warns and serves without discovery.
func TestRunReturnsRefusedService(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		withResponders(t, &fakeResponders{addFails: true})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- Run(ctx, []Info{{Name: testMic, Path: "/a", Port: 18999, Codec: testCodec, Rate: 48000, Channels: 1}})
		}()
		// Past several backoff delays, so a Run that retried the refused
		// service would still be retrying.
		time.Sleep(time.Hour)
		synctest.Wait()
		select {
		case err := <-done:
			if !errors.Is(err, errTestAdd) {
				t.Errorf("got Run error %v, want the refused service's", err)
			}
		default:
			t.Error("Run is still retrying a service the responder refused")
			cancel()
			<-done
		}
	})
}

// counts returns how many responders were built and how many Respond calls
// were made.
func (r *fakeResponders) counts() (made, responds int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.made, r.responds
}

// withResponders swaps the responder seam for fakes. A test using it does not
// run in parallel: the seam is package state.
func withResponders(t *testing.T, r *fakeResponders) *fakeResponders {
	t.Helper()
	prev := newResponder
	newResponder = func() (dnssd.Responder, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.buildFails > 0 {
			r.buildFails--
			return nil, errTestSocket
		}
		r.made++
		return &fakeResponder{r: r}, nil
	}
	t.Cleanup(func() { newResponder = prev })
	return r
}

// TestRunRetriesFailedResponder pins that a responder that cannot start is
// retried on the backoff until one runs, and that a built responder is reused
// for the retry: dnssd has no Close, so building one per attempt would leak its
// socket. The services it registers carry distinct names.
func TestRunRetriesFailedResponder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := withResponders(t, &fakeResponders{buildFails: 1, respondFails: 2})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		info := Info{Name: testMic, Path: "/a", Port: 18999, Codec: testCodec, Rate: 48000, Channels: 1}
		go func() { done <- Run(ctx, []Info{info, info}) }()
		synctest.Wait()
		if made, responds := r.counts(); made != 0 || responds != 0 {
			t.Fatalf("after a failed build: %d built, %d Respond calls; want none", made, responds)
		}
		steps := []struct{ made, responds int }{
			{1, 1}, // built, its registration fails
			{1, 2}, // the same responder retried, fails again
			{1, 3}, // the same responder retried, runs
		}
		for i, want := range steps {
			time.Sleep(responderBackoff[i])
			synctest.Wait()
			if made, responds := r.counts(); made != want.made || responds != want.responds {
				t.Fatalf("after delay %d: %d built, %d Respond calls; want %d and %d", i+1, made, responds, want.made, want.responds)
			}
		}
		// It runs, and nothing is retried or rebuilt while it does.
		time.Sleep(time.Hour)
		synctest.Wait()
		if made, responds := r.counts(); made != 1 || responds != 3 {
			t.Errorf("while one runs: %d built, %d Respond calls; want 1 and 3", made, responds)
		}
		cancel()
		if err := <-done; err != nil {
			t.Errorf("got Run error %v, want nil on cancel", err)
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if want := []string{testMic, testMic + " #2"}; !slices.Equal(r.names, want) {
			t.Errorf("got registered names %q, want %q once", r.names, want)
		}
	})
}

// TestRunBackoffRestartsAfterStableRun pins that a responder that ran for
// responderStable before failing starts the backoff over from the shortest
// delay, rather than climbing on from the failures before it.
func TestRunBackoffRestartsAfterStableRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := withResponders(t, &fakeResponders{respondFails: 1, runFor: responderStable})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() {
			_ = Run(ctx, []Info{{Name: testMic, Path: "/a", Port: 18999, Codec: testCodec, Rate: 48000, Channels: 1}})
		}()
		// The first Respond fails at once; the second, one delay later, runs
		// for responderStable and then fails. Having run that long, it is
		// retried after the shortest delay again, not the second.
		time.Sleep(responderBackoff[0] + responderStable + responderBackoff[0])
		synctest.Wait()
		if _, responds := r.counts(); responds != 3 {
			t.Errorf("got %d Respond calls one shortest delay after a stable run failed, want 3", responds)
		}
	})
}

// TestRunCancelDuringBackoff pins that a cancel while Run waits to retry a
// failed responder ends it at once, without waiting out the delay or starting
// another attempt.
func TestRunCancelDuringBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := withResponders(t, &fakeResponders{respondFails: 1 << 30})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			done <- Run(ctx, []Info{{Name: testMic, Path: "/a", Port: 18999, Codec: testCodec, Rate: 48000, Channels: 1}})
		}()
		synctest.Wait()
		_, before := r.counts()
		start := time.Now()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("got Run error %v, want nil on cancel", err)
		}
		if waited := time.Since(start); waited != 0 {
			t.Errorf("Run returned %s after the cancel, want at once (it waited out the backoff)", waited)
		}
		if _, after := r.counts(); after != before {
			t.Errorf("got %d Respond calls after the cancel, want none", after-before)
		}
	})
}
