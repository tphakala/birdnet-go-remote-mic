package rtspserver

import (
	"encoding/binary"
	"time"

	rtsp "github.com/tphakala/go-audio-stream/rtsp"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// maxWriterPayload bounds a single RTP payload. The reused write buffer is
// sized so AppendPacket never reallocates: a reallocation would move the packet
// off the buffer and break the interleave prefix written into buf[0:4].
const maxWriterPayload = 15360

// runWriter is the playing session's single writer: it drains frames from the
// FrameSource and writes each as an interleaved RTP packet, then emits an RTCP
// sender report every SRInterval. It owns the reused packet buffer, and every
// write goes through writeRaw so it never interleaves with an RTSP response.
func (cs *connSession) runWriter() {
	defer cs.close()

	pt := uint8(cs.track.PayloadType)
	ssrc := randU32()
	seq := cs.startSeq
	ts := cs.startTS
	var packetCount, octetCount uint32

	buf := make([]byte, 4+rtp.HeaderSize+maxWriterPayload)
	// Fire the first sender report right after the first frame (early clock
	// sync for the receiver), then every SRInterval.
	lastSR := time.Now().Add(-cs.srv.cfg.SRInterval)
	var lastCaptured time.Time
	var lastTS uint32

	for {
		frame, err := cs.track.Frames.Next(cs.ctx)
		if err != nil {
			return
		}
		// Proactive eviction: stop streaming the moment this connection is no
		// longer authorized under the guard's current generation (the token was
		// rotated past the one it authenticated under, or, for an open-access
		// session that never authenticated, a token was just enabled). handle()
		// only re-challenges when the client sends a request, but live555/VLC
		// keepalive with OPTIONS and other clients send only RTCP, so a
		// request-driven check alone would let such a session stream on
		// indefinitely after the token changed. The enabled state and generation
		// are read as one Snapshot so a rotation cannot split them. Returning here
		// runs the same teardown as any other writer exit: defer cs.close()
		// cancels the context and closes the socket, and serveConn's deferred
		// cleanup releases the track slot.
		if enabled, gen := cs.srv.cfg.Auth.Snapshot(); shouldEvict(enabled, gen, cs.authed.Load(), cs.authGen.Load()) {
			return
		}
		if len(frame.Payload) > maxWriterPayload {
			return // out-of-contract payload: tear down rather than corrupt the frame
		}
		hdr := rtp.Header{Version: 2, PayloadType: pt, SequenceNumber: seq, Timestamp: ts, SSRC: ssrc}
		pkt, aerr := rtp.AppendPacket(buf[4:4], hdr, frame.Payload)
		if aerr != nil {
			return
		}
		plen := len(pkt)
		buf[0] = '$'
		buf[1] = byte(cs.rtpCh)
		binary.BigEndian.PutUint16(buf[2:4], uint16(plen))
		if !cs.writeRaw(buf[:4+plen]) {
			return
		}

		seq++
		ts += frame.Duration
		packetCount++
		octetCount += uint32(len(frame.Payload))
		lastCaptured = frame.Captured
		lastTS = hdr.Timestamp

		if time.Since(lastSR) >= cs.srv.cfg.SRInterval && !lastCaptured.IsZero() {
			sr := rtp.SenderReport{
				SSRC:         ssrc,
				NTPTimestamp: rtp.NTPFromTime(lastCaptured),
				RTPTimestamp: lastTS,
				PacketCount:  packetCount,
				OctetCount:   octetCount,
			}
			// MarshalInterleaved allocates, which is fine at one SR per interval.
			if raw, merr := rtsp.MarshalInterleaved(cs.rtcpCh, sr.Marshal()); merr == nil {
				if !cs.writeRaw(raw) {
					return
				}
			}
			lastSR = time.Now()
		}
	}
}
