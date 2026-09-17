package monitor

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// hostPollInterval is how often the host monitor reads the host and the device
// drop counters. Host health moves slowly; ten seconds keeps the Pi Zero cost
// negligible while every onset dwell below still spans several readings.
const hostPollInterval = 10 * time.Second

// Onset and clear dwell times for the host conditions. Only the value thresholds
// are configurable; the dwells are constants so alert timing stays predictable.
const (
	cpuEnterAfter   = 60 * time.Second
	cpuClearAfter   = 60 * time.Second
	memEnterAfter   = 60 * time.Second
	memClearAfter   = 60 * time.Second
	tempEnterAfter  = 30 * time.Second
	tempClearAfter  = 60 * time.Second
	diskEnterAfter  = 60 * time.Second
	diskClearAfter  = 60 * time.Second
	voltEnterAfter  = 10 * time.Second
	voltClearAfter  = 60 * time.Second
	dropsEnterAfter = 30 * time.Second
	dropsClearAfter = 60 * time.Second

	// memClearGapDivisor sets the low-memory clear gap: available memory must
	// climb a quarter above the onset limit before the condition clears, so a host
	// hovering at the limit does not chatter. The gap is relative to the limit
	// (which follows whichever floor won, the percentage or the MiB floor) rather
	// than a fixed number of percentage points, so it stays proportionate on a small
	// host where the MiB floor binds and does not demand hundreds of MiB of recovery
	// on a large one. The clear level is capped at halfway between the limit and
	// total, because a high MemFreePercent can push limit*1.25 past total, where
	// available memory can never reach it and the warning would never clear.
	memClearGapDivisor = 4
	// dropsPerSecond is the dropped-frame rate above which a device's client is
	// judged not to be keeping up.
	dropsPerSecond = 1.0
)

// Host condition keys. There is one host, so the keys carry no subject.
const (
	hostCPUKey  = "system:cpu"
	hostMemKey  = "system:mem"
	hostTempKey = "system:temp"
	hostDiskKey = "system:disk"
	hostVoltKey = "system:volt"
)

func streamDropsKey(name string) string { return "stream:" + name + ":drops" }

// HostReader supplies the host readings the monitor judges. Each method reports
// ok=false when its figure is unavailable (no thermal zone, no rpi_volt hwmon, a
// failed read); a zero total from Mem or Disk is likewise treated as unavailable
// rather than dividing by it. The monitor then skips that condition for the
// reading, so an absent sensor never raises and a transient read failure never
// clears; an active condition whose reading stays unavailable for the clear dwell
// is resolved, so a sensor that vanishes for good cannot pin a stale alert. The
// interface lives here rather than in sysinfo because sysinfo imports the
// management server; cmd adapts the sysinfo functions to it.
type HostReader interface {
	// Mem returns total and available memory in bytes.
	Mem() (total, avail int64, ok bool)
	// Temp returns the SoC/CPU temperature in Celsius.
	Temp() (celsius float64, ok bool)
	// Disk returns total and used bytes of the appliance's data filesystem.
	Disk() (total, used int64, ok bool)
	// CPU returns the host CPU utilization percentage.
	CPU() (percent float64, ok bool)
	// Undervoltage reports whether the supply is undervolted right now.
	Undervoltage() (now, ok bool)
}

// DeviceDrops is one device's cumulative dropped-frame counter as the host
// monitor polls it, tagged with the identity (Gen) of the runtime the counter
// belongs to. A restart hands the device a fresh runtime whose counter starts at
// zero and a new Gen; the monitor rebaselines when Gen changes, so it never
// reports a negative rate and never under-reports when the fresh counter has
// already climbed past the old value between two polls. Gen zero (the source did
// not supply one) disables the Gen check and leaves the counter-went-backwards
// heuristic as the sole restart signal.
type DeviceDrops struct {
	Name    string
	Gen     uint64
	Dropped uint64
}

// DropSource returns the current per-device dropped-frame counters. A device
// absent from the result is treated as stopped: the appliance lists only serving
// devices, so a device that stops serving (disabled, failed, or removed) drops out
// and its drops condition is resolved after the presence grace.
type DropSource func() []DeviceDrops

