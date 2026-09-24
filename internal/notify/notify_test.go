package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/sse"
)

const (
	testDeviceKey    = "device:garden:down"
	testDownTitle    = "down"
	testDeviceSource = "garden"
)

// fixedClock returns a clock function pinned at t for deterministic stamps.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestCenterAssignsMonotonicGaplessIDs(t *testing.T) {
	c := NewCenter()
	for i := 0; i < 3; i++ {
		c.Publish(Notification{Category: CategorySystem, Kind: KindEvent, Title: "x"})
	}
	snap := c.Snapshot()
	if got := ids(snap.Notifications); !equalIDs(got, []uint64{1, 2, 3}) {
		t.Fatalf("ids = %v, want [1 2 3]", got)
	}
	if snap.NextID != 4 {
		t.Errorf("nextID = %d, want 4", snap.NextID)
	}
}

func TestPublishStampsBootIDAndTime(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	c := NewCenter(WithClock(fixedClock(now)))
	c.Publish(Notification{Category: CategorySystem, Kind: KindEvent, Title: "x"})
	snap := c.Snapshot()
	if len(snap.Notifications) != 1 {
		t.Fatalf("len = %d, want 1", len(snap.Notifications))
	}
	n := snap.Notifications[0]
	if n.BootID != c.BootID() || n.BootID == "" {
		t.Errorf("bootID = %q, want center bootID %q", n.BootID, c.BootID())
	}
	if !n.Time.Equal(now) {
		t.Errorf("time = %v, want %v", n.Time, now)
	}
	if snap.BootID != c.BootID() || !snap.ServerTime.Equal(now) {
		t.Errorf("snapshot envelope bootID/serverTime wrong: %q %v", snap.BootID, snap.ServerTime)
	}
}

// TestPublishStampsUptime checks that every entry carries the Center's age at its
// own publish, and that the snapshot's uptime and serverTime come from one read.
func TestPublishStampsUptime(t *testing.T) {
	t.Parallel()
	base := time.Unix(1_700_000_000, 0).UTC()
	now := base
	c := NewCenter(WithClock(func() time.Time { return now }))

	now = base.Add(1500 * time.Millisecond)
	c.Publish(Notification{Category: CategorySystem, Kind: KindEvent, Title: "a"})
	now = base.Add(90 * time.Second)
	c.Onset(Notification{Category: CategoryDevice, Key: testDeviceKey, Title: testDownTitle})
	now = base.Add(2 * time.Minute)
	snap := c.Snapshot()

	want := []int64{1500, 90_000}
	if len(snap.Notifications) != len(want) {
		t.Fatalf("got %d entries, want %d", len(snap.Notifications), len(want))
	}
	for i, n := range snap.Notifications {
		if n.UptimeMs != want[i] {
			t.Errorf("entry %d uptimeMs = %d, want %d", n.ID, n.UptimeMs, want[i])
		}
	}
	if snap.UptimeMs != 120_000 {
		t.Errorf("snapshot uptimeMs = %d, want 120000", snap.UptimeMs)
	}
	if !snap.ServerTime.Equal(now) {
		t.Errorf("snapshot serverTime = %v, want %v (same read as uptime)", snap.ServerTime, now)
	}
}

// TestDefaultStartIsMonotonic guards the property that makes uptime immune to a
// wall-clock step: with the default clock the start reading carries a monotonic
// component, and time.Time.Sub then ignores the wall clock entirely. A wall-only
// reading (Round(0) strips the monotonic part) would print without the "m=".
func TestDefaultStartIsMonotonic(t *testing.T) {
	t.Parallel()
	c := NewCenter()
	if !strings.Contains(c.start.String(), " m=") {
		t.Fatalf("start %q has no monotonic reading", c.start.String())
	}
}

