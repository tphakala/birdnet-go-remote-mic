package rtspserver

import (
	"errors"
	"testing"
	"time"

	rtsp "github.com/tphakala/go-audio-stream/rtsp"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"

	"github.com/tphakala/birdnet-go-remote-mic/internal/pipeline"
)

// nextRTP reads interleaved frames from c until an RTP packet (channel 0)
// arrives and returns its header, skipping RTCP. It must not be mixed with c.do
// on a connection that is streaming, since a response would be mistaken for
// media.
func (c *client) nextRTP(t *testing.T) rtp.Header {
	t.Helper()
	for {
		var (
			fr  rtsp.InterleavedFrame
			n   int
			err = rtsp.ErrIncomplete // an empty buffer needs more bytes
		)
		if len(c.buf) > 0 {
			fr, n, err = rtsp.ParseInterleaved(c.buf)
		}
		switch {
		case err == nil:
			c.buf = c.buf[n:]
			if fr.Channel != 0 {
				continue
			}
			pkt, perr := rtp.ParsePacket(fr.Payload)
			if perr != nil {
				t.Fatalf("parse RTP: %v", perr)
			}
			return pkt.Header
		case !errors.Is(err, rtsp.ErrIncomplete):
			t.Fatalf("parse interleaved frame: %v", err)
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		m, rerr := c.conn.Read(c.tmp)
		if m > 0 {
			c.buf = append(c.buf, c.tmp[:m]...)
		}
		if rerr != nil {
			t.Fatalf("read RTP: %v", rerr)
		}
	}
}

// feedTrack returns a track over a fresh Feed, and the feed.
func feedTrack() (*Track, *Feed) {
	f := NewFeed()
	return &Track{Path: testPath, SDP: testSDP, PayloadType: 96, Feed: f}, f
}

func pushOne(f *Feed) int {
	return f.Push(pipeline.Frame{Payload: make([]byte, 320), Duration: 160, Captured: time.Now()})
}

// TestSecondClientSurvivesFirstLeaving pins that clients are independent: A
// plays, B joins, A drops, and B's sequence numbers and timestamps stay
// continuous under an SSRC of its own.
func TestSecondClientSurvivesFirstLeaving(t *testing.T) {
	track, feed := feedTrack()
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second, SRInterval: time.Hour}, track)

	a, b := dial(t, addr), dial(t, addr)
	setupAndPlay(t, a, addr)
	setupAndPlay(t, b, addr)
	waitFor(t, func() bool { return feed.Clients() == 2 }, 2*time.Second, "both clients to subscribe")

	var bSeen []rtp.Header
	var aSSRC uint32
	for range 5 {
		if pushOne(feed) != 0 {
			t.Fatal("a frame was dropped with both clients reading")
		}
		aSSRC = a.nextRTP(t).SSRC
		bSeen = append(bSeen, b.nextRTP(t))
	}

	_ = a.conn.Close()
	waitFor(t, func() bool { return feed.Clients() == 1 }, 2*time.Second, "A to unsubscribe")

	for range 5 {
		pushOne(feed)
		bSeen = append(bSeen, b.nextRTP(t))
	}

	for i := 1; i < len(bSeen); i++ {
		prev, cur := bSeen[i-1], bSeen[i]
		if cur.SequenceNumber != prev.SequenceNumber+1 {
			t.Errorf("B packet %d: seq %d after %d, want consecutive", i, cur.SequenceNumber, prev.SequenceNumber)
		}
		if cur.Timestamp != prev.Timestamp+160 {
			t.Errorf("B packet %d: timestamp %d after %d, want +160", i, cur.Timestamp, prev.Timestamp)
		}
		if cur.SSRC != bSeen[0].SSRC {
			t.Errorf("B packet %d: SSRC changed", i)
		}
	}
	if bSeen[0].SSRC == aSSRC {
		t.Errorf("both clients use SSRC %#x, want one each", aSSRC)
	}
}

// TestSilentConnectionDoesNotBlockNewClient pins the stale-session case: a
// connection that played and then went silent (no TEARDOWN, socket open, for
// example a host that lost power) does not stop a new connection from the same
// host playing.
func TestSilentConnectionDoesNotBlockNewClient(t *testing.T) {
	track, feed := feedTrack()
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second, SRInterval: time.Hour}, track)

	stale := dial(t, addr)
	setupAndPlay(t, stale, addr)

	fresh := dial(t, addr)
	setupAndPlay(t, fresh, addr)
	waitFor(t, func() bool { return feed.Clients() == 2 }, 2*time.Second, "both connections to subscribe")
	pushOne(feed)
	fresh.nextRTP(t)
}