// hostCond is one host condition's state: its hysteresis machine, the time its
// reading was last available, and its clear dwell (which also bounds how long an
// active condition survives with its sensor gone).
type hostCond struct {
	key        string
	h          *notify.Hysteresis
	clearAfter time.Duration
	lastOK     time.Time
}

func newHostCond(key string, enterAfter, clearAfter time.Duration) *hostCond {
	return &hostCond{key: key, h: notify.NewHysteresis(enterAfter, clearAfter), clearAfter: clearAfter}
}

// dropState is one device's dropped-frame rate state. gen is the identity of the
// runtime prev belongs to, so a restart (a new gen) rebaselines instead of
// diffing two runtimes' counters.
type dropState struct {
	h      *notify.Hysteresis
	prev   uint64
	prevAt time.Time
	gen    uint64
	missed int
	seen   bool
}

// Host is the host-health condition monitor. A single goroutine polls the
// HostReader and the DropSource every hostPollInterval and raises CPU, memory,
// temperature, disk, undervoltage, and per-device dropped-frame conditions with
// hysteresis on both the value and the duration. Settings are swapped atomically
// by Apply; the poll goroutine reads them each tick and performs every state
// change itself, so Apply never races the poll.
type Host struct {
	pub    notify.Publisher
	clock  func() time.Time
	reader HostReader
	drops  DropSource
	set    atomic.Pointer[Settings]

	// Owned by the poll goroutine.
	applied                  *Settings
	cpu, mem, temp, disk, vt *hostCond
	devs                     map[string]*dropState
}

// Host implements the appliance's Monitors handle.
var _ Monitors = (*Host)(nil)

// HostOption configures a Host.
type HostOption func(*Host)

// WithHostClock injects the time source the host monitor evaluates hysteresis
// against, so a test can drive a deterministic clock. A nil clock is ignored.
func WithHostClock(fn func() time.Time) HostOption {
	return func(h *Host) {
		if fn != nil {
			h.clock = fn
		}
	}
}

// NewHost builds a host monitor publishing to center. A nil reader or drops
// source disables that half of the monitor. A nil center becomes a typed-nil
// *notify.Center (a no-op Publisher), matching NewSignal. Call poll on a ticker
// (RunHost does this) to drive it.
func NewHost(reader HostReader, drops DropSource, center notify.Publisher, s *Settings, opts ...HostOption) *Host {
	if center == nil {
		center = (*notify.Center)(nil)
	}
	h := &Host{
		pub:    center,
		clock:  time.Now,
		reader: reader,
		drops:  drops,
		cpu:    newHostCond(hostCPUKey, cpuEnterAfter, cpuClearAfter),
		mem:    newHostCond(hostMemKey, memEnterAfter, memClearAfter),
		temp:   newHostCond(hostTempKey, tempEnterAfter, tempClearAfter),
		disk:   newHostCond(hostDiskKey, diskEnterAfter, diskClearAfter),
		vt:     newHostCond(hostVoltKey, voltEnterAfter, voltClearAfter),
		devs:   map[string]*dropState{},
	}
	for _, o := range opts {
		o(h)
	}
	cp := *s
	h.set.Store(&cp)
	return h
}

// Apply swaps the settings the monitor evaluates against. The poll goroutine
// picks the change up on its next tick.
func (h *Host) Apply(set *Settings) {
	cp := *set
	h.set.Store(&cp)
}

// RunHost builds the monitor and polls it every hostPollInterval until ctx is
// done. It returns the monitor so the caller can hand it to the appliance as one
// of its Monitors.
func RunHost(ctx context.Context, reader HostReader, drops DropSource, center notify.Publisher, s *Settings) *Host {
	h := NewHost(reader, drops, center, s)
	go func() {
		t := time.NewTicker(hostPollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				h.poll()
			}
		}
	}()
	return h
}

