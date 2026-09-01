package reload

import (
	"reflect"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
)

// dev builds an enabled device with the given name and hw/path/rate; other
// capture params take sensible defaults so tests can vary one field at a time.
func dev(name, hw, path string, rate int) config.Device {
	return config.Device{
		Name: name, Device: hw, Path: path,
		Mode: config.ModePCM, Rate: rate, Channels: []int{1}, Format: "s16",
	}
}

func disabled(d *config.Device) config.Device {
	off := false
	c := *d
	c.Enabled = &off
	return c
}

func cfg(devs ...config.Device) *config.Config {
	return &config.Config{Devices: devs}
}

func namesOf(devs []config.Device) []string {
	out := make([]string, len(devs))
	for i := range devs {
		out[i] = devs[i].Name
	}
	return out
}

func TestReconcileStartsNewDevice(t *testing.T) {
	running := map[string]config.Device{}
	p := Reconcile(running, cfg(dev("a", "hw:0", "/a", 48000)))
	if got := namesOf(p.Start); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("Start = %v, want [a]", got)
	}
	if len(p.Stop) != 0 || len(p.Restart) != 0 {
		t.Fatalf("unexpected Stop=%v Restart=%v", p.Stop, namesOf(p.Restart))
	}
}

func TestReconcileLeavesUnchangedDeviceRunning(t *testing.T) {
	d := dev("a", "hw:0", "/a", 48000)
	running := map[string]config.Device{"a": d}
	p := Reconcile(running, cfg(d))
	if !p.Empty() {
		t.Fatalf("unchanged device produced a plan: Start=%v Stop=%v Restart=%v",
			namesOf(p.Start), p.Stop, namesOf(p.Restart))
	}
}

func TestReconcileStopsRemovedDevice(t *testing.T) {
	running := map[string]config.Device{"a": dev("a", "hw:0", "/a", 48000)}
	p := Reconcile(running, cfg()) // desired has no devices
	if !reflect.DeepEqual(p.Stop, []string{"a"}) {
		t.Fatalf("Stop = %v, want [a]", p.Stop)
	}
	if len(p.Start) != 0 || len(p.Restart) != 0 {
		t.Fatalf("unexpected Start=%v Restart=%v", namesOf(p.Start), namesOf(p.Restart))
	}
}

func TestReconcileStopsDisabledDevice(t *testing.T) {
	d := dev("a", "hw:0", "/a", 48000)
	running := map[string]config.Device{"a": d}
	p := Reconcile(running, cfg(disabled(&d)))
	if !reflect.DeepEqual(p.Stop, []string{"a"}) {
		t.Fatalf("Stop = %v, want [a] for a disabled device", p.Stop)
	}
}

func TestReconcileStartsNewlyEnabledDevice(t *testing.T) {
	// A device that was disabled (never running) and is now enabled must start.
	d := dev("a", "hw:0", "/a", 48000)
	running := map[string]config.Device{}
	p := Reconcile(running, cfg(d))
	if got := namesOf(p.Start); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("Start = %v, want [a]", got)
	}
}

func TestReconcileRestartsOnRateChange(t *testing.T) {
	running := map[string]config.Device{"a": dev("a", "hw:0", "/a", 48000)}
	p := Reconcile(running, cfg(dev("a", "hw:0", "/a", 96000)))
	if got := namesOf(p.Restart); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("Restart = %v, want [a] on rate change", got)
	}
	if len(p.Start) != 0 || len(p.Stop) != 0 {
		t.Fatalf("rate change should be a Restart only, got Start=%v Stop=%v", namesOf(p.Start), p.Stop)
	}
}

