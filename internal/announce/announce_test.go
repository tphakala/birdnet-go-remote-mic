package announce

import (
	"context"
	"testing"
	"time"
)

const testCodec = "L16"

func TestTXTRecords(t *testing.T) {
	txt := txtRecords(&Info{Name: "garden-mic", Path: "/garden", Codec: testCodec, Rate: 256000, Channels: 1, Version: "1.2.3"})
	want := map[string]string{
		"txtvers": "1",
		"model":   "birdnet-go-remote-mic",
		"version": "1.2.3",
		"codec":   "L16",
		"rate":    "256000",
		"ch":      "1",
		"path":    "/garden",
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

func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []Info{{Name: "test-mic-cancel", Path: "/stream", Port: 18999, Codec: testCodec, Rate: 48000, Channels: 1, Version: "test"}})
	}()
	time.Sleep(300 * time.Millisecond) // let the responder start
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
