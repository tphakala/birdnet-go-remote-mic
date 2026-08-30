package sysinfo

import "testing"

func TestParseOSReleasePretty(t *testing.T) {
	data := []byte(`NAME="Debian GNU/Linux"
PRETTY_NAME="Debian GNU/Linux 13 (trixie)"
VERSION_ID="13"
`)
	if got := parseOSReleasePretty(data); got != "Debian GNU/Linux 13 (trixie)" {
		t.Errorf("got %q", got)
	}
	if got := parseOSReleasePretty([]byte("NAME=x\n")); got != "" {
		t.Errorf("missing PRETTY_NAME: got %q, want empty", got)
	}
}

func TestParseCPUModel(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"x86 model name", "processor\t: 0\nmodel name\t: AMD Ryzen 7\n", "AMD Ryzen 7"},
		{"arm Model", "processor\t: 0\nModel\t\t: Raspberry Pi Zero 2 W Rev 1.0\n", "Raspberry Pi Zero 2 W Rev 1.0"},
		{"arm Hardware fallback", "processor\t: 0\nHardware\t: BCM2835\n", "BCM2835"},
		{"model name wins over Model", "model name\t: Cortex\nModel\t: Pi\n", "Cortex"},
		{"none", "processor\t: 0\n", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseCPUModel([]byte(tc.in)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseMemInfo(t *testing.T) {
	data := []byte(`MemTotal:        4000000 kB
MemFree:         1000000 kB
MemAvailable:    2500000 kB
Buffers:          100000 kB
`)
	total, used, ok := parseMemInfo(data)
	if !ok {
		t.Fatal("ok = false")
	}
	if total != 4000000*1024 {
		t.Errorf("total = %d", total)
	}
	if used != (4000000-2500000)*1024 {
		t.Errorf("used = %d", used)
	}
	if _, _, ok := parseMemInfo([]byte("MemFree: 1 kB\n")); ok {
		t.Error("ok = true without MemTotal")
	}
}

func TestParseCPUStat(t *testing.T) {
	// user nice system idle iowait irq softirq steal
	data := []byte("cpu  100 0 50 800 40 0 10 0\ncpu0 50 0 25 400 20 0 5 0\n")
	idle, total, ok := parseCPUStat(data)
	if !ok {
		t.Fatal("ok = false")
	}
	if idle != 800+40 {
		t.Errorf("idle = %d, want 840", idle)
	}
	if total != 100+0+50+800+40+0+10+0 {
		t.Errorf("total = %d, want 1000", total)
	}
	if _, _, ok := parseCPUStat([]byte("intr 1 2 3\n")); ok {
		t.Error("ok = true without a cpu line")
	}
}

func TestCPUBusyPercent(t *testing.T) {
	// From total 1000 idle 900 to total 1200 idle 1000: dTotal 200, dIdle 100 -> 50%.
	pct, ok := cpuBusyPercent(900, 1000, 1000, 1200)
	if !ok {
		t.Fatal("ok = false")
	}
	if pct != 50 {
		t.Errorf("pct = %v, want 50", pct)
	}
	if _, ok := cpuBusyPercent(0, 1000, 0, 1000); ok {
		t.Error("ok = true with no elapsed time")
	}
	// Idle jumping more than total must clamp to 0, not go negative.
	if pct, _ := cpuBusyPercent(0, 0, 500, 100); pct != 0 {
		t.Errorf("clamped pct = %v, want 0", pct)
	}
	// A backward idle jump with forward total must drop the sample entirely.
	if _, ok := cpuBusyPercent(1000, 1000, 500, 2000); ok {
		t.Error("ok = true when the idle counter rolled back")
	}
}

func TestParseCPUStatExcludesGuest(t *testing.T) {
	// 10 fields: user nice system idle iowait irq softirq steal guest guest_nice.
	// guest(5)/guest_nice(3) must NOT be summed (kernel folds them into user/nice).
	data := []byte("cpu  100 0 50 800 40 0 10 0 5 3\n")
	idle, total, ok := parseCPUStat(data)
	if !ok {
		t.Fatal("ok = false")
	}
	if total != 1000 {
		t.Errorf("total = %d, want 1000 (guest/guest_nice excluded)", total)
	}
	if idle != 840 {
		t.Errorf("idle = %d, want 840", idle)
	}
}

func TestParseCPUStatShortAndNonNumeric(t *testing.T) {
	if _, _, ok := parseCPUStat([]byte("cpu 1 2 3\n")); ok {
		t.Error("ok = true for a short (<6 field) cpu line")
	}
	if _, _, ok := parseCPUStat([]byte("cpu 1 2 x 4 5 6\n")); ok {
		t.Error("ok = true for a non-numeric field")
	}
}

func TestParseMemInfoClampAndMissingAvail(t *testing.T) {
	// MemAvailable > MemTotal must clamp used to 0, not underflow.
	_, used, ok := parseMemInfo([]byte("MemTotal: 100 kB\nMemAvailable: 200 kB\n"))
	if !ok || used != 0 {
		t.Errorf("clamp: used = %d ok = %v, want 0 true", used, ok)
	}
	// Missing MemAvailable leaves used 0 but still reports total.
	total, used, ok := parseMemInfo([]byte("MemTotal: 4000 kB\n"))
	if !ok || total != 4000*1024 || used != 0 {
		t.Errorf("missing avail: total=%d used=%d ok=%v", total, used, ok)
	}
}

func TestSelectTemp(t *testing.T) {
	tests := []struct {
		name   string
		in     []tempCandidate
		want   float64
		wantOK bool
	}{
		{"empty", nil, 0, false},
		{"prefers cpu type", []tempCandidate{{"gpu-thermal", 30}, {"cpu-thermal", 55}}, 55, true},
		{"prefers soc type", []tempCandidate{{"other", 20}, {"soc", 48}}, 48, true},
		{"cpu case-insensitive", []tempCandidate{{"CPU-Thermal", 60}}, 60, true},
		{"falls back to first", []tempCandidate{{"battery", 25}, {"ambient", 22}}, 25, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := selectTemp(tc.in)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("selectTemp = %v, %v; want %v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestParseMilliCelsius(t *testing.T) {
	if c, ok := parseMilliCelsius([]byte("48123\n")); !ok || c != 48.123 {
		t.Errorf("got %v, %v; want 48.123, true", c, ok)
	}
	if _, ok := parseMilliCelsius([]byte("  \n")); ok {
		t.Error("ok = true for empty input")
	}
	if _, ok := parseMilliCelsius([]byte("warm")); ok {
		t.Error("ok = true for non-numeric input")
	}
}

func TestParseCounter(t *testing.T) {
	if got := parseCounter([]byte("123456\n")); got != 123456 {
		t.Errorf("got %d", got)
	}
	if got := parseCounter([]byte("bad")); got != 0 {
		t.Errorf("got %d, want 0 on parse failure", got)
	}
}
