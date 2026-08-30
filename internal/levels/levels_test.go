package levels

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
)

const nameGarden = "garden"

// pcm builds an S16LE byte slice from samples.
func pcm(samples ...int16) []byte {
	b := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(s))
	}
	return b
}

// repeat returns n copies of s.
func repeat(s int16, n int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// meterWithSubs returns a meter whose subscriber gate is already open.
func meterWithSubs() *Meter {
	var subs atomic.Int32
	subs.Store(1)
	return &Meter{subs: &subs}
}

func approx(t *testing.T, got, want, tol float64, what string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.4f, want %.4f (+/- %.4f)", what, got, want, tol)
	}
}

func TestMeterSilence(t *testing.T) {
	m := meterWithSubs()
	m.Observe(pcm(repeat(0, 480)...))
	d := m.sample(nameGarden)
	if d.Name != nameGarden {
		t.Errorf("name = %q, want garden", d.Name)
	}
	approx(t, d.PeakDbfs, dbfsFloor, 0.001, "peak")
	approx(t, d.RmsDbfs, dbfsFloor, 0.001, "rms")
	if d.Clipped {
		t.Error("silence must not be clipped")
	}
}

func TestMeterFullScalePositive(t *testing.T) {
	m := meterWithSubs()
	m.Observe(pcm(repeat(math.MaxInt16, 256)...))
	d := m.sample("x")
	approx(t, d.PeakDbfs, 0, 0.01, "peak")
	approx(t, d.RmsDbfs, 0, 0.01, "rms")
	if !d.Clipped {
		t.Error("full-scale positive samples must be clipped")
	}
}

func TestMeterFullScaleNegative(t *testing.T) {
	m := meterWithSubs()
	m.Observe(pcm(repeat(math.MinInt16, 256)...))
	d := m.sample("x")
	// |MinInt16| == 32768 == fullScale, so exactly 0 dBFS.
	approx(t, d.PeakDbfs, 0, 0.0001, "peak")
	approx(t, d.RmsDbfs, 0, 0.0001, "rms")
	if !d.Clipped {
		t.Error("full-scale negative samples must be clipped")
	}
}

func TestMeterHalfScale(t *testing.T) {
	m := meterWithSubs()
	m.Observe(pcm(repeat(16384, 300)...)) // 0.5 of full scale
	d := m.sample("x")
	const wantDb = -6.0206 // 20*log10(0.5)
	approx(t, d.PeakDbfs, wantDb, 0.01, "peak")
	approx(t, d.RmsDbfs, wantDb, 0.01, "rms")
	if d.Clipped {
		t.Error("half-scale must not be clipped")
	}
}

func TestMeterReadResets(t *testing.T) {
	m := meterWithSubs()
	m.Observe(pcm(repeat(16384, 100)...))
	_ = m.sample("x")
	// No Observe between samples: the window is empty, so silence.
	d := m.sample("x")
	approx(t, d.PeakDbfs, dbfsFloor, 0.001, "peak after reset")
	approx(t, d.RmsDbfs, dbfsFloor, 0.001, "rms after reset")
}

func TestMeterSubscriberGate(t *testing.T) {
	var subs atomic.Int32 // starts at 0: no subscribers
	m := &Meter{subs: &subs}
	m.Observe(pcm(repeat(math.MaxInt16, 256)...))
	d := m.sample("x")
	if d.PeakDbfs != dbfsFloor || d.RmsDbfs != dbfsFloor || d.Clipped {
		t.Errorf("with no subscribers Observe must be a no-op, got %+v", d)
	}
	subs.Store(1)
	m.Observe(pcm(repeat(math.MaxInt16, 256)...))
	d = m.sample("x")
	if d.PeakDbfs == dbfsFloor {
		t.Error("with a subscriber Observe must accumulate")
	}
}

