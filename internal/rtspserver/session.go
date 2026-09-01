package rtspserver

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	rtsp "github.com/tphakala/go-audio-stream/rtsp"
)

type sessionState int

const (
	stateInit sessionState = iota
	stateReady
	statePlaying
)

type pathKind int

const (
	pathNone pathKind = iota
	pathSession
	pathTrack
)

// connSession is the per-connection RTSP state machine.
type connSession struct {
	srv   *Server
	conn  net.Conn
	track *Track // bound at SETUP; one connection serves one track

	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	writeMu   sync.Mutex

	state    sessionState
	id       string
	rtpCh    int
	rtcpCh   int
	startSeq uint16
	startTS  uint32
	hasSlot  bool
	writing  bool // the media writer goroutine is running

	// nonce is the Digest nonce issued to this connection on its first
	// challenge; answers are accepted only under it, so a captured Authorization
	// header is useless on any other connection. authed records that a valid
	// answer arrived: authentication is per connection, later requests are not
	// re-checked (every client resends credentials anyway, and tolerating one
	// that does not costs nothing on a single TCP connection).
	nonce  string
	authed bool
}

func (s *Server) serveConn(parent context.Context, conn net.Conn) {
	ctx, cancel := context.WithCancel(parent)
	cs := &connSession{srv: s, conn: conn, ctx: ctx, cancel: cancel}
	defer func() {
		if cs.hasSlot {
			if a, ok := cs.track.Frames.(activator); ok {
				a.SetActive(false)
			}
			cs.track.releaseSlot()
		}
		cs.close()
	}()

	buf := make([]byte, 0, 8192)
	tmp := make([]byte, 4096)
	for {
		for len(buf) > 0 {
			consumed, fatal := cs.step(buf)
			if fatal {
				return
			}
			if consumed == 0 {
				break // need more bytes
			}
			m := copy(buf, buf[consumed:])
			buf = buf[:m]
		}
		if ctx.Err() != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(s.cfg.Timeout))
		n, err := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			return
		}
	}
}

// step processes one frame or message at the front of buf. It returns the bytes
// consumed (0 means "need more") and whether the connection should close.
func (cs *connSession) step(buf []byte) (consumed int, fatal bool) {
	switch rtsp.ClassifyStream(buf) {
	case rtsp.FrameNeedMore:
		return 0, false
	case rtsp.FrameInterleaved:
		_, n, err := rtsp.ParseInterleaved(buf)
		if errors.Is(err, rtsp.ErrIncomplete) {
			return 0, false
		}
		if err != nil {
			return 1, false // resynchronize
		}
		return n, false // client RTCP RR: liveness only, discard
	case rtsp.FrameRequest:
		req, n, err := rtsp.ParseRequest(buf)
		if errors.Is(err, rtsp.ErrIncomplete) {
			return 0, false
		}
		if err != nil {
			return 0, true // malformed request: close
		}
		return n, cs.handle(req)
	case rtsp.FrameResponse, rtsp.FrameUnknown:
		return 1, false // not expected from a client; resynchronize
	default:
		return 1, false
	}
}

func (cs *connSession) handle(req *rtsp.Request) (fatal bool) {
	// OPTIONS stays open: clients probe capabilities before they authenticate
	// (live555 and mediamtx behave the same way). Everything else, including an
	// unknown method, must authenticate first.
	if req.Method != "OPTIONS" && !cs.authorized(req) {
		cs.respondUnauthorized(req)
		return false
	}
	switch req.Method {
	case "OPTIONS":
		cs.respondOptions(req)
	case "DESCRIBE":
		cs.respondDescribe(req)
	case "SETUP":
		cs.respondSetup(req)
	case "PLAY":
		cs.respondPlay(req)
	case "GET_PARAMETER":
		cs.respondSession(req, 200, "OK")
	case "TEARDOWN":
		cs.respondSession(req, 200, "OK")
		return true
	default:
		cs.respondStatus(req, 501, "Not Implemented")
	}
	return false
}

