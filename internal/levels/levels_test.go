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

// meterN returns a meter with the given channel count and its subscriber gate
// already open.
func meterN(channels int) *Meter {
	var subs atomic.Int32
	subs.Store(1)
	return &Meter{subs: &subs, ch: make([]chanAccum, channels)}
}

// meterWithSubs returns a mono meter whose subscriber gate is already open.
func meterWithSubs() *Meter { return meterN(1) }

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
	approx(t, d.Channels[0].PeakDbfs, dbfsFloor, 0.001, "peak")
	approx(t, d.Channels[0].RmsDbfs, dbfsFloor, 0.001, "rms")
	if d.Channels[0].Clipped {
		t.Error("silence must not be clipped")
	}
}

func TestMeterFullScalePositive(t *testing.T) {
	m := meterWithSubs()
	m.Observe(pcm(repeat(math.MaxInt16, 256)...))
	d := m.sample("x")
	approx(t, d.Channels[0].PeakDbfs, 0, 0.01, "peak")
	approx(t, d.Channels[0].RmsDbfs, 0, 0.01, "rms")
	if !d.Channels[0].Clipped {
		t.Error("full-scale positive samples must be clipped")
	}
}

func TestMeterFullScaleNegative(t *testing.T) {
	m := meterWithSubs()
	m.Observe(pcm(repeat(math.MinInt16, 256)...))
	d := m.sample("x")
	// |MinInt16| == 32768 == fullScale, so exactly 0 dBFS.
	approx(t, d.Channels[0].PeakDbfs, 0, 0.0001, "peak")
	approx(t, d.Channels[0].RmsDbfs, 0, 0.0001, "rms")
	if !d.Channels[0].Clipped {
		t.Error("full-scale negative samples must be clipped")
	}
}

func TestMeterHalfScale(t *testing.T) {
	m := meterWithSubs()
	m.Observe(pcm(repeat(16384, 300)...)) // 0.5 of full scale
	d := m.sample("x")
	const wantDb = -6.0206 // 20*log10(0.5)
	approx(t, d.Channels[0].PeakDbfs, wantDb, 0.01, "peak")
	approx(t, d.Channels[0].RmsDbfs, wantDb, 0.01, "rms")
	if d.Channels[0].Clipped {
		t.Error("half-scale must not be clipped")
	}
}

func TestMeterReadResets(t *testing.T) {
	m := meterWithSubs()
	m.Observe(pcm(repeat(16384, 100)...))
	_ = m.sample("x")
	// No Observe between samples: the window is empty, so silence.
	d := m.sample("x")
	approx(t, d.Channels[0].PeakDbfs, dbfsFloor, 0.001, "peak after reset")
	approx(t, d.Channels[0].RmsDbfs, dbfsFloor, 0.001, "rms after reset")
}

func TestMeterSubscriberGate(t *testing.T) {
	var subs atomic.Int32 // starts at 0: no subscribers
	m := &Meter{subs: &subs, ch: make([]chanAccum, 1)}
	m.Observe(pcm(repeat(math.MaxInt16, 256)...))
	d := m.sample("x")
	if d.Channels[0].PeakDbfs != dbfsFloor || d.Channels[0].RmsDbfs != dbfsFloor || d.Channels[0].Clipped {
		t.Errorf("with no subscribers Observe must be a no-op, got %+v", d)
	}
	subs.Store(1)
	m.Observe(pcm(repeat(math.MaxInt16, 256)...))
	d = m.sample("x")
	if d.Channels[0].PeakDbfs == dbfsFloor {
		t.Error("with a subscriber Observe must accumulate")
	}
}

