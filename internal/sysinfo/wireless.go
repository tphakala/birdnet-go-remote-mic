package sysinfo

import (
	"strconv"
	"strings"
	"unicode"
)

// Signal bounds for a dBm reading from /proc/net/wireless. A positive level
// comes from a driver that does not set IW_QUAL_DBM, so it is not a dBm value.
const (
	minSignalDBM = -150
	maxSignalDBM = -1
)

// ssidMaxBytes is IW_ESSID_MAX_SIZE, the longest network name 802.11 allows.
const ssidMaxBytes = 32

// Interface kinds reported on the wire (mgmtserver.NetworkInterface.Kind).
const (
	kindEthernet = "ethernet"
	kindWifi     = "wifi"
	kindOther    = "other"
)

// DEVTYPE values from a uevent file that classifyInterface acts on.
const (
	devTypeWLAN      = "wlan"
	devTypeBridge    = "bridge"
	devTypeWireGuard = "wireguard"
)

// arphrdEther is ARPHRD_ETHER, the /sys/class/net/<if>/type of Ethernet-like
// links (wired, Wi-Fi, bridges, veth pairs).
const arphrdEther = 1

// parseProcNetWireless maps interface name to signal strength in dBm from
// /proc/net/wireless. The kernel prints level - 256 when the driver sets
// IW_QUAL_DBM, so a dBm level is already negative in the file. Rows whose
// level does not parse as a negative dBm value are left out.
func parseProcNetWireless(data []byte) map[string]int {
	out := map[string]int{}
	lines := strings.Split(string(data), "\n")
	if len(lines) <= 2 { // two header lines, then one row per interface
		return out
	}
	for _, line := range lines[2:] {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		// Columns after the name: status, link quality, level, noise, ...
		fields := strings.Fields(rest)
		if len(fields) < 3 {
			continue
		}
		level, err := strconv.ParseFloat(strings.TrimSuffix(fields[2], "."), 64)
		if err != nil || level < minSignalDBM || level > maxSignalDBM {
			continue
		}
		out[strings.TrimSpace(name)] = int(level)
	}
	return out
}

// iwFreqMHz converts a wireless-extensions frequency (m * 10^e Hz) to MHz. It
// reports false for a channel number (a driver that returns m=6 e=0 means
// channel 6, not 6 Hz) and for values outside 1..100000 MHz.
func iwFreqMHz(m int32, e int16) (int, bool) {
	const maxHz = int64(100_000) * 1_000_000
	if m <= 0 || e < 0 || e > 12 {
		return 0, false
	}
	hz := int64(m)
	for range e {
		hz *= 10
		if hz > maxHz {
			return 0, false
		}
	}
	mhz := hz / 1_000_000
	if mhz < 1 || mhz > 100_000 {
		return 0, false
	}
	return int(mhz), true
}

// cleanSSID turns the raw ESSID buffer into text safe to show: cut at the
// first NUL (brcmfmac counts a trailing NUL in the length), invalid UTF-8
// replaced, control and bidi-override characters dropped. Empty means the
// network name is unknown or hidden.
func cleanSSID(b []byte) string {
	b = b[:min(len(b), ssidMaxBytes)]
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	s := strings.ToValidUTF8(string(b), "�")
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r),
			r >= 0x202A && r <= 0x202E, // bidi embeddings and overrides
			r >= 0x2066 && r <= 0x2069: // bidi isolates
			return -1
		}
		return r
	}, s)
}

// ifaceFacts are the /sys/class/net/<name> facts classifyInterface decides on.
type ifaceFacts struct {
	wireless  bool   // wireless dir or phy80211 link present, or DEVTYPE=wlan
	devType   string // DEVTYPE from uevent, empty when absent
	arpType   int    // ARPHRD type from the "type" file
	hasDevice bool   // a "device" link: backed by a bus device
}

// classifyInterface names the kind of link. An Ethernet kind needs a bus
// device, which covers onboard and USB adapters and excludes docker0, veth
// and bridges.
func classifyInterface(f ifaceFacts) string {
	switch {
	case f.wireless:
		return kindWifi
	case f.arpType != arphrdEther:
		return kindOther
	}
	switch f.devType {
	case devTypeBridge, "vlan", "bond", "team", "macvlan", "vxlan", devTypeWireGuard, "veth", "dummy":
		return kindOther
	}
	if f.hasDevice {
		return kindEthernet
	}
	return kindOther
}
