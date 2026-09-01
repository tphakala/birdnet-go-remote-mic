// Package announce advertises the appliance on the LAN over mDNS / DNS-SD so
// BirdNET-Go can discover it. It advertises only; browsing is the consumer's
// job. The service type is _rtsp._tcp so generic tools (avahi-browse, dns-sd)
// see it too.
package announce

import (
	"context"
	"errors"
	"strconv"

	"github.com/brutella/dnssd"
)

// serviceType is the DNS-SD service type advertised.
const serviceType = "_rtsp._tcp"

// Info is what the appliance advertises.
type Info struct {
	Name     string // instance name (dnssd renames on conflict)
	Path     string // RTSP path of this stream, e.g. "/stream"
	Port     int    // RTSP port
	Codec    string // "L16" or "opus"
	Rate     int    // sample rate in Hz
	Channels int
	Version  string // binary version
	// AuthRequired is whether the appliance demands its shared token: the TXT
	// record advertises auth=token so BirdNET-Go's adopt flow asks for it, or
	// auth=none for open access.
	AuthRequired bool
}

// txtRecords builds the TXT key/value set advertised with the service. The
// schema is coordinated with the BirdNET-Go adopt flow (txtvers 1).
func txtRecords(info *Info) map[string]string {
	authHint := "none"
	if info.AuthRequired {
		authHint = "token"
	}
	return map[string]string{
		"txtvers": "1",
		"model":   "birdnet-go-remote-mic",
		"version": info.Version,
		"codec":   info.Codec,
		"rate":    strconv.Itoa(info.Rate),
		"ch":      strconv.Itoa(info.Channels),
		"path":    info.Path,
		"auth":    authHint,
	}
}

// Run advertises every service until ctx is cancelled, at which point dnssd
// sends goodbye packets to flush peer caches. A startup error (no services, bad
// config, no usable interface) is returned so the caller can warn and keep
// serving without discovery; a clean cancellation returns nil.
//
// Known limitation: dnssd cannot remove one service from a live responder, so a
// device that dies mid-run keeps its advertisement until the process exits;
// clients that discover it get a 404 from the RTSP server.
func Run(ctx context.Context, infos []Info) error {
	if len(infos) == 0 {
		return errors.New("announce: no services to advertise")
	}
	resp, err := dnssd.NewResponder()
	if err != nil {
		return err
	}
	for i := range infos {
		info := &infos[i]
		srv, err := dnssd.NewService(dnssd.Config{
			Name: info.Name,
			Type: serviceType,
			Port: info.Port,
			Text: txtRecords(info),
		})
		if err != nil {
			return err
		}
		if _, err := resp.Add(srv); err != nil {
			return err
		}
	}
	if err := resp.Respond(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
