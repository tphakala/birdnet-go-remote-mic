//go:build linux

package main

import (
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
)

// TestBuildStagePayloadType locks the RTP payload type buildStage assigns to the
// track to the same pipeline.PayloadType the SDP is built from, so the writer
// and the DESCRIBE response cannot drift apart per mode.
func TestBuildStagePayloadType(t *testing.T) {
	for _, mode := range []config.Mode{config.ModePCM, config.ModeOpus} {
		_, payloadType := buildStage(&config.Device{Mode: mode, Opus: config.Opus{Bitrate: 64000}}, 1)
		if want := pipeline.PayloadType(mode); payloadType != want {
			t.Errorf("buildStage(%q) payloadType = %d, want %d", mode, payloadType, want)
		}
	}
}
