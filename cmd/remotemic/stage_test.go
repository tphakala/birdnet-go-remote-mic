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
		if out[i].Dropped != &streams[i].dropped {
			t.Errorf("stream %d: Dropped is not the stream's own counter", i)
		}
		if out[i].Active == nil || out[i].Active() {
			t.Errorf("stream %d: Active is nil or reports true with no client playing", i)
		}
	}
	streams[1].frames.SetActive(true)
	if out[0].Active() || !out[1].Active() {
		t.Errorf("after stream 1 plays: Active = %v, %v, want false, true", out[0].Active(), out[1].Active())
	}
}

// TestBuildStagePayloadType locks the RTP payload type buildStage assigns to the
// track to the same pipeline.PayloadType the SDP is built from, so the writer
// and the DESCRIBE response cannot drift apart per mode.
func TestBuildStagePayloadType(t *testing.T) {
	for _, mode := range []config.Mode{config.ModePCM, config.ModeOpus} {
		_, payloadType := buildStage(&config.Stream{Mode: mode, Opus: config.Opus{Bitrate: 64000}}, 1)
		if want := pipeline.PayloadType(mode); payloadType != want {
			t.Errorf("buildStage(%q) payloadType = %d, want %d", mode, payloadType, want)
		}
	}
}