// authorized reports whether req may proceed: always when no token is
// configured, once the connection has authenticated, or when req carries a
// valid Digest answer to this connection's nonce, which authenticates the
// connection for its lifetime. A token enabled by a hot reload starts
// challenging an open-access connection on its next request.
func (cs *connSession) authorized(req *rtsp.Request) bool {
	g := cs.srv.cfg.Auth
	if !g.Enabled() || cs.authed {
		return true
	}
	if cs.nonce != "" && g.CheckDigest(req.Method, req.Header.Get("Authorization"), cs.nonce) {
		cs.authed = true
		return true
	}
	return false
}

// respondUnauthorized answers 401 with this connection's Digest challenge,
// minting the nonce on the first challenge. The connection stays open so the
// client can retry with credentials.
func (cs *connSession) respondUnauthorized(req *rtsp.Request) {
	if cs.nonce == "" {
		cs.nonce = cs.srv.cfg.Auth.NewNonce()
	}
	h := rtsp.Header{}
	h.Set("WWW-Authenticate", cs.srv.cfg.Auth.DigestChallenge(cs.nonce))
	cs.write(&rtsp.Response{StatusCode: 401, Reason: "Unauthorized", CSeq: req.CSeq, Header: h})
}

func (cs *connSession) respondOptions(req *rtsp.Request) {
	h := rtsp.Header{}
	h.Set("Public", "OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN, GET_PARAMETER")
	cs.write(&rtsp.Response{StatusCode: 200, Reason: "OK", CSeq: req.CSeq, Header: h})
}

func (cs *connSession) respondDescribe(req *rtsp.Request) {
	t, kind := cs.resolve(req.URL)
	if kind != pathSession {
		cs.respondStatus(req, 404, "Not Found")
		return
	}
	h := rtsp.Header{}
	h.Set("Content-Type", "application/sdp")
	h.Set("Content-Base", cs.contentBase(req, t.Path))
	cs.write(&rtsp.Response{StatusCode: 200, Reason: "OK", CSeq: req.CSeq, Header: h, Body: t.SDP})
}