// poll evaluates one tick. It runs on the RunHost goroutine.
func (h *Host) poll() {
	now := h.clock()
	set := h.set.Load()
	prev := h.applied
	// Unlike Signal.reconcile (which the hub runs as a tap that levels.deliverTap
	// recovers, so a mid-resolve publisher panic is retried on the next window),
	// poll runs on RunHost's own ticker goroutine with no panic-recovering wrapper:
	// a publisher panic below is fatal, not retried. So there is no retry to skip,
	// and the order of this assignment relative to resolveAll does not matter here.
	h.applied = set
	if !set.Enabled {
		if prev == nil || prev.Enabled {
			h.resolveAll("notifications disabled")
		}
		// Disabled: skip both readers, so a disabled monitor reads nothing.
		return
	}
	if h.reader != nil {
		h.evaluateHost(now, set)
	}
	if h.drops != nil {
		h.evaluateDrops(now)
	}
}

// resolveAll clears every active condition this monitor owns and resets state,
// so a re-enable starts fresh (drop counters rebaseline on the next poll).
func (h *Host) resolveAll(reason string) {
	for _, c := range [...]*hostCond{h.cpu, h.mem, h.temp, h.disk, h.vt} {
		if c.h.Active() {
			h.pub.Resolve(c.key, reason)
		}
		c.h.Reset()
		c.lastOK = time.Time{}
	}
	for name, st := range h.devs {
		if st.h.Active() {
			h.pub.Resolve(streamDropsKey(name), reason)
		}
	}
	clear(h.devs)
}

// overWithGap is the value side of a host condition's hysteresis, shared by the
// CPU, temperature, and disk conditions. While inactive a reading counts as
// "over" at or above the onset threshold; while active it stays over until it
// falls to or below the lower clear threshold. The >= / > asymmetry gives the
// value a gap on top of the duration dwell, so a reading sitting between clear
// and onset holds an active condition without raising a fresh one.
func overWithGap(active bool, value, onsetLevel, clearLevel float64) bool {
	if active {
		return value > clearLevel
	}
	return value >= onsetLevel
}

// evaluateHost drives the five host conditions for this tick. Each condition
// judges "over" against its onset threshold while inactive and against its
// (lower) clear threshold while active, so the value has a hysteresis gap on top
// of the Hysteresis duration dwell.
func (h *Host) evaluateHost(now time.Time, set *Settings) {
	hs := &set.Host

	pct, ok := h.reader.CPU()
	h.cpu.observe(h, now, ok,
		func() bool {
			return overWithGap(h.cpu.h.Active(), pct, float64(intVal(hs.CPUPercent)), float64(intVal(hs.CPUClearPercent)))
		},
		func() notify.Notification { return cpuOnset(intVal(hs.CPUPercent)) },
		func() notify.Notification {
			return conditionClear("CPU load back to normal", fmt.Sprintf("CPU usage is back at or below %d%%", intVal(hs.CPUClearPercent)))
		})

	total, avail, ok := h.reader.Mem()
	h.mem.observe(h, now, ok && total > 0,
		func() bool {
			// One onset limit: the larger of the percentage and absolute floors, so
			// the stricter one wins on every memory size.
			limit := max(total/100*int64(intVal(hs.MemFreePercent)), int64(intVal(hs.MemFreeMiB))<<20)
			if h.mem.h.Active() {
				// Cap the clear level so it stays reachable: halfway between the limit
				// and total, since available memory can never exceed total.
				clearLimit := limit + limit/memClearGapDivisor
				clearLimit = min(clearLimit, limit+(total-limit)/2)
				return avail < clearLimit
			}
			return avail < limit
		},
		func() notify.Notification { return memOnset(avail, total) },
		func() notify.Notification {
			return conditionClear("Memory recovered", "Available memory is back above the low-memory threshold")
		})

	c, ok := h.reader.Temp()
	h.temp.observe(h, now, ok,
		func() bool {
			return overWithGap(h.temp.h.Active(), c, float64(intVal(hs.TempCelsius)), float64(intVal(hs.TempClearCelsius)))
		},
		func() notify.Notification { return tempOnset(c, intVal(hs.TempCelsius)) },
		func() notify.Notification {
			return conditionClear("Temperature back to normal", fmt.Sprintf("SoC temperature is back at or below %d C", intVal(hs.TempClearCelsius)))
		})

	// usedPct is computed once here (only when the reading is valid, so a zero
	// total never divides) and shared by the over and onset closures.
	dTotal, used, ok := h.reader.Disk()
	diskOK := ok && dTotal > 0
	var usedPct float64
	if diskOK {
		usedPct = float64(used) * 100 / float64(dTotal)
	}
	h.disk.observe(h, now, diskOK,
		func() bool {
			return overWithGap(h.disk.h.Active(), usedPct, float64(intVal(hs.DiskPercent)), float64(intVal(hs.DiskClearPercent)))
		},
		func() notify.Notification { return diskOnset(usedPct) },
		func() notify.Notification {
			return conditionClear("Disk space recovered", fmt.Sprintf("Disk usage is back at or below %d%%", intVal(hs.DiskClearPercent)))
		})

	uv, ok := h.reader.Undervoltage()
	h.vt.observe(h, now, ok,
		func() bool { return uv },
		voltOnset,
		func() notify.Notification {
			return conditionClear("Power supply recovered", fmt.Sprintf("No undervoltage detected for %s", humanDuration(int(voltClearAfter/time.Second))))
		})
}

