//go:build linux

package sysinfo

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
)

// Wireless-extensions ioctls. cfg80211 answers them through its compatibility
// layer (CONFIG_CFG80211_WEXT), without privileges. A kernel built without it
// fails them, and the Wi-Fi facts are then simply absent.
const (
	siocgiwfreq  = 0x8B05
	siocgiwessid = 0x8B1B

	ifNameSize = 16 // IFNAMSIZ: the name plus its NUL
	iwreqSize  = 32 // struct iwreq: the name and a 16-byte union
	iwUnion    = iwreqSize - ifNameSize
	essidBuf   = ssidMaxBytes + 1 // IW_ESSID_MAX_SIZE plus a NUL
)

// iwreqEssid is struct iwreq carrying a struct iw_point. The pointer word is
// 8 bytes on LP64 and 4 on ILP32, the tail pad absorbs the difference.
type iwreqEssid struct {
	name   [ifNameSize]byte
	ptr    unsafe.Pointer
	length uint16
	flags  uint16
	_      [iwUnion - unsafe.Sizeof(uintptr(0)) - 4]byte
}

// iwreqFreq is struct iwreq carrying a struct iw_freq.
type iwreqFreq struct {
	name  [ifNameSize]byte
	m     int32
	e     int16
	i     uint8
	flags uint8
	_     [iwUnion - 8]byte
}

// Compile-time layout checks: a mistake fails the build on every GOARCH the
// gate covers instead of corrupting memory on the appliance.
var (
	_ [iwreqSize - unsafe.Sizeof(iwreqEssid{})]byte
	_ [unsafe.Sizeof(iwreqEssid{}) - iwreqSize]byte
	_ [iwreqSize - unsafe.Sizeof(iwreqFreq{})]byte
	_ [unsafe.Sizeof(iwreqFreq{}) - iwreqSize]byte
)

// procNetWireless is the kernel's wireless statistics file. Reading it makes
// cfg80211 query the driver for station info, so it is read only inside a
// request that needs it, never on a timer.
const procNetWireless = "/proc/net/wireless"

// Files under /sys/class/net/<name> that readIfaceFacts reads.
const (
	fileUevent = "uevent"
	fileType   = "type"
)

// readIfaceFacts reads the classification facts for one interface under base
// (production passes sysClassNet). arpType is -1 when the type file cannot be
// read.
func readIfaceFacts(base, name string) ifaceFacts {
	dir := filepath.Join(base, name)
	f := ifaceFacts{arpType: -1}
	exists := func(p string) bool {
		_, err := os.Lstat(filepath.Join(dir, p))
		return err == nil
	}
	f.hasDevice = exists("device")
	f.wireless = exists("wireless") || exists("phy80211")
	if b, err := os.ReadFile(filepath.Join(dir, fileUevent)); err == nil { //nolint:gosec // name from the kernel's own interface list
		for line := range strings.SplitSeq(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "DEVTYPE="); ok {
				f.devType = strings.TrimSpace(v)
				if f.devType == devTypeWLAN {
					f.wireless = true
				}
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, fileType)); err == nil { //nolint:gosec // name from the kernel's own interface list
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			f.arpType = n
		}
	}
	return f
}

// ioctlFunc issues one wireless-extensions ioctl on an open socket. The tests
// replace it, since no test host is guaranteed a Wi-Fi interface.
type ioctlFunc func(req uintptr, arg unsafe.Pointer) syscall.Errno

// openWext opens the datagram socket the wireless ioctls run on and returns
// the ioctl bound to it and its close.
func openWext() (ioc ioctlFunc, closeFn func(), err error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	ioc = func(req uintptr, arg unsafe.Pointer) syscall.Errno {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
		return errno
	}
	return ioc, func() { _ = syscall.Close(fd) }, nil
}

// readWifi gathers what it can for one wireless interface: signal from the
// /proc/net/wireless map, SSID and frequency by ioctl. Every failed read
// leaves its field out, and it returns nil when nothing is known. It never
// logs: /system is polled every few seconds while a page is open.
func readWifi(ioc ioctlFunc, name string, signal map[string]int) *mgmtserver.WifiLink {
	link := mgmtserver.WifiLink{}
	if dbm, ok := signal[name]; ok {
		link.SignalDBM = &dbm
	}
	if ssid, ok := wextESSID(ioc, name); ok {
		link.SSID = ssid
	}
	if mhz, ok := wextFreqMHz(ioc, name); ok {
		link.FrequencyMHz = &mhz
	}
	if link == (mgmtserver.WifiLink{}) {
		return nil
	}
	return &link
}

// wextESSID reads the network name; ok is false when the interface is not
// wireless, is not associated, or the name is empty or hidden.
func wextESSID(ioc ioctlFunc, name string) (string, bool) {
	var req iwreqEssid
	if !setIfName(&req.name, name) {
		return "", false
	}
	var buf [essidBuf]byte
	req.ptr = unsafe.Pointer(&buf[0]) //nolint:gosec // G103: ioctl argument layout, sizes asserted at compile time
	req.length = essidBuf
	errno := ioc(siocgiwessid, unsafe.Pointer(&req)) //nolint:gosec // G103: ioctl argument layout, sizes asserted at compile time
	runtime.KeepAlive(&buf)
	if errno != 0 {
		return "", false
	}
	n := min(int(req.length), len(buf))
	ssid := cleanSSID(buf[:n])
	return ssid, ssid != ""
}

// wextFreqMHz reads the current channel frequency.
func wextFreqMHz(ioc ioctlFunc, name string) (int, bool) {
	var req iwreqFreq
	if !setIfName(&req.name, name) {
		return 0, false
	}
	if errno := ioc(siocgiwfreq, unsafe.Pointer(&req)); errno != 0 { //nolint:gosec // G103: ioctl argument layout, sizes asserted at compile time
		return 0, false
	}
	return iwFreqMHz(req.m, req.e)
}

// setIfName copies name into an ifreq name field. A name that does not fit
// with its NUL is skipped rather than truncated, which could name another
// interface.
func setIfName(dst *[ifNameSize]byte, name string) bool {
	if len(name) >= ifNameSize {
		return false
	}
	copy(dst[:], name)
	return true
}

// addWifiLinks fills Wifi on every wireless interface that is up. It reads
// procPath (/proc/net/wireless) and opens one socket through open, and only
// when there is such an interface, so a host with no Wi-Fi pays nothing.
func addWifiLinks(ifaces []mgmtserver.NetworkInterface, procPath string, open func() (ioctlFunc, func(), error)) {
	var wanted []int
	for i := range ifaces {
		if ifaces[i].Kind == kindWifi && ifaces[i].Up {
			wanted = append(wanted, i)
		}
	}
	if len(wanted) == 0 {
		return
	}
	ioc, closeFn, err := open()
	if err != nil {
		return
	}
	defer closeFn()
	var signal map[string]int
	if b, err := os.ReadFile(procPath); err == nil { //nolint:gosec // procPath is the constant kernel file, or a test file
		signal = parseProcNetWireless(b)
	}
	for _, i := range wanted {
		ifaces[i].Wifi = readWifi(ioc, ifaces[i].Name, signal)
	}
}
