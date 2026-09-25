//go:build linux

package main

import (
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
	"github.com/tphakala/birdnet-go-remote-mic/internal/rtspserver"
)

// TestFanoutStreamsWiresFeedAndDrops pins the production fan-out wiring: each
// stream's consumer counts drops into that stream's counter and follows that
// stream's own feed, so an idle stream is gated and a playing one is not.
func TestFanoutStreamsWiresFeedAndDrops(t *testing.T) {
	t.Parallel()
	streams := []*streamRuntime{
		{frames: rtspserver.NewChanSource(1)},
		{frames: rtspserver.NewChanSource(1)},
	}
	out := fanoutStreams(streams)
	if len(out) != len(streams) {
		t.Fatalf("got %d fan-out streams, want %d", len(out), len(streams))
	}
	for i := range out {
		if got, want := out[i].Dropped, &streams[i].dropped; got != want {
			t.Errorf("stream %d: got Dropped %p, want the stream's own counter %p", i, got, want)
		}
		if out[i].Gate == nil {
			t.Fatalf("stream %d: got a nil Gate, want the stream feed's", i)
		}
		if got, _ := out[i].Gate(); got {
			t.Errorf("stream %d: got an active Gate with no client playing, want inactive", i)
		}
	}
	streams[1].frames.SetActive(true)
	on0, _ := out[0].Gate()
	on1, s1 := out[1].Gate()
	if _, want := streams[1].frames.Session(); on0 || !on1 || s1 != want {
		t.Errorf("after stream 1 plays: got Gate() %v, (%v, %d), want false, (true, %d)", on0, on1, s1, want)
	}
}

// TestBuildStagePayloadType locks the RTP payload type buildStage assigns to the
// track to the same pipeline.PayloadType the SDP is built from, so the writer
// and the DESCRIBE response cannot drift apart per mode.
func TestBuildStagePayloadType(t *testing.T) {
	for _, mode := range []config.Mode{config.ModePCM, config.ModeOpus} {
		_, payloadType := buildStage(&config.Stream{Mode: mode, Opus: config.Opus{Bitrate: 64000}})
		if want := pipeline.PayloadType(mode); payloadType != want {
			t.Errorf("buildStage(%q) payloadType = %d, want %d", mode, payloadType, want)
		}
	}
}
