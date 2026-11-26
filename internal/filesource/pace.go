package filesource

import (
	"sync/atomic"
	"time"
)

// ringPeriods is how many completed periods may wait for a late reader before
// the source re-anchors, mirroring go-audio-capture's default of 4 periods.
const ringPeriods = 4

// framesDuration is the duration of frames at rate. It splits whole seconds
// from the remainder: frames*time.Second overflows int64 after about 6.7 h of
// frames at 384 kHz.
func framesDuration(frames uint64, rate int) time.Duration {
	r := uint64(rate)
	return time.Duration(frames/r)*time.Second + time.Duration((frames%r)*uint64(time.Second)/r)
}

// pacer holds a Read to real time. The end of the next period is the anchor
// plus the duration of every frame served since, so per-period rounding never
// drifts. Only the reader goroutine calls wait; overruns is read by others.
type pacer struct {
	rate     int
	ring     time.Duration
	anchor   time.Time // zero until the first wait, so open-to-first-read is not lag
	frames   uint64    // frames served since anchor
	timer    *time.Timer
	overruns atomic.Uint64
}

func newPacer(rate, periodFrames int) *pacer {
	t := time.NewTimer(time.Hour)
	t.Stop()
	return &pacer{rate: rate, ring: ringPeriods * framesDuration(uint64(periodFrames), rate), timer: t}
}

// wait blocks until the period of n frames has ended on the source's timeline
// and returns the instant to stamp it with: its end, or for a catch-up period
// the same end expressed on the current wall clock. A reader that left 4 or
// more periods waiting re-anchors instead of bursting, and the overrun is
// counted; the next period then waits one full period. It returns ErrClosed
// when done closes first.
func (p *pacer) wait(n int, done <-chan struct{}) (time.Time, error) {
	now := time.Now()
	if p.anchor.IsZero() {
		p.anchor = now
	}
	end := p.anchor.Add(framesDuration(p.frames+uint64(n), p.rate))
	if now.Sub(end) >= p.ring {
		p.overruns.Add(1)
		p.anchor, p.frames = now, 0
		end = p.anchor.Add(framesDuration(uint64(n), p.rate))
	}
	if d := end.Sub(now); d > 0 {
		p.timer.Reset(d)
		select {
		case <-p.timer.C:
		case <-done:
			p.timer.Stop()
			return time.Time{}, ErrClosed
		}
	}
	now = time.Now()
	late := max(0, now.Sub(end))
	p.frames += uint64(n)
	return now.Add(-late), nil
}
