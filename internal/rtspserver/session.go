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
	srv  *Server
	conn net.Conn

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
}

func (s *Server) serveConn(parent context.Context, conn net.Conn) {
	ctx, cancel := context.WithCancel(parent)
	cs := &connSession{srv: s, conn: conn, ctx: ctx, cancel: cancel}
	defer func() {
		if cs.hasSlot {
			if a, ok := s.frames.(activator); ok {
				a.SetActive(false)
			}
			s.releaseSlot()
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

func (cs *connSession) respondOptions(req *rtsp.Request) {
	h := rtsp.Header{}
	h.Set("Public", "OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN, GET_PARAMETER")
	cs.write(&rtsp.Response{StatusCode: 200, Reason: "OK", CSeq: req.CSeq, Header: h})
}

func (cs *connSession) respondDescribe(req *rtsp.Request) {
	if cs.matchPath(req.URL) != pathSession {
		cs.respondStatus(req, 404, "Not Found")
		return
	}
	h := rtsp.Header{}
	h.Set("Content-Type", "application/sdp")
	h.Set("Content-Base", cs.contentBase(req))
	cs.write(&rtsp.Response{StatusCode: 200, Reason: "OK", CSeq: req.CSeq, Header: h, Body: cs.srv.cfg.SDP})
}

func (cs *connSession) respondSetup(req *rtsp.Request) {
	if cs.matchPath(req.URL) != pathTrack {
		cs.respondStatus(req, 404, "Not Found")
		return
	}
	th, err := rtsp.ParseTransport(req.Header.Get("Transport"))
	if err != nil || !strings.Contains(strings.ToUpper(th.Protocol), "TCP") {
		cs.respondStatus(req, 461, "Unsupported Transport")
		return
	}
	if !cs.hasSlot {
		if !cs.srv.acquireSlot() {
			cs.respondStatus(req, 453, "Not Enough Bandwidth")
			return
		}
		cs.hasSlot = true
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

	// Turn frame delivery on before the PLAY response goes out; the writer
	// itself starts only after the response is on the wire, so the client
	// sees RTP-Info's starting seq/rtptime before the first packet.
	if startWriter && cs.srv.frames != nil {
		if a, ok := cs.srv.frames.(activator); ok {
			a.SetActive(true)
		}
	}

	h := rtsp.Header{}
	h.Set("Session", cs.id)
	h.Set("Range", "npt=0.000-")
	h.Set("RTP-Info", fmt.Sprintf("url=%strackID=0;seq=%d;rtptime=%d", cs.contentBase(req), cs.startSeq, cs.startTS))
	cs.write(&rtsp.Response{StatusCode: 200, Reason: "OK", CSeq: req.CSeq, Header: h})

	// Start streaming only after the PLAY response is on the wire, so the
	// client sees RTP-Info's starting seq/rtptime before the first packet.
	if startWriter && cs.srv.frames != nil && !cs.writing {
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

func (cs *connSession) matchPath(rawURL string) pathKind {
	u, err := url.Parse(rawURL)
	if err != nil {
		return pathNone
	}
	switch u.Path {
	case cs.srv.cfg.Path:
		return pathSession
	case cs.srv.cfg.Path + "/trackID=0":
		return pathTrack
	default:
		return pathNone
	}
}

func (cs *connSession) contentBase(req *rtsp.Request) string {
	host := req.Header.Get("Host")
	if host == "" {
		if u, err := url.Parse(req.URL); err == nil && u.Host != "" {
			host = u.Host
		} else {
			host = cs.conn.LocalAddr().String()
		}
	}
	return "rtsp://" + host + cs.srv.cfg.Path + "/"
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