// observe feeds one reading to the condition. For an active condition an
// unavailable reading (ok=false) is skipped, so a transient read failure never
// clears; but an active condition whose sensor stays unavailable for the whole
// clear dwell is resolved, so a sensor that vanishes for good cannot pin a stale
// alert. While inactive, an unavailable reading abandons any pending onset run,
// so an onset always needs a contiguous run of available over readings and an
// absent sensor never raises. over is evaluated only for an available reading.
func (c *hostCond) observe(h *Host, now time.Time, ok bool, over func() bool, mkOnset, mkClear func() notify.Notification) {
	if !ok {
		if c.h.Active() {
			if !c.lastOK.IsZero() && now.Sub(c.lastOK) >= c.clearAfter {
				h.pub.Resolve(c.key, "sensor reading unavailable")
				c.h.Reset()
			}
			return
		}
		// Inactive: abandon any pending onset run so a gap in the readings cannot
		// let an onset complete from readings taken minutes apart.
		c.h.Reset()
		return
	}
	c.lastOK = now
	h.transition(c.h.Observe(now, over()), c.key, mkOnset, mkClear)
}

// transition publishes the onset or clear a Hysteresis reported. Both onset and
// clear are built lazily, so the steady state (no transition) formats no message
// and a poll that does not transition allocates none.
func (h *Host) transition(tr notify.Transition, key string, mkOnset, mkClear func() notify.Notification) {
	switch tr {
	case notify.TransitionOnset:
		h.pub.Onset(mkOnset())
	case notify.TransitionClear:
		h.pub.Clear(key, mkClear())
	case notify.TransitionNone:
	}
}