func TestOnsetIdempotentAndActive(t *testing.T) {
	c := NewCenter()
	on := Notification{Severity: SeverityError, Category: CategoryDevice, Key: testDeviceKey, Title: "Device failed"}
	if !c.Onset(on) {
		t.Fatal("first Onset returned false, want true")
	}
	if c.Onset(on) {
		t.Fatal("second Onset for the same key returned true, want false (idempotent)")
	}
	active := c.Active()
	if len(active) != 1 || active[0].Key != testDeviceKey {
		t.Fatalf("Active() = %+v, want one entry keyed %q", active, testDeviceKey)
	}
	if active[0].Kind != KindOnset {
		t.Errorf("active kind = %q, want onset", active[0].Kind)
	}
	// Only one entry reached the ring despite two Onset calls.
	if got := len(c.Snapshot().Notifications); got != 1 {
		t.Errorf("ring entries = %d, want 1", got)
	}
}

func TestActiveSortedByID(t *testing.T) {
	c := NewCenter()
	// Three conditions so the ascending assertion has real power: map iteration
	// is randomized, so with three entries an unsorted Active() reliably fails.
	c.Onset(Notification{Severity: SeverityError, Category: CategoryDevice, Key: "device:a:down", Title: "a"})   // id 1
	c.Onset(Notification{Severity: SeverityWarning, Category: CategoryAudio, Key: "audio:b:quiet", Title: "b"})  // id 2
	c.Onset(Notification{Severity: SeverityWarning, Category: CategoryStream, Key: "stream:c:flap", Title: "c"}) // id 3
	active := c.Active()
	if got := ids(active); !equalIDs(got, []uint64{1, 2, 3}) {
		t.Fatalf("Active() ids = %v, want [1 2 3] (ascending)", got)
	}
}

func TestOnsetEmptyKeyRejected(t *testing.T) {
	c := NewCenter()
	if c.Onset(Notification{Severity: SeverityError, Category: CategoryDevice, Title: "no key"}) {
		t.Fatal("Onset with an empty key returned true, want false")
	}
	if len(c.Active()) != 0 {
		t.Error("empty-key Onset registered an active condition")
	}
	if len(c.Snapshot().Notifications) != 0 {
		t.Error("empty-key Onset published an entry")
	}
}

func TestClearTakesOnsetIdentity(t *testing.T) {
	c := NewCenter()
	c.Onset(Notification{Severity: SeverityError, Category: CategoryDevice, Key: testDeviceKey, Source: testDeviceSource, Title: testDownTitle})
	// A clear's category and source always come from the onset (they are the
	// condition's identity, fixed by its key), overriding whatever the caller
	// passes, so an onset and its clear can never disagree for the same key.
	if !c.Clear(testDeviceKey, Notification{Severity: SeverityInfo, Category: CategorySystem, Source: "wrong", Title: "recovered"}) {
		t.Fatal("Clear returned false, want true")
	}
	snap := c.Snapshot()
	last := snap.Notifications[len(snap.Notifications)-1]
	if last.Category != CategoryDevice {
		t.Errorf("clear category = %q, want onset's %q", last.Category, CategoryDevice)
	}
	if last.Source != testDeviceSource {
		t.Errorf("clear source = %q, want onset's %q", last.Source, testDeviceSource)
	}
}

func TestClearIdempotent(t *testing.T) {
	c := NewCenter()
	if c.Clear(testDeviceKey, Notification{}) {
		t.Fatal("Clear of an inactive key returned true, want false")
	}
	c.Onset(Notification{Category: CategoryDevice, Key: testDeviceKey, Title: testDownTitle})
	if !c.Clear(testDeviceKey, Notification{Severity: SeverityInfo, Category: CategoryDevice, Title: "recovered"}) {
		t.Fatal("Clear of an active key returned false, want true")
	}
	if len(c.Active()) != 0 {
		t.Error("Active() not empty after Clear")
	}
	if c.Clear(testDeviceKey, Notification{}) {
		t.Fatal("second Clear returned true, want false")
	}
	// The clear entry carries Kind clear and the key.
	snap := c.Snapshot()
	last := snap.Notifications[len(snap.Notifications)-1]
	if last.Kind != KindClear || last.Key != testDeviceKey {
		t.Errorf("clear entry = %+v, want kind clear keyed %q", last, testDeviceKey)
	}
}