func TestLevelsEventMatchesContract(t *testing.T) {
	// The hand-rolled JSON must unmarshal cleanly into the generated contract
	// types, so the SSE payload and the OpenAPI schema cannot drift apart.
	h := NewHub()
	m := h.Meter(nameGarden, 1)
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
	if math.Abs(d.Channels[0].PeakDbfs-wantDb) > 0.01 {
		t.Errorf("peakDbfs tag not carried into contract: got %v, want ~%v", d.Channels[0].PeakDbfs, wantDb)
	}
	if math.Abs(d.Channels[0].RmsDbfs-wantDb) > 0.01 {
		t.Errorf("rmsDbfs tag not carried into contract: got %v, want ~%v", d.Channels[0].RmsDbfs, wantDb)
	}
	if d.Channels[0].Clipped {
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
	h.Meter(nameGarden, 1)

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
	h.Meter(nameGarden, 1)

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
	m := h.Meter("x", 1)

	_, cancel := h.Subscribe() // subs=1: metering active
	m.Observe(pcm(repeat(math.MaxInt16, 256)...))
	cancel() // subs=0: residual left undrained in m

	_, cancel2 := h.Subscribe() // first subscriber again: must reset m
	defer cancel2()
	d := m.sample("x")
	if d.Channels[0].PeakDbfs != dbfsFloor || d.Channels[0].RmsDbfs != dbfsFloor || d.Channels[0].Clipped {
		t.Errorf("residual not cleared on re-subscribe: %+v", d)
	}
}

// TestMeterPerChannel checks that a multi-channel meter deinterleaves the lanes
// and reports independent levels: a half-scale left channel and a silent right
// channel must not bleed into each other, and each carries its own index.
func TestMeterPerChannel(t *testing.T) {
	var subs atomic.Int32
	subs.Store(1)
	m := &Meter{subs: &subs, ch: make([]chanAccum, 2)}
	// Interleaved L,R frames: left at half scale (16384), right silent.
	const frames = 300
	samples := make([]int16, 0, frames*2)
	for i := 0; i < frames; i++ {
		samples = append(samples, 16384, 0)
	}
	m.Observe(pcm(samples...))

	d := m.sample(nameGarden)
	if len(d.Channels) != 2 {
		t.Fatalf("channels = %d, want 2", len(d.Channels))
	}
	if d.Channels[0].Channel != 0 || d.Channels[1].Channel != 1 {
		t.Errorf("channel indices = %d,%d, want 0,1", d.Channels[0].Channel, d.Channels[1].Channel)
	}
	const halfDb = -6.0206 // 20*log10(0.5)
	approx(t, d.Channels[0].PeakDbfs, halfDb, 0.01, "left peak")
	approx(t, d.Channels[0].RmsDbfs, halfDb, 0.01, "left rms")
	approx(t, d.Channels[1].PeakDbfs, dbfsFloor, 0.001, "right peak (silent)")
	approx(t, d.Channels[1].RmsDbfs, dbfsFloor, 0.001, "right rms (silent)")
	if d.Channels[0].Clipped || d.Channels[1].Clipped {
		t.Error("half-scale and silence must not clip")
	}
}

// TestMeterPartialFrame checks that a period whose sample count is not a clean
// multiple of the channel count is handled without a panic: the trailing partial
// frame is dropped, and a period shorter than one whole frame is a no-op. An
// off-by-one that rounded frames up instead of down would index past the buffer
// and panic on the capture hot path; this is the test that would catch it.
func TestMeterPartialFrame(t *testing.T) {
	m := meterN(2)
	// 5 samples on a stereo meter: two whole L,R frames plus a dangling 5th
	// sample. frames = 5/2 = 2; the remainder must be dropped, not read.
	m.Observe(pcm(16384, 16384, 16384, 16384, 16384))
	d := m.sample("x")
	if len(d.Channels) != 2 {
		t.Fatalf("channels = %d, want 2", len(d.Channels))
	}
	const halfDb = -6.0206
	approx(t, d.Channels[0].RmsDbfs, halfDb, 0.01, "left rms over 2 frames")
	approx(t, d.Channels[1].RmsDbfs, halfDb, 0.01, "right rms over 2 frames")

	// A period shorter than one full frame, and an empty period, are no-ops
	// (frames == 0), not panics.
	m2 := meterN(2)
	m2.Observe(pcm(16384)) // 1 sample across 2 channels -> frames 0
	m2.Observe(nil)        // empty period
	d2 := m2.sample("x")
	approx(t, d2.Channels[0].RmsDbfs, dbfsFloor, 0.001, "left rms, no whole frame")
	approx(t, d2.Channels[1].RmsDbfs, dbfsFloor, 0.001, "right rms, no whole frame")
}

// TestMeterDefaultsToMono checks that a non-positive channel count is clamped to
// a single mono lane, so a caller that registers a meter before negotiating the
// hardware channel count still gets a usable, non-empty meter (sample() must
// never return an empty Channels slice that downstream Channels[0] access would
// panic on).
func TestMeterDefaultsToMono(t *testing.T) {
	h := NewHub()
	h.subs.Store(1)
	for _, ch := range []int{0, -3} {
		m := h.Meter("x", ch)
		if len(m.ch) != 1 {
			t.Errorf("Meter(x, %d): channels = %d, want 1", ch, len(m.ch))
		}
		m.Observe(pcm(repeat(16384, 100)...))
		d := m.sample("x")
		if len(d.Channels) != 1 {
			t.Fatalf("Meter(x, %d): sample channels = %d, want 1", ch, len(d.Channels))
		}
		approx(t, d.Channels[0].RmsDbfs, -6.0206, 0.01, "mono rms")
	}
}

// TestLevelsEventMultiChannelContract drives a 2-channel meter through the SSE
// marshal and asserts the per-channel array survives the wire contract with more
// than one element and in order. The single-channel contract test cannot catch a
// dropped, reordered, or empty channels array on the multi-channel path.
func TestLevelsEventMultiChannelContract(t *testing.T) {
	h := NewHub()
	m := h.Meter(nameGarden, 2)
	h.subs.Store(1)
	// Interleaved left half-scale, right silent.
	samples := make([]int16, 0, 200)
	for i := 0; i < 100; i++ {
		samples = append(samples, 16384, 0)
	}
	m.Observe(pcm(samples...))
	ev := h.levelsEvent()

	var contract mgmtapi.LevelsEvent
	if err := json.Unmarshal(ev.Data, &contract); err != nil {
		t.Fatalf("levels payload does not match contract: %v", err)
	}
	if len(contract.Devices) != 1 {
		t.Fatalf("devices = %d, want 1", len(contract.Devices))
	}
	chans := contract.Devices[0].Channels
	if len(chans) != 2 {
		t.Fatalf("channels = %d, want 2", len(chans))
	}
	if chans[0].Channel != 0 || chans[1].Channel != 1 {
		t.Errorf("channel indices = %d,%d, want 0,1", chans[0].Channel, chans[1].Channel)
	}
	approx(t, chans[0].RmsDbfs, -6.0206, 0.01, "left rms via contract")
	approx(t, chans[1].RmsDbfs, dbfsFloor, 0.001, "right rms via contract (silent)")
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
