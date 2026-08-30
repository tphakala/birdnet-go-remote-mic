package rtspserver

import "testing"

func TestTrackClientConnected(t *testing.T) {
	tr := &Track{Path: "/stream"}
	if tr.ClientConnected() {
		t.Fatal("a fresh track should report no client")
	}
	if !tr.acquireSlot() {
		t.Fatal("acquireSlot on a free track should succeed")
	}
	if !tr.ClientConnected() {
		t.Error("track with an acquired slot should report a client")
	}
	tr.releaseSlot()
	if tr.ClientConnected() {
		t.Error("track should report no client after release")
	}
}