// evaluateDrops turns each device's cumulative dropped-frame counter into a rate
// over the poll interval and drives its condition. The onset needs the rate over
// dropsPerSecond; once active, any drop at all holds the condition, so it clears
// only after dropsClearAfter with no drops.
func (h *Host) evaluateDrops(now time.Time) {
	for _, st := range h.devs {
		st.seen = false
	}
	for _, d := range h.drops() {
		st := h.devs[d.Name]
		if st == nil {
			// First sighting: baseline only, since a cumulative counter says nothing
			// about the current rate.
			h.devs[d.Name] = &dropState{
				h:    notify.NewHysteresis(dropsEnterAfter, dropsClearAfter),
				prev: d.Dropped, prevAt: now, gen: d.Gen, seen: true,
			}
			continue
		}
		st.seen, st.missed = true, 0
		if d.Gen != st.gen || d.Dropped < st.prev {
			// A fresh runtime: either its identity changed (a restart the counter did
			// not have to reveal, e.g. the new counter already passed the old value
			// between polls) or, absent a Gen, the counter went backwards. Rebaseline
			// prev AND gen, then observe no drops for this poll: a fresh runtime has no
			// evidence of drops yet, so an active condition's clear run keeps
			// advancing and a pending onset run is abandoned, instead of skipping the
			// observation and letting a run survive the restart. Rebaselining gen here
			// is essential: without it a restarted device would rebaseline on every
			// subsequent poll and its drop condition would never fire again.
			st.prev, st.prevAt, st.gen = d.Dropped, now, d.Gen
			name := d.Name
			h.transition(st.h.Observe(now, false), streamDropsKey(name),
				func() notify.Notification { return dropsOnset(name, 0) },
				func() notify.Notification { return dropsClearFor(name) })
			continue
		}
		delta := d.Dropped - st.prev
		elapsed := now.Sub(st.prevAt).Seconds()
		st.prev, st.prevAt = d.Dropped, now
		if elapsed <= 0 {
			continue
		}
		rate := float64(delta) / elapsed
		over := rate > dropsPerSecond
		if st.h.Active() {
			over = delta > 0
		}
		name := d.Name
		h.transition(st.h.Observe(now, over), streamDropsKey(name),
			func() notify.Notification { return dropsOnset(name, rate) },
			func() notify.Notification { return dropsClearFor(name) })
	}
	for name, st := range h.devs {
		if st.seen {
			continue
		}
		st.missed++
		if st.missed < devicePresenceGrace {
			// Absent but within the presence grace: abandon a pending onset run so a
			// blip cannot carry it across the gap, but leave an active condition
			// untouched. A device that stopped serving must end with the grace
			// resolve ("device stopped"), never with a clear claiming its client
			// caught up.
			if !st.h.Active() {
				st.h.Reset()
			}
			continue
		}
		if st.h.Active() {
			h.pub.Resolve(streamDropsKey(name), "device stopped")
		}
		delete(h.devs, name)
	}
}

func hostOnset(key, title, msg string, sev notify.Severity) notify.Notification {
	return notify.Notification{Severity: sev, Category: notify.CategorySystem, Key: key, Source: "host", Title: title, Message: msg}
}

func cpuOnset(threshold int) notify.Notification {
	return hostOnset(hostCPUKey, "High CPU load", fmt.Sprintf("CPU usage has read at or above %d%% at every check for %s", threshold, humanDuration(int(cpuEnterAfter/time.Second))), notify.SeverityWarning)
}

func memOnset(avail, total int64) notify.Notification {
	pct := float64(avail) * 100 / float64(total)
	return hostOnset(hostMemKey, "Low memory", fmt.Sprintf("Available memory is low: %.0f%% (%d MiB) free", pct, avail>>20), notify.SeverityWarning)
}

func tempOnset(c float64, threshold int) notify.Notification {
	return hostOnset(hostTempKey, "High temperature", fmt.Sprintf("SoC temperature is %.0f C, at or above the %d C threshold (check cooling)", c, threshold), notify.SeverityWarning)
}

func diskOnset(pct float64) notify.Notification {
	return hostOnset(hostDiskKey, "Disk almost full", fmt.Sprintf("Disk is %.0f%% full", pct), notify.SeverityWarning)
}

func voltOnset() notify.Notification {
	return hostOnset(hostVoltKey, "Undervoltage", "Undervoltage detected: check the power supply", notify.SeverityError)
}

func dropsOnset(name string, rate float64) notify.Notification {
	return notify.Notification{
		Severity: notify.SeverityWarning,
		Category: notify.CategoryStream,
		Key:      streamDropsKey(name),
		Source:   name,
		Title:    "Client not keeping up",
		Message:  fmt.Sprintf("Client not keeping up: %s is dropping %.1f frames/s", name, rate),
	}
}

// dropsClearFor builds the clear body for a device's dropped-frame condition,
// shared by the counter-restart and normal-rate paths in evaluateDrops. It is
// symmetric with dropsOnset.
func dropsClearFor(name string) notify.Notification {
	return conditionClear("Client keeping up", name+" is no longer dropping frames")
}
