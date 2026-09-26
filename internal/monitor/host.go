package monitor

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// hostPollInterval is how often the host monitor reads the host and the device
// counters. Host health moves slowly; ten seconds keeps the Pi Zero cost
// negligible while every onset dwell below still spans several readings (the
// overrun condition counts events instead, so a burst can raise it in one).
const hostPollInterval = 10 * time.Second

// Onset and clear dwell times and fixed thresholds for the host and device
// counter conditions. Only the host value thresholds are configurable; these are
// constants so alert timing stays predictable.
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

	// Capture overruns are sporadic events rather than a steady rate, so their
	// condition counts them over a sliding window: overrunOnsetCount within
	// overrunWindow raises it, and overrunClearAfter with none clears it. An
	// isolated overrun (a passing scheduling hiccup, say) is only logged; a
	// recurring pattern means the host is too busy or the USB link is unstable.
	overrunWindow     = 5 * time.Minute
	overrunOnsetCount = 5
	overrunClearAfter = 5 * time.Minute
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

func deviceOverrunsKey(name string) string { return "device:" + name + ":overruns" }

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
	// Disk returns total, used, and available bytes of the appliance's data
	// filesystem. The condition judges used/(used+avail) (df's Use%), so avail
	// excludes root-reserved blocks; total is carried only for the zero-total
	// unavailable guard.
	Disk() (total, used, avail int64, ok bool)
	// CPU returns the host CPU utilization percentage.
	CPU() (percent float64, ok bool)
	// Undervoltage reports whether the supply is undervolted right now.
	Undervoltage() (now, ok bool)
}

// DeviceCounters is one device's cumulative loss counters as the host monitor
// polls them, tagged with the identity (Gen) of the runtime the counters belong
// to. A restart hands the device a fresh runtime whose counters start at zero
// and a new Gen; the monitor treats a Gen change as a restart even when a fresh
// counter has already climbed past the old value between two polls, so it never
// diffs two runtimes' counters: drops take the restart poll as a new baseline
// and overruns count the fresh total. Gen zero (the source did not supply one)
// disables the Gen check and leaves a counter going backwards as the sole
// restart signal.
type DeviceCounters struct {
	Name string
	Gen  uint64
	// Dropped counts the audio frames the device's streams dropped because a
	// client or encoder was not keeping up.
	Dropped uint64
	// Overruns counts the capture overruns (ALSA xruns, which include a
	// recovered system suspend) the device's capture recovered from; each one
	// lost audio before any stream saw it.
	Overruns uint64
}

// CounterSource returns the current per-device counters. A device absent from
// the result is treated as stopped: the appliance lists only serving devices, so
// a device that stops serving (disabled, failed, or removed) drops out and its
// counter conditions are resolved after the presence grace.
type CounterSource func() []DeviceCounters

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

// counterState is one device's counter-condition state: the dropped-frame rate
// and the capture-overrun window. gen is the identity of the runtime prev and
// ovPrev belong to, so a restart (a new gen) rebaselines instead of diffing two
// runtimes' counters.
type counterState struct {
	h      *notify.Hysteresis
	prev   uint64
	prevAt time.Time
	gen    uint64
	missed int
	seen   bool

	// ovPrev is the last overrun count seen; ov counts the overruns since then
	// into the sliding window and holds whether the condition is raised.
	// ovLogAt is when the last overrun line short of the onset was written (zero
	// before the first, and again after an onset), and ovUnlogged how many
	// overruns have not been reported in a line since. ovEpisode counts the
	// overruns since the warning was raised, starting with the poll that raised
	// it, for the line that ends the warning.
	ovPrev     uint64
	ov         *notify.Flap
	ovLogAt    time.Time
	ovUnlogged uint64
	ovEpisode  uint64
}