func TestReconcileRestartsOnParamChanges(t *testing.T) {
	base := dev("a", "hw:0", "/a", 48000)
	cases := map[string]func(config.Device) config.Device{
		"hw":       func(d config.Device) config.Device { d.Device = "hw:1"; return d },
		"path":     func(d config.Device) config.Device { d.Path = "/b"; return d },
		"channels": func(d config.Device) config.Device { d.Channels = []int{2}; return d },
		"format":   func(d config.Device) config.Device { d.Format = "s32"; return d },
		"mode":     func(d config.Device) config.Device { d.Mode = config.ModeOpus; return d },
		"bitrate":  func(d config.Device) config.Device { d.Opus.Bitrate = 64000; return d },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			running := map[string]config.Device{"a": base}
			p := Reconcile(running, cfg(mutate(base)))
			if got := namesOf(p.Restart); !reflect.DeepEqual(got, []string{"a"}) {
				t.Fatalf("%s change: Restart = %v, want [a]", name, got)
			}
		})
	}
}

func TestReconcileRenameStopsOldStartsNew(t *testing.T) {
	// Name is the identity key, so a renamed device is a stop of the old plus a
	// start of the new, not a restart.
	running := map[string]config.Device{"old": dev("old", "hw:0", "/a", 48000)}
	p := Reconcile(running, cfg(dev("new", "hw:0", "/a", 48000)))
	if !reflect.DeepEqual(p.Stop, []string{"old"}) {
		t.Fatalf("Stop = %v, want [old]", p.Stop)
	}
	if got := namesOf(p.Start); !reflect.DeepEqual(got, []string{"new"}) {
		t.Fatalf("Start = %v, want [new]", got)
	}
}

func TestReconcileDeterministicOrder(t *testing.T) {
	// Map iteration is random; the plan must be sorted so execution order and
	// tests are stable.
	running := map[string]config.Device{
		"z": dev("z", "hw:0", "/z", 48000),
		"y": dev("y", "hw:1", "/y", 48000),
	}
	desired := cfg(
		dev("c", "hw:2", "/c", 48000),
		dev("a", "hw:3", "/a", 48000),
	)
	p := Reconcile(running, desired)
	if !reflect.DeepEqual(p.Stop, []string{"y", "z"}) {
		t.Fatalf("Stop = %v, want sorted [y z]", p.Stop)
	}
	if got := namesOf(p.Start); !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Fatalf("Start = %v, want sorted [a c]", got)
	}
}

func TestReconcileRestartListIsSorted(t *testing.T) {
	// Two devices change params at once; the Restart list must be sorted by name
	// so execution order is deterministic (exercises the Restart sort).
	running := map[string]config.Device{
		"z": dev("z", "hw:0", "/z", 48000),
		"a": dev("a", "hw:1", "/a", 48000),
	}
	desired := cfg(
		dev("z", "hw:0", "/z", 96000),
		dev("a", "hw:1", "/a", 96000),
	)
	p := Reconcile(running, desired)
	if got := namesOf(p.Restart); !reflect.DeepEqual(got, []string{"a", "z"}) {
		t.Fatalf("Restart = %v, want sorted [a z]", got)
	}
}

func TestReconcileMixedPlan(t *testing.T) {
	running := map[string]config.Device{
		"keep":   dev("keep", "hw:0", "/keep", 48000),
		"change": dev("change", "hw:1", "/change", 48000),
		"drop":   dev("drop", "hw:2", "/drop", 48000),
	}
	desired := cfg(
		dev("keep", "hw:0", "/keep", 48000),     // unchanged
		dev("change", "hw:1", "/change", 96000), // restart (rate)
		dev("add", "hw:3", "/add", 48000),       // start
		// "drop" absent -> stop
	)
	p := Reconcile(running, desired)
	if !reflect.DeepEqual(p.Stop, []string{"drop"}) {
		t.Fatalf("Stop = %v, want [drop]", p.Stop)
	}
	if got := namesOf(p.Start); !reflect.DeepEqual(got, []string{"add"}) {
		t.Fatalf("Start = %v, want [add]", got)
	}
	if got := namesOf(p.Restart); !reflect.DeepEqual(got, []string{"change"}) {
		t.Fatalf("Restart = %v, want [change]", got)
	}
}