func TestResolve(t *testing.T) {
	c := NewCenter()
	if c.Resolve(testDeviceKey, "gone") {
		t.Fatal("Resolve of an inactive key returned true, want false")
	}
	c.Onset(Notification{Severity: SeverityError, Category: CategoryDevice, Key: testDeviceKey, Source: testDeviceSource, Title: testDownTitle})
	if !c.Resolve(testDeviceKey, "device removed or disabled") {
		t.Fatal("Resolve of an active key returned false, want true")
	}
	if len(c.Active()) != 0 {
		t.Error("Active() not empty after Resolve")
	}
	snap := c.Snapshot()
	last := snap.Notifications[len(snap.Notifications)-1]
	if last.Kind != KindClear || last.Key != testDeviceKey {
		t.Errorf("resolve entry kind/key = %q/%q, want clear/%q", last.Kind, last.Key, testDeviceKey)
	}
	if last.Severity != SeverityInfo || last.Message != "device removed or disabled" {
		t.Errorf("resolve entry severity/message = %q/%q", last.Severity, last.Message)
	}
	// The title reads neutrally ("Cleared"), not "Resolved": a vanished subject
	// was removed, not recovered.
	if last.Title != "Cleared" {
		t.Errorf("resolve title = %q, want Cleared", last.Title)
	}
	// It reuses the onset's category and source.
	if last.Category != CategoryDevice || last.Source != testDeviceSource {
		t.Errorf("resolve entry category/source = %q/%q, want device/garden", last.Category, last.Source)
	}
}

func TestSnapshotPinsTrimmedActiveOnset(t *testing.T) {
	c := NewCenter(WithCapacity(2))
	c.Onset(Notification{Severity: SeverityError, Category: CategoryDevice, Key: testDeviceKey, Title: testDownTitle}) // id 1
	c.Publish(Notification{Category: CategorySystem, Kind: KindEvent, Title: "a"})                                     // id 2
	c.Publish(Notification{Category: CategorySystem, Kind: KindEvent, Title: "b"})                                     // id 3, trims id 1 from the ring
	snap := c.Snapshot()
	// The active onset (id 1) is pinned back in even though the ring dropped it.
	if got := ids(snap.Notifications); !equalIDs(got, []uint64{1, 2, 3}) {
		t.Fatalf("ids = %v, want [1 2 3] (trimmed active onset re-pinned)", got)
	}
	if snap.NextID != 4 {
		t.Errorf("nextID = %d, want 4", snap.NextID)
	}
}

func TestSnapshotDoesNotDuplicateActiveStillInRing(t *testing.T) {
	c := NewCenter()
	c.Onset(Notification{Severity: SeverityError, Category: CategoryDevice, Key: testDeviceKey, Title: testDownTitle})
	snap := c.Snapshot()
	if got := ids(snap.Notifications); !equalIDs(got, []uint64{1}) {
		t.Fatalf("ids = %v, want [1] (active onset not duplicated)", got)
	}
}

func TestDefaultCapacityKeepsNewest(t *testing.T) {
	// The OpenAPI /notifications description quotes the default ring depth, so a
	// change here must move it too. The web UI reads it from Snapshot.Capacity.
	if defaultCapacity != 500 {
		t.Fatalf("defaultCapacity = %d, want 500; update the /notifications description in api/openapi.yaml with it", defaultCapacity)
	}
	c := NewCenter()
	for range defaultCapacity + 1 {
		c.Publish(Notification{Category: CategorySystem, Kind: KindEvent, Title: "x"})
	}
	snap := c.Snapshot()
	if snap.Capacity != defaultCapacity {
		t.Errorf("snapshot capacity = %d, want %d", snap.Capacity, defaultCapacity)
	}
	if got := len(snap.Notifications); got != defaultCapacity {
		t.Fatalf("ring entries = %d, want %d (bounded at default capacity)", got, defaultCapacity)
	}
	// Ids 1..defaultCapacity+1 were published; the ring keeps the newest, so the
	// oldest (id 1) is trimmed and the window is [2, defaultCapacity+1].
	first := snap.Notifications[0].ID
	last := snap.Notifications[len(snap.Notifications)-1].ID
	if first != 2 {
		t.Errorf("first id = %d, want 2 (oldest trimmed)", first)
	}
	if last != uint64(defaultCapacity+1) {
		t.Errorf("last id = %d, want %d (newest retained)", last, defaultCapacity+1)
	}
}

