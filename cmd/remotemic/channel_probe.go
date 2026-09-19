//go:build linux

package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/audio"
	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
	"github.com/tphakala/birdnet-go-remote-mic/internal/levels"
)

// The channel probe discards probeSettle of audio before measuring, because a
// capture that has just started can carry an interface's power-on click or DC
// settling, which would make an idle channel look loud. It then measures for
// probeMeasure.
const (
	probeSettle  = 150 * time.Millisecond
	probeMeasure = 700 * time.Millisecond
)

// openProbeCapture opens the capture the channel probe reads. It is a package
// var so the probe's timing and error handling are testable without hardware.
var openProbeCapture = func(device string, rate, channels int) (audio.Source, error) {
	return audio.OpenCaptureAt(&config.Device{Device: device, Rate: rate, Format: "s16"}, channels)
}

// probeOpenSlot admits one probe device open at a time. The capture open cannot
// be cancelled (the capture library takes no context), so an open that never
// returns strands the goroutine running it. Holding the slot for the duration
// of the open bounds that to a single goroutine: while an earlier open is still
// stuck, later probes fail fast instead of stranding one goroutine each.
var probeOpenSlot = make(chan struct{}, 1)

// probeChannelLevels is the provisioning channel probe (mgmtserver.ChannelProbe):
// it opens an unconfigured device at rate and channels, lets the capture settle,
// and returns each channel's RMS level in dBFS over the measurement window. A
// device another process already holds exclusively is rejected without opening.
// The open and the read run on a goroutine, so the probe itself returns as soon
// as ctx is done. A stalled read is then ended by closing the source, and a
// source that opens only after the cancel is closed by the goroutine. An open
// that blocks and never returns cannot be interrupted: its goroutine stays parked
// in the open, and probeOpenSlot keeps that to one at a time. The accumulator is
// read only on the success path, after the goroutine has finished with it, so the
// main path never races it.
func probeChannelLevels(ctx context.Context, device string, rate, channels int) ([]float64, error) {
	// A device another process holds exclusively is skipped with a fast,
	// non-blocking check, so a busy device never reaches the blocking open below.
	if deviceInUse(device, channels) {
		return nil, errors.New("channel probe: device busy")
	}
	select {
	case probeOpenSlot <- struct{}{}:
	default:
		return nil, errors.New("channel probe: an earlier probe is still opening a device")
	}

	// holder shares the opened source across the two goroutines under a mutex.
	// closeShared marks the holder done and closes the source if it is open;
	// closing ends a read blocked on the goroutine, the same way the live pipeline
	// stops a capture pump (fanout.Close).
	var (
		mu   sync.Mutex
		src  audio.Source
		done bool
	)
	closeShared := func() {
		mu.Lock()
		done = true
		s := src
		src = nil
		mu.Unlock()
		if s != nil {
			_ = s.Close()
		}
	}

	type result struct {
		levels []float64
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		s, err := openProbeCapture(device, rate, channels)
		<-probeOpenSlot
		if err != nil {
			resultCh <- result{err: err}
			return
		}
		mu.Lock()
		if done {
			// Cancelled while opening: nobody else holds this source, so close it
			// here. The main path already returned on ctx.Done.
			mu.Unlock()
			_ = s.Close()
			return
		}
		src = s
		mu.Unlock()

		gotRate, nch := s.Negotiated()
		settle := int(int64(gotRate) * int64(probeSettle) / int64(time.Second))
		measure := int(int64(gotRate) * int64(probeMeasure) / int64(time.Second))
		if measure <= 0 {
			resultCh <- result{err: errors.New("channel probe: capture reported no sample rate")}
			return
		}
		acc := levels.NewAccumulator(nch)
		seen := 0
		for seen < settle+measure {
			p, rerr := s.Read()
			if rerr != nil {
				resultCh <- result{err: rerr}
				return
			}
			// Count the period against the settle window first; only frames
			// past it are measured.
			start := max(0, settle-seen)
			if start < p.Frames {
				acc.Add(p.Buf[start*nch*2 : p.Frames*nch*2])
			}
			seen += p.Frames
		}
		resultCh <- result{levels: acc.RMSDbfs()}
	}()

	select {
	case r := <-resultCh:
		closeShared()
		return r.levels, r.err
	case <-ctx.Done():
		// Stop waiting: closing an open source ends a stalled read, and a source
		// opened after this closes itself in the goroutine above.
		closeShared()
		return nil, ctx.Err()
	}
}
