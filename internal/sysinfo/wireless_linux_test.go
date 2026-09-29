//go:build linux

package sysinfo

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"

	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
)

// fakeIface builds a /sys/class/net/<name> tree under base. files maps a
// relative path to content; a key ending in "/" makes an empty directory and a
// value starting with "->" makes a symlink.
func fakeIface(t *testing.T, base, name string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		switch {
		case rel[len(rel)-1] == '/':
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
		case len(content) > 2 && content[:2] == "->":
			if err := os.Symlink(content[2:], p); err != nil {
				t.Fatal(err)
			}
		default:
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestReadIfaceFacts(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	fakeIface(t, base, testWlan, map[string]string{
		"wireless/": "", "phy80211": "->../../ieee80211/phy0", "device": "->../../mmc",
		fileUevent: "DEVTYPE=wlan\nINTERFACE=wlan0\n", fileType: "1\n",
	})
	fakeIface(t, base, testEth, map[string]string{"device": "->../../platform", fileType: "1\n", fileUevent: "INTERFACE=eth0\n"})
	fakeIface(t, base, "docker0", map[string]string{fileUevent: "DEVTYPE=bridge\n", fileType: "1\n"})
	fakeIface(t, base, "wg0", map[string]string{fileUevent: "DEVTYPE=wireguard\n", fileType: "65534\n"})
	fakeIface(t, base, "odd", map[string]string{fileUevent: "DEVTYPE=wlan\n"})
	fakeIface(t, base, "bare", nil)

	tests := []struct {
		name     string
		want     ifaceFacts
		wantKind string
	}{
		{testWlan, ifaceFacts{wireless: true, devType: devTypeWLAN, arpType: 1, hasDevice: true}, kindWifi},
		{testEth, ifaceFacts{arpType: 1, hasDevice: true}, kindEthernet},
		{"docker0", ifaceFacts{devType: devTypeBridge, arpType: 1}, kindOther},
		{"wg0", ifaceFacts{devType: devTypeWireGuard, arpType: 65534}, kindOther},
		{"odd", ifaceFacts{wireless: true, devType: devTypeWLAN, arpType: -1}, kindWifi},
		{"bare", ifaceFacts{arpType: -1}, kindOther},
		{"missing", ifaceFacts{arpType: -1}, kindOther},
	}
	for _, tc := range tests {
		got := readIfaceFacts(base, tc.name)
		if got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
		if k := classifyInterface(got); k != tc.wantKind {
			t.Errorf("%s: kind %q, want %q", tc.name, k, tc.wantKind)
		}
	}
}

// TestReadWifiLoopback runs the real ioctls against a link that is not
// wireless: the answer is "nothing known", not an error, and needs no hardware.
func TestReadWifiLoopback(t *testing.T) {
	t.Parallel()
	ioc, closeFn, err := openWext()
	if err != nil {
		t.Skipf("no AF_INET socket: %v", err)
	}
	defer closeFn()
	if got := readWifi(ioc, "lo", nil); got != nil {
		t.Errorf("readWifi(lo) = %+v, want nil", got)
	}
	if got := readWifi(ioc, "an-interface-name-too-long", nil); got != nil {
		t.Errorf("readWifi(long name) = %+v, want nil", got)
	}
	// A signal from /proc/net/wireless alone is still a known fact.
	got := readWifi(ioc, "lo", map[string]int{"lo": -60})
	if got == nil || got.SignalDBM == nil || *got.SignalDBM != -60 || got.SSID != "" || got.FrequencyMHz != nil {
		t.Errorf("readWifi with signal only = %+v, want just the signal", got)
	}
}

// fakeIoctl answers the two wireless ioctls the way a driver does: an ESSID
// with a trailing NUL counted in the length (brcmfmac) and a frequency as
// m * 10^e Hz. A nil ssid or freq makes that ioctl fail like a non-wireless
// link.
func fakeIoctl(ssid []byte, m int32, e int16) ioctlFunc {
	return func(req uintptr, arg unsafe.Pointer) syscall.Errno {
		switch req {
		case siocgiwessid:
			if ssid == nil {
				return syscall.EOPNOTSUPP
			}
			r := (*iwreqEssid)(arg)
			buf := unsafe.Slice((*byte)(r.ptr), int(r.length)) //nolint:gosec // G103: the reader passes a buffer of exactly length bytes
			copy(buf, ssid)
			r.length = uint16(len(ssid)) //nolint:gosec // G115: test SSIDs are a few bytes
			r.flags = 1
		case siocgiwfreq:
			if m == 0 {
				return syscall.EOPNOTSUPP
			}
			r := (*iwreqFreq)(arg)
			r.m, r.e = m, e
		default:
			return syscall.EINVAL
		}
		return 0
	}
}

func TestReadWifiFromIoctls(t *testing.T) {
	t.Parallel()
	sig := map[string]int{testWlan: -52}
	got := readWifi(fakeIoctl([]byte(testSSID+"\x00"), 5220, 6), testWlan, sig)
	if got == nil || got.SSID != testSSID || got.SignalDBM == nil || *got.SignalDBM != -52 || got.FrequencyMHz == nil || *got.FrequencyMHz != 5220 {
		t.Fatalf("readWifi = %+v, want ASUS, -52 dBm, 5220 MHz", got)
	}
	// Hidden network: the name is all NULs, so it is left out; the rest stays.
	got = readWifi(fakeIoctl([]byte("\x00\x00"), 2437, 6), testWlan, nil)
	if got == nil || got.SSID != "" || got.FrequencyMHz == nil || *got.FrequencyMHz != 2437 {
		t.Errorf("hidden network: readWifi = %+v, want only the frequency", got)
	}
	// A driver that reports a channel number, not a frequency, adds nothing.
	if got := readWifi(fakeIoctl(nil, 6, 0), testWlan, nil); got != nil {
		t.Errorf("channel number: readWifi = %+v, want nil", got)
	}
	// A length past the buffer is clamped, never read beyond it.
	long := make([]byte, essidBuf)
	for i := range long {
		long[i] = 'a'
	}
	if got := readWifi(fakeIoctl(long, 0, 0), testWlan, nil); got == nil || len(got.SSID) != ssidMaxBytes {
		t.Errorf("long name: readWifi = %+v, want a %d-byte name", got, ssidMaxBytes)
	}
}

func TestAddWifiLinks(t *testing.T) {
	t.Parallel()
	proc := filepath.Join(t.TempDir(), "wireless")
	if err := os.WriteFile(proc, []byte(procNetWirelessHeader+" wlan0: 0000 58. -52. -256 0 0 0 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opened, closed := 0, 0
	open := func() (ioctlFunc, func(), error) {
		opened++
		return fakeIoctl([]byte(testSSID+"\x00"), 5220, 6), func() { closed++ }, nil
	}
	ifaces := []mgmtserver.NetworkInterface{
		{Name: testEth, Kind: kindEthernet, Up: true},
		{Name: testWlan, Kind: kindWifi, Up: true},
		{Name: testWlan1, Kind: kindWifi, Up: false},
	}
	addWifiLinks(ifaces, proc, open)
	if ifaces[0].Wifi != nil || ifaces[2].Wifi != nil {
		t.Errorf("wifi set on a wired or down link: %+v %+v", ifaces[0].Wifi, ifaces[2].Wifi)
	}
	w := ifaces[1].Wifi
	if w == nil || w.SSID != testSSID || w.SignalDBM == nil || *w.SignalDBM != -52 || w.FrequencyMHz == nil || *w.FrequencyMHz != 5220 {
		t.Errorf("wlan0 wifi = %+v, want ASUS, -52 dBm, 5220 MHz", w)
	}
	if opened != 1 || closed != 1 {
		t.Errorf("socket opened %d, closed %d times, want once each", opened, closed)
	}

	// No wireless link that is up: nothing is opened or read.
	opened = 0
	addWifiLinks([]mgmtserver.NetworkInterface{{Name: testEth, Kind: kindEthernet, Up: true}, {Name: testWlan1, Kind: kindWifi}}, proc, open)
	if opened != 0 {
		t.Errorf("socket opened %d times with no wireless link up, want 0", opened)
	}

	// A socket that cannot be opened, or a missing /proc file, leaves what is known.
	failing := func() (ioctlFunc, func(), error) { return nil, nil, syscall.EMFILE }
	up := []mgmtserver.NetworkInterface{{Name: testWlan, Kind: kindWifi, Up: true}}
	addWifiLinks(up, proc, failing)
	if up[0].Wifi != nil {
		t.Errorf("wifi = %+v after a failed socket open, want nil", up[0].Wifi)
	}
	addWifiLinks(up, filepath.Join(t.TempDir(), "missing"), open)
	if w := up[0].Wifi; w == nil || w.SignalDBM != nil || w.SSID != testSSID {
		t.Errorf("wifi = %+v without /proc/net/wireless, want SSID and frequency but no signal", w)
	}
}

func TestSetIfName(t *testing.T) {
	t.Parallel()
	var b [ifNameSize]byte
	if !setIfName(&b, testWlan) || string(b[:5]) != testWlan || b[5] != 0 {
		t.Errorf("wlan0 not copied: %q", b)
	}
	if setIfName(&b, "0123456789abcdef") {
		t.Error("a 16-byte name (no room for NUL) was accepted")
	}
	if !setIfName(&b, "0123456789abcde") {
		t.Error("a 15-byte name was refused")
	}
}
