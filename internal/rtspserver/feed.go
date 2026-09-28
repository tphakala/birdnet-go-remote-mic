package rtspserver

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
)

// ErrSourceClosed is returned by Next once the source's device is gone, and by
// Subscribe on a feed that has been closed.
var ErrSourceClosed = errors.New("rtspserver: frame source closed")

// ErrTooManyClients is returned by Subscribe when the stream already has
// maxClients subscribers.
var ErrTooManyClients = errors.New("rtspserver: too many clients on this stream")

const (
	// maxClients is the per-stream cap on simultaneous subscribers. It covers a
	// few BirdNET-Go instances plus their stream tests and a listener, and bounds
	// a runaway reconnect loop. A constant: config is additive-only, so a setting
	// can be added later if someone needs one.
	maxClients = 8
	// subQueueDepth is the frames queued per subscriber: 1.28 s of Opus (20 ms
	// frames), and up to 1.28 s of PCM (20 ms frames; less where the 15360-byte
	// L16 cap shortens them: 10 ms at 384 kHz stereo, 2.5 ms at 384 kHz with
	// eight channels, so as little as 0.16 s).
	subQueueDepth = 64
)

// FrameFeed is how the server attaches a playing client to a track's media.
type FrameFeed interface {
	Subscribe() (Subscription, error)
}

// Subscription is one client's view of a FrameFeed: its writer Nexts frames
// and the connection Closes it when the client is gone.
type Subscription interface {
	Next(ctx context.Context) (pipeline.Frame, error)
	// Close unsubscribes. It is idempotent.
	Close()
}

// Feed is a broadcast FrameFeed: the pipeline Pushes each frame once and every
// subscribed client receives it from its own bounded queue, so one encode
// serves all clients and a slow client drops only its own frames. Push copies
// the payload once (whatever the client count) so the pipeline's buffer reuse
// is safe; the clients share that read-only copy.
type Feed struct {
	mu        sync.Mutex               // serializes Subscribe and Sub.Close
	snap      atomic.Pointer[feedSnap] // Push, Session and Clients read it in one load
	done      chan struct{}            // closed by Close: the device is gone
	closeOnce sync.Once
}

// feedSnap is the immutable state Push and Session read: the play session and
// the subscribers of that session. Replaced copy-on-write under Feed.mu, so
// the active flag can never disagree with the client set.
type feedSnap struct {
	// epoch is the play session: it advances when the first client joins an
	// idle stream and is kept while any client plays and after the last leaves.
	epoch uint64
	subs  []*Sub // never mutated after publication
}

// NewFeed returns a Feed with no subscribers.
func NewFeed() *Feed {
	f := &Feed{done: make(chan struct{})}
	f.snap.Store(&feedSnap{})
	return f
}

// Sub is one client's subscription to a Feed; it implements Subscription.
type Sub struct {
	feed   *Feed
	ch     chan pipeline.Frame
	closed bool // guarded by feed.mu
}

var (
	_ FrameFeed    = (*Feed)(nil)
	_ Subscription = (*Sub)(nil)
)

// Subscribe attaches a client. It fails with ErrSourceClosed once the feed is
// closed and with ErrTooManyClients at maxClients. Joining an idle stream
// starts a new play session; joining one that is playing keeps the session, so
// the pipeline's encoder is not reset under the clients already playing. The
// new client's queue starts empty: it receives frames from the next Push on.
func (f *Feed) Subscribe() (Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.done:
		return nil, ErrSourceClosed
	default:
	}
	old := f.snap.Load()
	if len(old.subs) >= maxClients {
		return nil, ErrTooManyClients
	}
	s := &Sub{feed: f, ch: make(chan pipeline.Frame, subQueueDepth)}
	next := &feedSnap{epoch: old.epoch, subs: slices.Concat(old.subs, []*Sub{s})}
	if len(old.subs) == 0 {
		next.epoch++
	}
	f.snap.Store(next)
	return s, nil
}

// Push copies a frame once and offers it to every subscriber without blocking,
// returning how many had a full queue and dropped it (a slow client never
// stalls the stage or the other clients). With no subscribers, or for a frame
// tagged with another play session than the current one (pipeline.Frame.
// Session; it was produced for an earlier stretch of playing and overtaken
// while it was being encoded), it returns 0 without copying. An untagged frame
// (session zero) is delivered to whoever plays.
func (f *Feed) Push(fr pipeline.Frame) (dropped int) {
	snap := f.snap.Load()
	if len(snap.subs) == 0 || (fr.Session != 0 && fr.Session != snap.epoch) {
		return 0
	}
	// Copy the whole frame, then detach only the payload, so a field added to
	// pipeline.Frame later is carried without touching this.
	cp := fr
	cp.Payload = append([]byte(nil), fr.Payload...)
	for _, s := range snap.subs {
		select {
		case s.ch <- cp:
		default:
			dropped++
		}
	}
	return dropped
}

// Session reports whether any client is playing and which play session it is,
// from one snapshot. The session advances when the first client joins an idle
// stream and is kept after the last one leaves, so a stage comparing it with
// the session it last encoded for sees every new stretch of playing, even one
// that began within a period of the previous one ending (see pipeline.Gate).
// It is one atomic load.
func (f *Feed) Session() (active bool, session uint64) {
	snap := f.snap.Load()
	return len(snap.subs) > 0, snap.epoch
}

// Clients reports how many clients are subscribed.
func (f *Feed) Clients() int { return len(f.snap.Load().subs) }

// Close marks the feed dead: every Next returns ErrSourceClosed so the
// playing writers tear down, and later Subscribe calls fail. Safe to call more
// than once.
func (f *Feed) Close() {
	f.closeOnce.Do(func() { close(f.done) })
}

// Next returns the next frame, blocking until one is available, ctx is done,
// or the feed is closed (dead device). Closure wins over a queued frame, so a
// dead device's writer tears down at once rather than emitting one last packet
// (select would otherwise pick a ready case at random). It needs no session
// check: a subscriber appears only in snapshots of its own play session, and
// Push sends it only frames tagged with that session or untagged.
func (s *Sub) Next(ctx context.Context) (pipeline.Frame, error) {
	select {
	case <-s.feed.done:
		return pipeline.Frame{}, ErrSourceClosed
	default:
	}
	select {
	case <-ctx.Done():
		return pipeline.Frame{}, ctx.Err()
	case <-s.feed.done:
		return pipeline.Frame{}, ErrSourceClosed
	case f := <-s.ch:
		return f, nil
	}
}

// Close unsubscribes. The last client out makes the feed report inactive at
// once, so the stage and the fan-out go idle. Idempotent.
func (s *Sub) Close() {
	f := s.feed
	f.mu.Lock()
	defer f.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	old := f.snap.Load()
	// Clone before deleting: DeleteFunc works in place and would corrupt the
	// published slice a concurrent Push is ranging over.
	subs := slices.DeleteFunc(slices.Clone(old.subs), func(x *Sub) bool { return x == s })
	f.snap.Store(&feedSnap{epoch: old.epoch, subs: subs})
}
