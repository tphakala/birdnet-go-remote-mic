package sysinfo

import (
	"reflect"
	"testing"
)

// Names shared by the wireless tests, here and in wireless_linux_test.go.
const (
	testWlan  = "wlan0"
	testWlan1 = "wlan1"
	testEth   = "eth0"
	testSSID  = "ASUS"
)

const procNetWirelessHeader = `Inter-| sta-|   Quality        |   Discarded packets               | Missed | WE
 face | tus | link level noise |  nwid  crypt   frag  retry   misc | beacon | 22
`

func TestParseProcNetWireless(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want map[string]int
	}{
		{"pi row", procNetWirelessHeader + " " + testWlan + ": 0000   58.  -52.  -256        0      0      0    251      0        0\n", map[string]int{testWlan: -52}},
		{"header only", procNetWirelessHeader, map[string]int{}},
		{"no data", "", map[string]int{}},
		{"non-dBm driver", procNetWirelessHeader + " " + testWlan + ": 0000   58.   200.  0   0 0 0 0 0 0\n", map[string]int{}},
		{"zero level", procNetWirelessHeader + " " + testWlan + ": 0000   58.   0.  0   0 0 0 0 0 0\n", map[string]int{}},
		{"malformed and short", procNetWirelessHeader + "garbage\n wlan0: 0000 58.\n wlan1: 0000 58. abc. 0\n", map[string]int{}},
		{"two interfaces", procNetWirelessHeader + " " + testWlan + ": 0000 58. -52. -256 0 0 0 0 0 0\n wlan1: 0000 30. -80 -256 0 0 0 0 0 0\n", map[string]int{testWlan: -52, testWlan1: -80}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := parseProcNetWireless([]byte(tc.in)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIwFreqMHz(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		m    int32
		e    int16
		want int
		ok   bool
	}{
		{"5 GHz", 5220, 6, 5220, true},
		{"2.4 GHz", 2437, 6, 2437, true},
		{"mantissa in tens of Hz", 241200000, 1, 2412, true},
		{"channel number", 6, 0, 0, false},
		{"zero", 0, 6, 0, false},
		{"negative mantissa", -5220, 6, 0, false},
		{"negative exponent", 5220, -1, 0, false},
		{"exponent overflow", 5220, 30, 0, false},
		{"too large", 2000000000, 6, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := iwFreqMHz(tc.m, tc.e)
			if got != tc.want || ok != tc.ok {
				t.Errorf("got (%d, %v), want (%d, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestCleanSSID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"plain", []byte("garden-ap"), "garden-ap"},
		{"trailing NUL", []byte(testSSID + "\x00"), testSSID},
		{"hidden", []byte("\x00\x00\x00"), ""},
		{"no bytes", nil, ""},
		{"invalid UTF-8", []byte("a\xffb"), "a�b"},
		{"control and bidi", []byte("a\x1b[31m\u202Eb\u2066c"), "a[31mbc"},
		{"bidi marks and separators", []byte("a\u200Eb\u200Fc\u061Cd\u2028e\u2029f"), "abcdef"},
		{"joiners are kept", []byte("a\u200Db\u200Cc"), "a\u200Db\u200Cc"},
		{"utf-8", []byte("Kämpe 🐦"), "Kämpe 🐦"},
		{"capped at 32 bytes", []byte("0123456789012345678901234567890123456789"), "01234567890123456789012345678901"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := cleanSSID(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyInterface(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   ifaceFacts
		want string
	}{
		{"wifi by wireless dir", ifaceFacts{wireless: true, arpType: 1, hasDevice: true}, kindWifi},
		{"wifi wins over type", ifaceFacts{wireless: true, arpType: 801}, kindWifi},
		{"onboard ethernet", ifaceFacts{arpType: 1, hasDevice: true}, kindEthernet},
		{"usb ethernet with devtype", ifaceFacts{arpType: 1, hasDevice: true, devType: ""}, kindEthernet},
		{"docker0 bridge", ifaceFacts{arpType: 1, devType: devTypeBridge}, kindOther},
		{"bridge with device link", ifaceFacts{arpType: 1, hasDevice: true, devType: devTypeBridge}, kindOther},
		{"veth without device", ifaceFacts{arpType: 1}, kindOther},
		{"vlan", ifaceFacts{arpType: 1, devType: "vlan"}, kindOther},
		{devTypeWireGuard, ifaceFacts{arpType: 65534, devType: devTypeWireGuard}, kindOther},
		{"tunnel", ifaceFacts{arpType: 65534}, kindOther},
		{"zero value", ifaceFacts{}, kindOther},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyInterface(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
