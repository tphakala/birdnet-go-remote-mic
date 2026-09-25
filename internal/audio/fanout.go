package audio

import (
	"errors"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// fanoutBuffer is the per-consumer period queue depth. A consumer whose pipeline
// falls behind fills its queue and then drops whole periods rather than stalling
// the shared reader; a small depth keeps latency low while smoothing a brief
// scheduling hiccup.
const fanoutBuffer = 8

// Fanout reads periods from one upstream Source on a single goroutine and
// distributes each to N downstream consumers, so one exclusive capture can feed
// several independent stream pipelines. Because the upstream buffer is reused on
// every Read (Source is single-consumer), each period is copied once into fresh
// storage before it is shared with the consumers; the copy is never mutated, so
// every consumer may reference it safely and simultaneously. A consumer whose
// pipeline falls behind drops whole periods (its bounded queue fills) rather than
// stalling the shared reader, which must keep draining the capture to avoid an
// ALSA overrun that would disrupt every stream at once. Run does the reading;
// distributing N encodes onto N goroutines (the consumers' pipelines) keeps the
// per-period work off the capture read loop, so a slow encoder cannot blow the
// capture period budget.
//
// A consumer whose stream has no client playing is sent nothing, and when no
// consumer has one the copy is skipped altogether, so an unattended appliance
// allocates nothing per period here and never wakes an idle stream's stage.
// The stage does not need the idle stretch to be visible: it resets its
// encoder on the play session its gate reports (see pipeline.Gate). Each
// period sent is tagged with the play session it was sent for and the time it
// was read from the capture, so a stage that has fallen behind drops what was
// queued for an earlier client and stamps the rest with their capture time.
type Fanout struct {
	src       Source
	name      string
	consumers []*fanoutConsumer
	closeOnce sync.Once
}

// fanoutConsumer is one downstream Source fed by a Fanout. Read blocks until a
// period arrives or the feed closes (the shared reader ended). dropped is shared
// with the owning stream runtime so a fan-out drop and a downstream frame drop
// accumulate into the one "audio lost for this stream" counter the host monitor
// reads. gate is the stream's play gate (nil means always active, untagged);
// while it reports inactive the consumer is sent nothing and costs no copy.
type fanoutConsumer struct {
	rate, channels int
	ch             chan Period
	dropped        *atomic.Uint64
	gate           func() (active bool, session uint64)
}

// FanoutStream describes one fan-out consumer. Dropped, which is required,
// counts the periods the consumer lost to a full queue; the caller shares it
// with the stream's downstream frame-drop counter. Gate reports whether the
// stream has a client playing and which play session it is
// (rtspserver.ChanSource.Session): while it reports inactive the consumer is
// sent no periods, since its stage would discard them unencoded anyway, and
// each period sent carries the session in Period.Session. A nil Gate means
// always active, with Period.Session left zero (Captured is still set).
type FanoutStream struct {
	Dropped *atomic.Uint64
	Gate    func() (active bool, session uint64)
}

// NewFanout builds a Fanout over src (typically a metered base capture, so every
// channel is metered once here) with one consumer per entry in streams. It
// returns the Fanout and the consumer Sources in the same order. name labels
// drop logs. The consumers are ready to read immediately; they block until Run
// starts feeding them.
func NewFanout(src Source, name string, streams []FanoutStream) (*Fanout, []Source) {
	rate, channels := src.Negotiated()
	consumers := make([]*fanoutConsumer, len(streams))
	out := make([]Source, len(streams))
	for i, st := range streams {
		c := &fanoutConsumer{
			rate:     rate,
			channels: channels,
			ch:       make(chan Period, fanoutBuffer),
			dropped:  st.Dropped,
			gate:     st.Gate,
		}
		consumers[i] = c
		out[i] = c
	}
	return &Fanout{src: src, name: name, consumers: consumers}, out
}

// Run reads the upstream source until it ends, copying each period and handing
// the copy to every active consumer that can accept it without blocking (an idle
// consumer is sent nothing). It returns when the source ends: nil
// on EOF, else the source's error (a real capture closed on purpose returns
// capture.ErrClosed, not EOF). On return it closes
// every consumer feed so each consumer's Read reports EOF and its pipeline
// goroutine exits. Run is the sole reader of src and the sole closer of the
// consumer channels, so no lock is needed.
func (f *Fanout) Run() error {
	for {
		p, err := f.src.Read()
		if err != nil {
			f.closeConsumers()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		f.distribute(p)
	}
}

// distribute hands one upstream period to every consumer without blocking.
//
// The upstream buffer is reused on the next Read, and consumers hold their
// period until their pipeline reads it, so the period is copied once into fresh
// storage the consumers share (never mutated after this point). The copy is
// made for the first active consumer, so a period no client plays costs no
// allocation. An idle consumer is sent nothing, so it can neither wake its
// stage nor fill its queue and count a drop. A client that starts playing
// after this check loses this period, as it would had it connected that much
// later; it gets no audio captured while the stream sat idle. Periods already
// queued for a stage that had fallen behind stay queued across a teardown, but
// each carries the session it was sent for, so the stage drops them rather
// than encode them for a client that starts playing before it reads them.
func (f *Fanout) distribute(p Period) {
	var cp Period
	copied := false
	for _, c := range f.consumers {
		var session uint64
		if c.gate != nil {
			var on bool
			if on, session = c.gate(); !on {
				continue
			}
		}
		if !copied {
			// Stamped before the copy, so a large ultrasonic period's copy does
			// not delay its capture time. An upstream that already stamped the
			// period knows its capture time better than this read does, so its
			// stamp is kept (as pipeline.Gate keeps it).
			captured := p.Captured
			if captured.IsZero() {
				captured = time.Now()
			}
			buf := make([]byte, len(p.Buf))
			copy(buf, p.Buf)
			cp = Period{Buf: buf, Frames: p.Frames, Captured: captured}
			copied = true
		}
		// The storage is shared; only the session tag differs per consumer.
		out := cp
		out.Session = session
		select {
		case c.ch <- out:
		default:
			// The consumer's queue is full: its encoder or client is not keeping up.
			// Drop this period for that stream only; the shared reader must not block
			// or the capture overruns for every stream.
			n := c.dropped.Add(1)
			if n%50 == 1 {
				log.Printf("%s: fan-out dropping periods for a stream (encoder or client not keeping up, total: %d)", f.name, n)
			}
		}
	}
}

// closeConsumers closes every consumer feed so a blocked or subsequent Read
// returns io.EOF. Called once by Run after the source ends.
func (f *Fanout) closeConsumers() {
	for _, c := range f.consumers {
		close(c.ch)
	}
}

// Close ends the fan-out by closing the upstream source, which makes the next
// upstream Read fail and drives Run to close the consumer feeds. It is
// idempotent (a sync.Once guards the single upstream close), so the several
// teardown paths that reach for it (a deliberate stop, a spontaneous pump exit,
// a per-stream stage fault, shutdown) can all call it without double-closing the
// capture. Safe to call from a goroutine other than Run.
func (f *Fanout) Close() error {
	f.closeOnce.Do(func() { _ = f.src.Close() })
	return nil
}

func (c *fanoutConsumer) Negotiated() (rate, channels int) { return c.rate, c.channels }

// Read returns the next period, blocking until one is available or the feed is
// closed. A closed feed reports io.EOF, so the consumer's pipeline stage returns
// as it would at the end of any source.
func (c *fanoutConsumer) Read() (Period, error) {
	p, ok := <-c.ch
	if !ok {
		return Period{}, io.EOF
	}
	return p, nil
}

// Close is a no-op: the Fanout owns the shared capture's lifecycle and closing it
// (Fanout.Close) tears down every consumer. A consumer's pipeline stage never
// closes its own source.
func (c *fanoutConsumer) Close() error { return nil }