func (cs *connSession) respondSetup(req *rtsp.Request) {
	t, kind := cs.resolve(req.URL)
	if kind != pathTrack {
		cs.respondStatus(req, 404, "Not Found")
		return
	}
	if cs.track != nil && cs.track != t {
		// One connection serves one track; BirdNET-Go opens one conn per URL.
		cs.respondStatus(req, 455, "Method Not Valid in This State")
		return
	}
	if cs.state == statePlaying {
		// RFC 2326 A.1: SETUP is not valid while Playing. A re-SETUP here would
		// re-randomize startSeq/startTS (below) while the running writer keeps
		// the old values, permanently breaking RTP seq/timestamp continuity for
		// the live session. BirdNET-Go does one SETUP/PLAY per connection, so
		// this is off the normal path; reject it rather than desync the stream.
		cs.respondStatus(req, 455, "Method Not Valid in This State")
		return
	}
	th, err := rtsp.ParseTransport(req.Header.Get("Transport"))
	if err != nil || !strings.Contains(strings.ToUpper(th.Protocol), "TCP") {
		cs.respondStatus(req, 461, "Unsupported Transport")
		return
	}
	if !cs.hasSlot {
		if !t.acquireSlot() {
			cs.respondStatus(req, 453, "Not Enough Bandwidth")
			return
		}
		cs.hasSlot = true
		cs.track = t
	}

	// The client chooses the interleaved channel pair (RFC 2326); default to
	// 0-1 only when it was absent or unusable.
	cs.rtpCh, cs.rtcpCh = 0, 1
	if th.Interleaved {
		if a, b, cerr := th.InterleavedChannels(nil); cerr == nil {
			cs.rtpCh, cs.rtcpCh = a, b
		}
	}
	cs.id = newSessionID()
	cs.startSeq = uint16(randU32())
	cs.startTS = randU32()
	cs.state = stateReady

	h := rtsp.Header{}
	h.Set("Transport", fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d", cs.rtpCh, cs.rtcpCh))
	h.Set("Session", cs.id+";timeout="+strconv.Itoa(int(cs.srv.cfg.Timeout.Seconds())))
	cs.write(&rtsp.Response{StatusCode: 200, Reason: "OK", CSeq: req.CSeq, Header: h})
}

func (cs *connSession) respondPlay(req *rtsp.Request) {
	if cs.state == stateInit || !cs.sessionMatches(req) {
		cs.respondStatus(req, 454, "Session Not Found")
		return
	}
	startWriter := cs.state != statePlaying
	cs.state = statePlaying

	// Enable frame delivery before the PLAY response goes out so no current
	// audio is dropped, but hold the writer itself until the response is on the
	// wire (see below).
	if startWriter && cs.track.Frames != nil {
		if a, ok := cs.track.Frames.(activator); ok {
			a.SetActive(true)
		}
	}

	h := rtsp.Header{}
	h.Set("Session", cs.id)
	h.Set("Range", "npt=0.000-")
	h.Set("RTP-Info", fmt.Sprintf("url=%strackID=0;seq=%d;rtptime=%d", cs.contentBase(req, cs.track.Path), cs.startSeq, cs.startTS))
	cs.write(&rtsp.Response{StatusCode: 200, Reason: "OK", CSeq: req.CSeq, Header: h})

	// Start streaming only after the PLAY response is on the wire, so the
	// client sees RTP-Info's starting seq/rtptime before the first packet.
	if startWriter && cs.track.Frames != nil && !cs.writing {
		cs.writing = true
		go cs.runWriter()
	}
}

// respondSession answers with a status and the Session header (used for
// GET_PARAMETER keepalive and TEARDOWN).
func (cs *connSession) respondSession(req *rtsp.Request, code int, reason string) {
	h := rtsp.Header{}
	if cs.id != "" {
		h.Set("Session", cs.id)
	}
	cs.write(&rtsp.Response{StatusCode: code, Reason: reason, CSeq: req.CSeq, Header: h})
}

func (cs *connSession) respondStatus(req *rtsp.Request, code int, reason string) {
	cs.write(&rtsp.Response{StatusCode: code, Reason: reason, CSeq: req.CSeq})
}

func (cs *connSession) write(resp *rtsp.Response) {
	data, err := rtsp.MarshalResponse(resp)
	if err != nil {
		return
	}
	cs.writeRaw(data)
}

// writeRaw writes one complete message or interleaved frame to the connection
// under the write mutex, so RTSP responses, RTP media, and RTCP never interleave
// mid-write. It returns false on a write error (the caller should tear down).
func (cs *connSession) writeRaw(b []byte) bool {
	cs.writeMu.Lock()
	defer cs.writeMu.Unlock()
	_ = cs.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := cs.conn.Write(b)
	return err == nil
}

// close tears the connection down once: it cancels the per-conn context (so the
// writer stops) and closes the socket (so a parked reader or writer unblocks).
func (cs *connSession) close() {
	cs.closeOnce.Do(func() {
		cs.cancel()
		_ = cs.conn.Close()
	})
}

// resolve maps a request URL to the registered track it addresses: the track's
// own path is the session URL, path+"/trackID=0" the track URL.
func (cs *connSession) resolve(rawURL string) (*Track, pathKind) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, pathNone
	}
	if t := cs.srv.lookup(u.Path); t != nil {
		return t, pathSession
	}
	if p, ok := strings.CutSuffix(u.Path, "/trackID=0"); ok {
		if t := cs.srv.lookup(p); t != nil {
			return t, pathTrack
		}
	}
	return nil, pathNone
}

func (cs *connSession) contentBase(req *rtsp.Request, path string) string {
	host := req.Header.Get("Host")
	if host == "" {
		if u, err := url.Parse(req.URL); err == nil && u.Host != "" {
			host = u.Host
		} else {
			host = cs.conn.LocalAddr().String()
		}
	}
	return "rtsp://" + host + path + "/"
}

func (cs *connSession) sessionMatches(req *rtsp.Request) bool {
	if cs.id == "" {
		return false
	}
	got := req.Header.Get("Session")
	if got == "" {
		return false
	}
	id, _, _ := strings.Cut(got, ";") // strip any ;timeout the client echoed
	return strings.TrimSpace(id) == cs.id
}

func newSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func randU32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}