// TestPlayBeyondTheClientCap pins the cap: the ninth PLAY on a path is
// refused with 453, starts no writer, reports no connect, and leaves the count
// at 8. The refused connection stays Ready, so a PLAY retried after a slot
// frees succeeds.
func TestPlayBeyondTheClientCap(t *testing.T) {
	track, feed := feedTrack()
	rec := &recordingListener{}
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second, SRInterval: time.Hour, Listener: rec}, track)

	players := make([]*client, maxClients)
	for i := range players {
		players[i] = dial(t, addr)
		setupAndPlay(t, players[i], addr)
	}
	waitFor(t, func() bool { return rec.connectCount() == maxClients }, 2*time.Second, "every player's connect")

	extra := dial(t, addr)
	setup := extra.do(t, "SETUP", trackURL(addr), tcpTransport("0-1"))
	if setup.StatusCode != 200 {
		t.Fatalf("SETUP beyond the cap = %d, want 200 (the cap applies at PLAY)", setup.StatusCode)
	}
	sess := rtsp.Header{}
	sess.Set("Session", setup.Header.Get("Session"))
	if r := extra.do(t, "PLAY", baseURL(addr), sess); r.StatusCode != 453 {
		t.Fatalf("PLAY beyond the cap = %d, want 453", r.StatusCode)
	}
	if got := feed.Clients(); got != maxClients {
		t.Errorf("Clients() = %d after the refused PLAY, want %d", got, maxClients)
	}
	if got := rec.connectCount(); got != maxClients {
		t.Errorf("connect events = %d after the refused PLAY, want %d", got, maxClients)
	}

	_ = players[0].conn.Close()
	waitFor(t, func() bool { return feed.Clients() == maxClients-1 }, 2*time.Second, "a slot to free")
	if r := extra.do(t, "PLAY", baseURL(addr), sess); r.StatusCode != 200 {
		t.Errorf("PLAY retried after a slot freed = %d, want 200", r.StatusCode)
	}
}

// TestPlayAfterFeedClosed pins that a track whose device went away between
// SETUP and PLAY answers 404 and starts no writer.
func TestPlayAfterFeedClosed(t *testing.T) {
	track, feed := feedTrack()
	rec := &recordingListener{}
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second, SRInterval: time.Hour, Listener: rec}, track)

	c := dial(t, addr)
	setup := c.do(t, "SETUP", trackURL(addr), tcpTransport("0-1"))
	if setup.StatusCode != 200 {
		t.Fatalf("SETUP = %d", setup.StatusCode)
	}
	feed.Close()
	sess := rtsp.Header{}
	sess.Set("Session", setup.Header.Get("Session"))
	if r := c.do(t, "PLAY", baseURL(addr), sess); r.StatusCode != 404 {
		t.Fatalf("PLAY on a closed feed = %d, want 404", r.StatusCode)
	}
	if got := rec.connectCount(); got != 0 {
		t.Errorf("connect events = %d, want 0", got)
	}
	if got := feed.Clients(); got != 0 {
		t.Errorf("Clients() = %d, want 0", got)
	}
}

// TestRepeatedPlayDoesNotResubscribe pins that PLAY on a connection that is
// already playing takes no second subscription.
func TestRepeatedPlayDoesNotResubscribe(t *testing.T) {
	track, feed := feedTrack()
	rec := &recordingListener{}
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second, SRInterval: time.Hour, Listener: rec}, track)

	c := dial(t, addr)
	sess := setupAndPlay(t, c, addr)
	if r := c.do(t, "PLAY", baseURL(addr), sess); r.StatusCode != 200 {
		t.Fatalf("repeated PLAY = %d, want 200", r.StatusCode)
	}
	if got := feed.Clients(); got != 1 {
		t.Errorf("Clients() = %d after a repeated PLAY, want 1", got)
	}
	if got := rec.connectCount(); got != 1 {
		t.Errorf("connect events = %d after a repeated PLAY, want 1", got)
	}
}

// errFeed is a FrameFeed whose Subscribe fails with err.
type errFeed struct{ err error }

func (f errFeed) Subscribe() (Subscription, error) { return nil, f.err }

// TestPlayWhenSubscribeFails pins that an unexpected Subscribe error answers
// 500 and starts no writer, leaving the connection Ready.
func TestPlayWhenSubscribeFails(t *testing.T) {
	rec := &recordingListener{}
	track := &Track{Path: testPath, SDP: testSDP, PayloadType: 96, Feed: errFeed{errors.New("boom")}}
	addr, _ := startServer(t, Config{Timeout: 60 * time.Second, SRInterval: time.Hour, Listener: rec}, track)

	c := dial(t, addr)
	setup := c.do(t, "SETUP", trackURL(addr), tcpTransport("0-1"))
	if setup.StatusCode != 200 {
		t.Fatalf("SETUP = %d", setup.StatusCode)
	}
	sess := rtsp.Header{}
	sess.Set("Session", setup.Header.Get("Session"))
	if r := c.do(t, "PLAY", baseURL(addr), sess); r.StatusCode != 500 {
		t.Fatalf("PLAY with a failing feed = %d, want 500", r.StatusCode)
	}
	if got := rec.connectCount(); got != 0 {
		t.Errorf("connect events = %d, want 0", got)
	}
}
