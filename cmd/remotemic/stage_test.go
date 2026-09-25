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
		if out[i].Active == nil {
			t.Fatalf("stream %d: got a nil Active, want the stream feed's", i)
		}
		if got := out[i].Active(); got {
			t.Errorf("stream %d: got Active() %v with no client playing, want false", i, got)
		}
	}
	streams[1].frames.SetActive(true)
	if got0, got1 := out[0].Active(), out[1].Active(); got0 || !got1 {
		t.Errorf("after stream 1 plays: got Active() %v, %v, want false, true", got0, got1)
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