func TestLevelsEventMatchesContract(t *testing.T) {
	// The hand-rolled JSON must unmarshal cleanly into the generated contract
	// types, so the SSE payload and the OpenAPI schema cannot drift apart.
	h := NewHub()
	m := h.Meter(nameGarden)
	h.subs.Store(1)
	m.Observe(pcm(repeat(16384, 480)...))
	ev := h.levelsEvent()
	if ev.Name != "levels" {
		t.Fatalf("event name = %q, want levels", ev.Name)
	}
	var contract mgmtapi.LevelsEvent
	if err := json.Unmarshal(ev.Data, &contract); err != nil {
		t.Fatalf("levels payload does not match contract: %v", err)
	}
	if len(contract.Devices) != 1 || contract.Devices[0].Name != nameGarden {
		t.Fatalf("contract devices wrong: %+v", contract.Devices)
	}
	// Assert the numeric and bool fields decode into the contract type, so a
	// renamed json tag on DeviceLevels (peakDbfs/rmsDbfs/clipped) fails the test
	// instead of silently leaving the mgmtapi field zero-valued. 16384 is half
	// scale, so both peak and RMS are 20*log10(0.5) = -6.02 dBFS.
	const wantDb = -6.0206
	d := contract.Devices[0]
	if math.Abs(d.PeakDbfs-wantDb) > 0.01 {
		t.Errorf("peakDbfs tag not carried into contract: got %v, want ~%v", d.PeakDbfs, wantDb)
	}
	if math.Abs(d.RmsDbfs-wantDb) > 0.01 {
		t.Errorf("rmsDbfs tag not carried into contract: got %v, want ~%v", d.RmsDbfs, wantDb)
	}
	if d.Clipped {
		t.Errorf("clipped tag: got true, want false")
	}
}

// startTestHub runs a hub with a fast cadence for streaming tests.
func startTestHub(t *testing.T) *Hub {
	t.Helper()
	h := NewHub()
	h.interval = 10 * time.Millisecond
	h.heartbeat = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go h.Run(ctx)
	return h
}

func TestEventsHandlerStreamsLevels(t *testing.T) {
	h := startTestHub(t)
	h.Meter(nameGarden)

	srv := httptest.NewServer(h.EventsHandler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	name, data := readEvent(t, resp.Body)
	if name != "levels" {
		t.Fatalf("first event = %q, want levels", name)
	}
	var le mgmtapi.LevelsEvent
	if err := json.Unmarshal([]byte(data), &le); err != nil {
		t.Fatalf("levels data: %v", err)
	}
	if len(le.Devices) != 1 || le.Devices[0].Name != nameGarden {
		t.Fatalf("devices = %+v, want one garden", le.Devices)
	}
}

func TestEventsHandlerHeartbeatSurvivesLevelsFilter(t *testing.T) {
	h := startTestHub(t)
	h.Meter(nameGarden)

	srv := httptest.NewServer(h.EventsHandler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Subscribe to a type that never fires; heartbeat must still arrive.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"?events=nonexistent", http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	name, _ := readEvent(t, resp.Body)
	if name != "heartbeat" {
		t.Fatalf("first event = %q, want heartbeat (levels filtered out)", name)
	}
}

func TestSubscribeTracksCount(t *testing.T) {
	h := NewHub()
	if got := h.subs.Load(); got != 0 {
		t.Fatalf("initial subs = %d, want 0", got)
	}
	_, cancel := h.Subscribe()
	if got := h.subs.Load(); got != 1 {
		t.Fatalf("after subscribe subs = %d, want 1", got)
	}
	cancel()
	if got := h.subs.Load(); got != 0 {
		t.Fatalf("after cancel subs = %d, want 0", got)
	}
	cancel() // idempotent
	if got := h.subs.Load(); got != 0 {
		t.Fatalf("double cancel subs = %d, want 0", got)
	}
}

func TestSubscribeResetsResidualMeters(t *testing.T) {
	// While no client is subscribed the sampler stops draining the meters, so a
	// meter can hold residual from a previous session. The first re-subscribe
	// must clear it so the new session does not open on stale levels.
	h := NewHub()
	m := h.Meter("x")

	_, cancel := h.Subscribe() // subs=1: metering active
	m.Observe(pcm(repeat(math.MaxInt16, 256)...))
	cancel() // subs=0: residual left undrained in m

	_, cancel2 := h.Subscribe() // first subscriber again: must reset m
	defer cancel2()
	d := m.sample("x")
	if d.PeakDbfs != dbfsFloor || d.RmsDbfs != dbfsFloor || d.Clipped {
		t.Errorf("residual not cleared on re-subscribe: %+v", d)
	}
}

// readEvent reads one SSE event (its type and data) from r.
func readEvent(t *testing.T, r io.Reader) (name, data string) {
	t.Helper()
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if name != "" {
				return name, data
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan SSE: %v", err)
	}
	t.Fatal("stream ended before a full event")
	return "", ""
}
