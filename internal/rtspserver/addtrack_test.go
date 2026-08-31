package rtspserver

import (
	"testing"
	"time"
)

func TestAddTrackRegistersPath(t *testing.T) {
	srv := New(Config{})
	if got := srv.lookup("/x"); got != nil {
		t.Fatalf("lookup(/x) before AddTrack = %v, want nil", got)
	}
	tr := &Track{Path: "/x", PayloadType: 96}
	srv.AddTrack(tr)
	if got := srv.lookup("/x"); got != tr {
		t.Fatalf("lookup(/x) after AddTrack = %v, want the added track", got)
	}
}

// TestAddTrackReplacesExistingPath covers the hot-reload restart case: a device
// whose parameters changed is removed and re-added on the same path, and the new
// track must take over.
func TestAddTrackReplacesExistingPath(t *testing.T) {
	srv := New(Config{})
	first := &Track{Path: "/x", PayloadType: 96}
	second := &Track{Path: "/x", PayloadType: 97}
	srv.AddTrack(first)
	srv.AddTrack(second)
	if got := srv.lookup("/x"); got != second {
		t.Fatalf("lookup(/x) after replacing AddTrack = %v, want the second track", got)
	}
}

// TestAddTrackServesAfterStart adds a track to an already-serving server and
// confirms a client can DESCRIBE the new path (404 before, 200 after), proving
// the listener need not be rebuilt to serve a newly hot-reloaded device.
func TestAddTrackServesAfterStart(t *testing.T) {
	addr, srv := startServer(t, Config{Timeout: 60 * time.Second}, defaultTrack())

	c := dial(t, addr)
	if r := c.do(t, "DESCRIBE", "rtsp://"+addr+"/added", nil); r.StatusCode != 404 {
		t.Fatalf("DESCRIBE /added before AddTrack = %d, want 404", r.StatusCode)
	}

	srv.AddTrack(&Track{Path: "/added", SDP: testSDP, PayloadType: 96})

	c2 := dial(t, addr)
	if r := c2.do(t, "DESCRIBE", "rtsp://"+addr+"/added", nil); r.StatusCode != 200 {
		t.Errorf("DESCRIBE /added after AddTrack = %d, want 200", r.StatusCode)
	}
}