func TestSubscribeReceivesPublishedInOrder(t *testing.T) {
	c := NewCenter()
	ch, cancel := c.Subscribe()
	defer cancel()
	c.Publish(Notification{Category: CategorySystem, Kind: KindEvent, Title: "one"})
	c.Publish(Notification{Category: CategorySystem, Kind: KindEvent, Title: "two"})
	for wantID := uint64(1); wantID <= 2; wantID++ {
		select {
		case ev := <-ch:
			if ev.Name != notificationEvent {
				t.Fatalf("event name = %q, want %q", ev.Name, notificationEvent)
			}
			var n Notification
			if err := json.Unmarshal(ev.Data, &n); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if n.ID != wantID {
				t.Fatalf("event id = %d, want %d", n.ID, wantID)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event id %d", wantID)
		}
	}
}

func TestBroadcastDropsOnFullWithoutBlocking(t *testing.T) {
	c := NewCenter()
	ch, cancel := c.Subscribe()
	defer cancel()
	// Publish more than the subscriber buffer without draining. Publish must not
	// block, and the channel fills to exactly its capacity with the rest dropped.
	for i := 0; i < subscriberBuffer+10; i++ {
		c.Publish(Notification{Category: CategorySystem, Kind: KindEvent, Title: "x"})
	}
	if got := len(ch); got != subscriberBuffer {
		t.Fatalf("buffered = %d, want %d (excess dropped)", got, subscriberBuffer)
	}
}

func TestPublishSkipsBroadcastOnMarshalFailure(t *testing.T) {
	// A time whose year is outside [0,9999] is the only input that makes
	// json.Marshal fail. A real clock never produces it, but the stream must
	// degrade gracefully: the ring still records the entry (the snapshot is the
	// source of truth) and no broken event reaches subscribers.
	badClock := func() time.Time { return time.Date(10001, 1, 1, 0, 0, 0, 0, time.UTC) }
	c := NewCenter(WithClock(badClock))
	ch, cancel := c.Subscribe()
	defer cancel()
	c.Publish(Notification{Category: CategorySystem, Kind: KindEvent, Title: "x"})
	// The entry is in the ring despite the marshal failure.
	if got := len(c.Snapshot().Notifications); got != 1 {
		t.Errorf("ring entries = %d, want 1 (entry recorded despite marshal failure)", got)
	}
	// No broken event was broadcast to the subscriber.
	select {
	case ev := <-ch:
		t.Errorf("a broken event was broadcast: %+v", ev)
	default:
	}
}

func TestCancelUnsubscribesIdempotently(t *testing.T) {
	c := NewCenter()
	ch, cancel := c.Subscribe()
	cancel()
	cancel() // second call must not panic
	c.Publish(Notification{Category: CategorySystem, Kind: KindEvent, Title: "x"})
	select {
	case <-ch:
		t.Fatal("received an event after cancel; subscriber not removed")
	default:
	}
}

func TestNilCenterIsNoOp(t *testing.T) {
	var c *Center
	// None of these must panic.
	c.Publish(Notification{})
	if c.Onset(Notification{Key: "k"}) {
		t.Error("nil Onset returned true")
	}
	if c.Clear("k", Notification{}) {
		t.Error("nil Clear returned true")
	}
	if c.Resolve("k", "r") {
		t.Error("nil Resolve returned true")
	}
	if c.Active() != nil {
		t.Error("nil Active() not nil")
	}
	if c.BootID() != "" {
		t.Error("nil BootID() not empty")
	}
	ch, cancelSub := c.Subscribe()
	if ch != nil {
		t.Error("nil Subscribe() channel not nil")
	}
	cancelSub() // must not panic
	if snap := c.Snapshot(); snap.BootID != "" || snap.NextID != 0 || snap.Notifications != nil {
		t.Errorf("nil Snapshot() = %+v, want zero value", snap)
	}
	// A nil *Center still satisfies Publisher (typed nil in the interface).
	var _ Publisher = c
}

func TestBootIDStableHexAndDistinct(t *testing.T) {
	c := NewCenter()
	id := c.BootID()
	if len(id) != bootIDBytes*2 {
		t.Fatalf("bootID %q length = %d, want %d hex chars", id, len(id), bootIDBytes*2)
	}
	if c.BootID() != id {
		t.Error("bootID changed between calls")
	}
	if NewCenter().BootID() == id {
		t.Error("two centers share a bootID")
	}
}

func TestNewBootIDFallbackOnRandFailure(t *testing.T) {
	orig := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	defer func() { randRead = orig }()
	c := NewCenter()
	id := c.BootID()
	if len(id) != bootIDBytes*2 {
		t.Fatalf("fallback bootID %q length = %d, want %d", id, len(id), bootIDBytes*2)
	}
	// The fallback derives from the clock, so it must be a real non-zero value.
	// Asserting length alone would still pass if the fallback body were deleted
	// (an all-zero [8]byte also hex-encodes to 16 chars).
	if id == strings.Repeat("0", bootIDBytes*2) {
		t.Fatal("fallback bootID is all zeros; the fallback body did not run")
	}
}

func TestStartedHelper(t *testing.T) {
	n := Started("v1.2.3")
	if n.Category != CategorySystem || n.Kind != KindEvent || n.Severity != SeverityInfo {
		t.Errorf("Started envelope wrong: %+v", n)
	}
	if !strings.Contains(n.Message, "v1.2.3") {
		t.Errorf("message %q does not name the version", n.Message)
	}
	// Publishing stamps id, boot id and time.
	c := NewCenter()
	c.Publish(n)
	got := c.Snapshot().Notifications[0]
	if got.ID != 1 || got.BootID == "" {
		t.Errorf("published Started not stamped: %+v", got)
	}
}

// --- SSE integration: a Center is an sse.Source and multiplexes with others ---

// staticLevels is a minimal sse.Source that pre-queues one levels event, to
// prove a published notification and a levels event share one multiplexed stream.
type staticLevels struct{}

func (staticLevels) Subscribe() (events <-chan sse.Event, cancel func()) {
	ch := make(chan sse.Event, 1)
	ch <- sse.Event{Name: "levels", Data: []byte(`{"devices":[]}`)}
	return ch, func() {}
}

func TestHandlerMultiplexesNotificationsAndLevels(t *testing.T) {
	c := NewCenter()
	srv := httptest.NewServer(sse.Handler(staticLevels{}, c))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
	}()

	// The stream is best effort: a notification published before the handler
	// subscribes is dropped by design (the snapshot is the source of truth). Wait
	// for the subscription so this test asserts multiplexing, not timing.
	waitForSubscriber(t, c)
	c.Publish(Notification{Category: CategorySystem, Kind: KindEvent, Title: "started", Message: "up"})

	seen := map[string]bool{}
	rd := bufio.NewReader(resp.Body)
	for !seen["levels"] || !seen[notificationEvent] {
		name, _, err := readSSEEvent(rd)
		if err != nil {
			t.Fatalf("stream ended before both events seen (%v); saw %v", err, seen)
		}
		if name == "heartbeat" {
			continue
		}
		seen[name] = true
	}
}

// waitForSubscriber blocks until the SSE handler has registered a subscriber on
// the center, so a following Publish is guaranteed to reach the live stream.
func waitForSubscriber(t *testing.T, c *Center) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.bc.Len() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("SSE handler never subscribed to the center")
}

// readSSEEvent reads one complete SSE event (up to the blank separator line) and
// returns its event name and data payload.
func readSSEEvent(r *bufio.Reader) (name, data string, err error) {
	for {
		line, rerr := r.ReadString('\n')
		if rerr != nil {
			return "", "", rerr
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if name != "" {
				return name, data, nil
			}
		}
	}
}