// Host is the host-health condition monitor. A single goroutine polls the
// HostReader and the CounterSource every hostPollInterval and raises CPU,
// memory, temperature, disk, undervoltage, and per-device dropped-frame
// conditions with hysteresis on both the value and the duration, and a
// per-device capture-overrun condition counted over a sliding window. Settings
// are swapped atomically by Apply; the poll goroutine reads them each tick and
// performs every state change itself, so Apply never races the poll.
type Host struct {
	pub      notify.Publisher
	clock    func() time.Time
	logf     func(format string, args ...any)
	reader   HostReader
	counters CounterSource
	set      atomic.Pointer[Settings]

	// Owned by the poll goroutine.
	applied                  *Settings
	cpu, mem, temp, disk, vt *hostCond
	devs                     map[string]*counterState
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

// WithHostLogf injects the function the host monitor writes its log lines
// through (log.Printf by default), so a test can capture them without swapping
// the global logger. A nil function is ignored.
func WithHostLogf(fn func(format string, args ...any)) HostOption {
	return func(h *Host) {
		if fn != nil {
			h.logf = fn
		}
	}
}

// NewHost builds a host monitor publishing to center. A nil reader or counter
// source disables that half of the monitor. A nil center becomes a typed-nil
// *notify.Center (a no-op Publisher), matching NewSignal. Call poll on a ticker
// (RunHost does this) to drive it.
func NewHost(reader HostReader, counters CounterSource, center notify.Publisher, s *Settings, opts ...HostOption) *Host {
	if center == nil {
		center = (*notify.Center)(nil)
	}
	h := &Host{
		pub:      center,
		clock:    time.Now,
		logf:     log.Printf,
		reader:   reader,
		counters: counters,
		cpu:      newHostCond(hostCPUKey, cpuEnterAfter, cpuClearAfter),
		mem:      newHostCond(hostMemKey, memEnterAfter, memClearAfter),
		temp:     newHostCond(hostTempKey, tempEnterAfter, tempClearAfter),
		disk:     newHostCond(hostDiskKey, diskEnterAfter, diskClearAfter),
		vt:       newHostCond(hostVoltKey, voltEnterAfter, voltClearAfter),
		devs:     map[string]*counterState{},
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
func RunHost(ctx context.Context, reader HostReader, counters CounterSource, center notify.Publisher, s *Settings) *Host {
	h := NewHost(reader, counters, center, s)
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
	if h.counters != nil {
		h.evaluateCounters(now)
	}
}

// resolveAll clears every active condition this monitor owns and resets state,
// so a re-enable starts fresh (device counters rebaseline on the next poll).
// Each device's overrun state writes the line it owes first (endOverruns).
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
		h.endOverruns(st, name, reason)
		if st.ov.Active() {
			h.pub.Resolve(deviceOverrunsKey(name), reason)
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
			// the stricter one wins on every memory size. The MiB floor is clamped to
			// half of RAM: a mem_free_mib set above (or near) what the host physically
			// has would otherwise put the limit at or above total, where available
			// memory can never fall below it to clear, pinning a permanent low-memory
			// warning whose "% free" message contradicts itself. Config validation
			// cannot know the host's RAM size, so the monitor bounds it here. The
			// percentage floor needs no clamp: it is at most total (at 100%), and the
			// clear-level cap below already keeps a high percentage's warning clearable.
			mibFloor := min(int64(intVal(hs.MemFreeMiB))<<20, total/2)
			limit := max(total/100*int64(intVal(hs.MemFreePercent)), mibFloor)
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

	// usedPct is computed once here (only when the reading is valid, so an empty
	// filesystem never divides) and shared by the over and onset closures. The
	// percentage is used/(used+avail), matching df's Use%: avail excludes the
	// root-reserved blocks, so the warning tracks the space the unprivileged
	// service can actually write rather than the raw filesystem size. total is
	// only the unavailable guard (a zero total means the statfs figure is absent).
	dTotal, used, avail, ok := h.reader.Disk()
	usable := used + avail
	diskOK := ok && dTotal > 0 && usable > 0
	var usedPct float64
	if diskOK {
		usedPct = float64(used) * 100 / float64(usable)
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
			// No exact "for N minutes" here: the rpi_volt alarm is a sticky bit the
			// firmware samples about every 2 s and the monitor polls at 10 s, so the
			// clear dwell is not a continuous undervoltage-free minute and claiming one
			// would overstate the measurement.
			return conditionClear("Power supply recovered", "No undervoltage detected recently; the supply looks stable again")
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
				return
			}
			// Active but the reading gapped out short of the clear dwell: abandon any
			// pending clear run so the clear needs a contiguous stretch of under
			// readings after the gap, rather than completing from two under readings a
			// whole clear dwell apart with the sensor dark in between.
			c.h.ResetRun()
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

// evaluateCounters drives each serving device's counter conditions (dropped
// frames and capture overruns) from its cumulative counters, and resolves them
// for a device that has stopped serving.
func (h *Host) evaluateCounters(now time.Time) {
	for _, st := range h.devs {
		st.seen = false
	}
	for _, d := range h.counters() {
		st := h.devs[d.Name]
		if st == nil {
			// First sighting (the monitor's first poll, a device back after the
			// presence grace, or a re-enable): baseline only. A cumulative counter
			// cannot say how much of its count is recent, so nothing is counted
			// toward a condition until the next poll shows what changed. A nonzero
			// overrun count is still logged once, as the capture's running total,
			// so overruns since it opened leave a trace; a re-enable repeats a
			// total already logged, which the wording does not present as new.
			st = &counterState{
				h:    notify.NewHysteresis(dropsEnterAfter, dropsClearAfter),
				prev: d.Dropped, prevAt: now, gen: d.Gen, seen: true,
				ovPrev: d.Overruns,
				ov:     notify.NewFlap(overrunOnsetCount-1, overrunWindow, overrunClearAfter),
			}
			if d.Overruns > 0 {
				h.logf("device %q: capture has recovered from %d overrun(s) since it opened", d.Name, d.Overruns)
				st.ovLogAt = now
			}
			h.devs[d.Name] = st
			continue
		}
		st.seen, st.missed = true, 0
		// A fresh runtime: either its identity changed (a restart the counters did
		// not have to reveal, e.g. a new counter already passed the old value
		// between polls) or, absent a Gen, a counter went backwards. Adopting the
		// new gen here is essential: without it a restarted device would read as
		// restarted on every later poll, so its drops would rebaseline forever and
		// never fire, and its overruns would be recounted every poll and never clear.
		restarted := d.Gen != st.gen || d.Dropped < st.prev || d.Overruns < st.ovPrev
		st.gen = d.Gen
		h.observeDrops(st, now, d.Name, d.Dropped, restarted)
		h.observeOverruns(st, now, d.Name, d.Overruns, restarted)
	}
	for name, st := range h.devs {
		if st.seen {
			continue
		}
		st.missed++
		if st.missed < devicePresenceGrace {
			// Absent but within the presence grace: abandon a pending drops onset run
			// so a blip cannot carry it across the gap, but leave an active condition
			// untouched. A device that stopped serving must end with the grace
			// resolve ("device stopped"), never with a clear claiming it recovered.
			// A pending overrun window is kept: it counts overruns by wall-clock time,
			// so the gap cannot stretch it the way it would stretch a drops run.
			if !st.h.Active() {
				st.h.Reset()
			}
			continue
		}
		if st.h.Active() {
			h.pub.Resolve(streamDropsKey(name), "device stopped")
		}
		h.endOverruns(st, name, "device stopped")
		if st.ov.Active() {
			h.pub.Resolve(deviceOverrunsKey(name), "device stopped")
		}
		delete(h.devs, name)
	}
}

// observeDrops turns one device's cumulative dropped-frame counter into a rate
// over the poll interval and drives its condition. The onset needs the rate
// over dropsPerSecond; once active, any drop at all holds the condition, so it
// clears only after dropsClearAfter with no drops.
func (h *Host) observeDrops(st *counterState, now time.Time, name string, dropped uint64, restarted bool) {
	if restarted {
		// Rebaseline, then observe no drops for this poll: a fresh runtime has no
		// evidence of drops yet, so an active condition's clear run keeps advancing
		// and a pending onset run is abandoned, instead of skipping the observation
		// and letting a run survive the restart.
		st.prev, st.prevAt = dropped, now
		h.transition(st.h.Observe(now, false), streamDropsKey(name),
			func() notify.Notification { return dropsOnset(name, 0) },
			func() notify.Notification { return dropsClearFor(name) })
		return
	}
	delta := dropped - st.prev
	elapsed := now.Sub(st.prevAt).Seconds()
	st.prev, st.prevAt = dropped, now
	if elapsed <= 0 {
		return
	}
	rate := float64(delta) / elapsed
	over := rate > dropsPerSecond
	if st.h.Active() {
		over = delta > 0
	}
	h.transition(st.h.Observe(now, over), streamDropsKey(name),
		func() notify.Notification { return dropsOnset(name, rate) },
		func() notify.Notification { return dropsClearFor(name) })
}

// observeOverruns feeds one device's new capture overruns to its flap detector
// and publishes what it reports: the onset needs overrunOnsetCount within
// overrunWindow, and an active condition clears after overrunClearAfter with
// none. A restarted runtime's counter started at zero after the poll that last
// saw its predecessor, so its whole count is new since that poll. Overruns short
// of the onset are logged (see logOverruns); once the condition is raised its
// onset line speaks for them, and the line that ends it (the clear here, or a
// resolve in endOverruns) reports how many there were while it was raised. The quiet dwell is judged per
// poll, as notify.Flap does: the poll that completes the dwell clears the
// condition before counting its own overruns, which start a fresh window, so a
// burst of overrunOnsetCount there clears and re-raises it in the same poll.
// Like every condition monitor this runs only while notifications are enabled
// (the device's API and dashboard counter keeps counting regardless).
func (h *Host) observeOverruns(st *counterState, now time.Time, name string, total uint64, restarted bool) {
	delta := total
	if !restarted {
		delta = total - st.ovPrev
	}
	st.ovPrev = total
	if st.ov.Sweep(now) == notify.TransitionClear {
		h.logf("device %q: no capture overruns at any check for %s, overrun warning cleared (%d overrun(s) while it was raised)",
			name, humanDuration(int(overrunClearAfter/time.Second)), st.ovEpisode)
		st.ovEpisode = 0
		h.pub.Clear(deviceOverrunsKey(name), overrunsClearFor(name))
	}
	// One Event per overrun, capped at the onset count: past it a burst changes
	// nothing, since an active flap only records the time of its latest event.
	// Sweep has already cleared a quiet-ended flap at this now, so Event can
	// only report an onset.
	for range min(delta, overrunOnsetCount) {
		if st.ov.Event(now) == notify.TransitionOnset {
			h.logf("device %q: at least %d capture overruns within %s, audio lost; raising an overrun warning", name, overrunOnsetCount, humanDuration(int(overrunWindow/time.Second)))
			h.pub.Onset(overrunsOnset(name))
			// The onset line speaks for any overruns not yet logged, and after the
			// clear the first overrun is logged at once again. The episode starts
			// with this poll's overruns.
			st.ovUnlogged, st.ovLogAt, st.ovEpisode = 0, time.Time{}, delta
			return
		}
	}
	if st.ov.Active() {
		st.ovEpisode += delta
		return
	}
	h.logOverruns(st, now, name, delta)
}

// logOverruns logs overruns short of the onset at most once per overrunWindow
// per device, so a device that keeps overrunning just below the threshold does
// not write a line every poll for as long as it runs. An overrun is logged at
// once when no overrun line (or first-sighting line) was written in the last
// overrunWindow, or since an onset; otherwise it accumulates and is reported
// in one line with the others at the first poll after the window has passed,
// even a quiet one, so no count is held back while the device keeps serving.
func (h *Host) logOverruns(st *counterState, now time.Time, name string, delta uint64) {
	st.ovUnlogged += delta
	if st.ovUnlogged == 0 {
		return
	}
	switch {
	case st.ovLogAt.IsZero():
		h.logf("device %q: %d capture overrun(s), audio lost", name, st.ovUnlogged)
	case now.Sub(st.ovLogAt) >= overrunWindow:
		h.logf("device %q: %d capture overrun(s) since the last overrun report, audio lost", name, st.ovUnlogged)
	default:
		return
	}
	st.ovUnlogged, st.ovLogAt = 0, now
}

// endOverruns writes the line a device's overrun state owes before it is
// dropped (the device stopped serving, or notifications were turned off):
// the raised warning's overrun count, or the overruns still held back by the
// log rate limit, so neither is lost from the journal.
func (h *Host) endOverruns(st *counterState, name, reason string) {
	switch {
	case st.ov.Active():
		h.logf("device %q: overrun warning resolved (%s) after %d overrun(s) while it was raised", name, reason, st.ovEpisode)
	case st.ovUnlogged > 0:
		h.logf("device %q: %d capture overrun(s) since the last overrun report, audio lost (%s)", name, st.ovUnlogged, reason)
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
// shared by the counter-restart and normal-rate paths in observeDrops. It is
// symmetric with dropsOnset.
func dropsClearFor(name string) notify.Notification {
	return conditionClear("Client keeping up", name+" is no longer dropping frames")
}

func overrunsOnset(name string) notify.Notification {
	return notify.Notification{
		Severity: notify.SeverityWarning,
		Category: notify.CategoryDevice,
		Key:      deviceOverrunsKey(name),
		Source:   name,
		Title:    "Capture overruns",
		Message: fmt.Sprintf("%s lost audio to at least %d capture overruns within %s; the host may be too busy or the USB connection unstable",
			name, overrunOnsetCount, humanDuration(int(overrunWindow/time.Second))),
	}
}

func overrunsClearFor(name string) notify.Notification {
	return conditionClear("Capture overruns stopped",
		fmt.Sprintf("%s has had no capture overruns for %s", name, humanDuration(int(overrunClearAfter/time.Second))))
}
