//go:build linux

package sysinfo

import (
	"context"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtserver"
)

// Sampler tracks host CPU utilization by periodically diffing /proc/stat. It is
// safe for concurrent use: the collector reads Percent from a handler goroutine
// while the sampler goroutine updates it.
type Sampler struct {
	pct     atomic.Uint64 // math.Float64bits of the latest percentage
	hasData atomic.Bool

	// Owned solely by the sampler goroutine.
	prevIdle  uint64
	prevTotal uint64
}

// NewSampler starts a background sampler that refreshes CPU utilization every
// interval until ctx is cancelled. It primes the first /proc/stat reading so
// the next tick can produce a value.
func NewSampler(ctx context.Context, interval time.Duration) *Sampler {
	s := &Sampler{}
	if idle, total, ok := readCPUStat(); ok {
		s.prevIdle, s.prevTotal = idle, total
	}
	go s.loop(ctx, interval)
	return s
}

func (s *Sampler) loop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			idle, total, ok := readCPUStat()
			if !ok {
				continue
			}
			pct, valid := cpuBusyPercent(s.prevIdle, s.prevTotal, idle, total)
			s.prevIdle, s.prevTotal = idle, total
			if !valid {
				continue
			}
			s.pct.Store(math.Float64bits(pct))
			s.hasData.Store(true)
		}
	}
}

// Percent returns the latest CPU utilization percentage. ok is false on a nil
// sampler or before two readings have been taken.
func (s *Sampler) Percent() (pct float64, ok bool) {
	if s == nil || !s.hasData.Load() {
		return 0, false
	}
	return math.Float64frombits(s.pct.Load()), true
}

// readCPUStat reads and parses the aggregate line of /proc/stat.
func readCPUStat() (idle, total uint64, ok bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	return parseCPUStat(b)
}

// staticFacts holds the host facts that do not change for the life of the
// process, so they are gathered once rather than re-read on every /system poll.
type staticFacts struct {
	platform string
	os       string
	kernel   string
	hostname string
	cpuModel string
	cpuCores int
}

var (
	staticOnce sync.Once
	staticInfo staticFacts
)

// gatherStatic reads the immutable host facts once.
func gatherStatic() staticFacts {
	s := staticFacts{
		platform: runtime.GOOS + "/" + runtime.GOARCH,
		cpuCores: runtime.NumCPU(),
	}
	if h, err := os.Hostname(); err == nil {
		s.hostname = h
	}
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		s.os = parseOSReleasePretty(b)
	}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		s.kernel = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		s.cpuModel = parseCPUModel(b)
	}
	return s
}

// Collect gathers a full SystemInfo snapshot. dataPath is any path on the
// filesystem whose usage should be reported (the appliance's config directory).
// sampler may be nil, in which case CPUPercent is absent. Static host facts are
// cached after the first call; the live fields (memory, disk, temperature, CPU
// percent, network) are read every call. Every source is best effort: an
// unreadable file leaves its field zero or absent rather than failing the whole
// snapshot. It returns mgmtserver.SystemInfo directly (the shared API DTO)
// rather than a private struct, collapsing what would be a triple mapping into
// one; sysinfo depends on mgmtserver, not the reverse, so there is no cycle.
func Collect(dataPath string, sampler *Sampler) mgmtserver.SystemInfo {
	staticOnce.Do(func() { staticInfo = gatherStatic() })
	si := mgmtserver.SystemInfo{
		Platform: staticInfo.platform,
		OS:       staticInfo.os,
		Kernel:   staticInfo.kernel,
		Hostname: staticInfo.hostname,
		CPUModel: staticInfo.cpuModel,
		CPUCores: staticInfo.cpuCores,
	}
	if b, ok := readMeminfoFile(procMeminfo); ok {
		if total, used, ok := parseMemInfo(b); ok {
			si.MemTotal, si.MemUsed = total, used
		}
	}
	if total, used, ok := DiskUsage(dataPath); ok {
		si.DiskTotal, si.DiskUsed = total, used
	}
	if c, ok := ReadTemp(); ok {
		si.TempCelsius = &c
	}
	if p, ok := sampler.Percent(); ok {
		si.CPUPercent = &p
	}
	si.Network = readInterfaces()
	return si
}

// procMeminfo is the kernel memory-info pseudo-file. ReadMem passes it to readMem
// so the parsing and the "no MemAvailable" branch are testable against a fixture
// without a fixed /proc path; production always reads this file.
const procMeminfo = "/proc/meminfo"

// ReadMem returns total and available memory in bytes from /proc/meminfo. ok is
// false when the file is unreadable or lacks MemTotal or MemAvailable, so a
// caller judging free memory never mistakes a missing figure for zero.
func ReadMem() (total, avail int64, ok bool) {
	return readMem(procMeminfo)
}

// readMem is ReadMem's seam: it parses total and available memory from the
// meminfo file at path. ok is false when the file is unreadable, lacks MemTotal,
// or lacks MemAvailable (a kernel before 3.14), so the reject-when-absent branch
// can be exercised in a test.
func readMem(path string) (total, avail int64, ok bool) {
	b, ok := readMeminfoFile(path)
	if !ok {
		return 0, 0, false
	}
	total, avail, haveAvail, ok := parseMemAvailable(b)
	if !ok || !haveAvail {
		return 0, 0, false
	}
	return total, avail, true
}

// readMeminfoFile reads the kernel meminfo pseudo-file at path, returning its bytes
// and whether the read succeeded. Collect and readMem share it so the read and
// its gosec exception live in one place; each keeps its own parser (parseMemInfo
// derives used memory, parseMemAvailable derives available).
func readMeminfoFile(path string) ([]byte, bool) {
	b, err := os.ReadFile(path) //nolint:gosec // /proc/meminfo in production; a test fixture otherwise
	if err != nil {
		return nil, false
	}
	return b, true
}

// DiskUsage returns total and used bytes of the filesystem holding path. ok is
// false when path is empty or the statfs syscall fails (a missing or unmounted
// path).
func DiskUsage(path string) (total, used int64, ok bool) {
	if path == "" {
		return 0, 0, false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	// used = Blocks - Bfree (both include root-reserved space) so the figure
	// matches df's Used; Bavail would exclude reserved blocks from free only,
	// overstating used by the reserved amount.
	// Bsize is int32 on ILP32 (arm, 386) and int64 on LP64; widen once so the
	// multiplications below are int64 on every architecture. The conversion is
	// required on 32-bit; unconvert only sees the 64-bit build, where it is a
	// no-op, hence the suppression.
	bsize := int64(st.Bsize)         //nolint:unconvert // required on 32-bit where Bsize is int32
	total = int64(st.Blocks) * bsize //nolint:gosec // block counts fit int64 on real filesystems
	free := int64(st.Bfree) * bsize  //nolint:gosec // block counts fit int64 on real filesystems
	used = total - free
	if used < 0 {
		used = 0
	}
	return total, used, true
}

// cachedSensor resolves a /sys sensor path lazily and caches it, so a periodic
// reader (the 10 s host-health poll) re-reads one known file instead of
// re-scanning the sysfs tree on every tick. R is the reading type: float64 for a
// temperature, bool for the undervoltage alarm. It is safe for concurrent use;
// the management /system handler and the monitor poll goroutine both read
// temperature. The mutex is held across the sysfs read, which is fine off any hot
// path.
//
// The cached path is re-resolved immediately when a cached read fails (the device
// renumbered or briefly unreadable), and that re-resolution is never throttled, so
// a transient read blip recovers on the next poll. The backoff applies only when
// nothing is cached and a scan still finds no sensor, so a host without the sensor
// is not walked on every poll.
type cachedSensor[R any] struct {
	mu        sync.Mutex
	clock     func() time.Time
	backoff   time.Duration
	readAt    func(path string) (R, bool)        // read+parse a known sensor path
	probe     func() (path string, r R, ok bool) // scan for the sensor path + reading
	path      string                             // resolved path, "" until first found
	nextProbe time.Time                          // earliest re-scan while nothing is cached
}

func (c *cachedSensor[R]) read() (R, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero R
	hadPath := c.path != ""
	if hadPath {
		if r, ok := c.readAt(c.path); ok {
			return r, true
		}
		// The cached path went stale (renumbered or removed): drop it and re-probe
		// now, since the sensor may still exist elsewhere in the tree.
		c.path = ""
	}
	now := c.clock()
	// The backoff throttles re-scanning only while nothing is cached, so a host
	// without the sensor is not walked on every poll. A path cached on entry is
	// exempt from both the backoff gate and arming it: a transient read failure that
	// just invalidated a good sensor must recover on the next poll (as the old
	// stateless reader did), not stay dark for the whole backoff window.
	if !hadPath && now.Before(c.nextProbe) {
		return zero, false
	}
	path, r, ok := c.probe()
	if !ok {
		if !hadPath {
			c.nextProbe = now.Add(c.backoff)
		}
		return zero, false
	}
	c.path = path
	return r, true
}

// sensorBackoff bounds how often a cachedSensor re-scans sysfs while the sensor is
// absent. The host monitor polls every 10 s; a host with no thermal zone or no
// rpi_volt hwmon is walked at most once a minute rather than on every poll.
const sensorBackoff = 60 * time.Second

// sysClassThermal is the base directory of the kernel's thermal zones.
const sysClassThermal = "/sys/class/thermal"

var defaultTempSensor = &cachedSensor[float64]{
	clock:   time.Now,
	backoff: sensorBackoff,
	readAt:  readTempAt,
	probe:   func() (string, float64, bool) { return probeTemp(sysClassThermal) },
}

// ReadTemp returns the SoC/CPU temperature in Celsius. It prefers a thermal zone
// whose type names a CPU or SoC sensor, falling back to the first readable zone.
// ok is false when no thermal zone is exposed or none can be read and parsed. The
// resolved zone path is cached (see cachedSensor), so the periodic host poll
// re-reads one file rather than re-scanning every zone.
func ReadTemp() (celsius float64, ok bool) { return defaultTempSensor.read() }

// readTempAt reads a cached thermal zone for cachedSensor. It first re-reads the
// zone's type and rejects the cached path unless it still names a CPU/SoC sensor,
// so a driver reload that renumbered the zones forces a fresh probe rather than
// trusting a path that now points at a different device. ok is false on any read,
// identity, or parse failure.
func readTempAt(tempPath string) (float64, bool) {
	tb, err := os.ReadFile(filepath.Join(filepath.Dir(tempPath), "type")) //nolint:gosec // sibling of a path resolved by probeTemp under /sys/class/thermal
	if err != nil || !isPreferredTempType(strings.TrimSpace(string(tb))) {
		return 0, false
	}
	b, err := os.ReadFile(tempPath) //nolint:gosec // path resolved by probeTemp under /sys/class/thermal
	if err != nil {
		return 0, false
	}
	return parseMilliCelsius(b)
}

// probeTemp scans root for thermal zones and returns the reading of the preferred
// zone (a CPU/SoC zone, else the first readable one). The returned path is the
// zone's temp file when the pick is a CPU/SoC zone (cacheable), and empty for a
// fallback pick: a fallback reading is still returned, but left uncached so a
// CPU/SoC zone whose driver loads later is picked up on the next poll instead of
// being masked by a locked-in fallback. ok is false when no zone can be read.
//
// Among several CPU/SoC zones the first by glob order wins, and once one is cached
// the reader stays on it (readTempAt re-verifies only that its type is still
// CPU/SoC, not that it is still the lowest-indexed one). So a transient read error
// on the preferred zone during this one probe can leave a sibling CPU/SoC zone
// cached instead; that is accepted, since every CPU/SoC zone reports an equally
// valid SoC temperature for the health monitor.
func probeTemp(root string) (path string, celsius float64, ok bool) {
	zones, _ := filepath.Glob(filepath.Join(root, "thermal_zone*", "temp"))
	candidates := make([]tempCandidate, 0, len(zones))
	paths := make([]string, 0, len(zones))
	for _, tempPath := range zones {
		b, err := os.ReadFile(tempPath) //nolint:gosec // path from a fixed /sys glob
		if err != nil {
			continue
		}
		c, ok := parseMilliCelsius(b)
		if !ok {
			continue
		}
		typ := ""
		if tb, err := os.ReadFile(filepath.Join(filepath.Dir(tempPath), "type")); err == nil {
			typ = strings.TrimSpace(string(tb))
		}
		candidates = append(candidates, tempCandidate{Type: typ, Celsius: c})
		paths = append(paths, tempPath)
	}
	i, preferred, ok := selectTempIndex(candidates)
	if !ok {
		return "", 0, false
	}
	if !preferred {
		return "", candidates[i].Celsius, true
	}
	return paths[i], candidates[i].Celsius, true
}

// sysClassHwmon is the base directory of the kernel's hardware-monitor devices.
const sysClassHwmon = "/sys/class/hwmon"

// undervoltHwmonName is the hwmon device name the Raspberry Pi firmware driver
// registers. Its in0_lcrit_alarm exposes the firmware's sticky undervoltage bit,
// which the driver samples about every 2 s, so a set alarm means undervoltage
// occurred within the last driver poll rather than at this instant.
const undervoltHwmonName = "rpi_volt"

var defaultVoltSensor = &cachedSensor[bool]{
	clock:   time.Now,
	backoff: sensorBackoff,
	readAt:  readAlarmAt,
	probe:   func() (string, bool, bool) { return probeUndervoltage(sysClassHwmon) },
}

// ReadUndervoltage reports whether the Raspberry Pi has been undervolted within
// the last driver poll, from the rpi_volt hwmon's in0_lcrit_alarm. That alarm is
// the firmware's sticky bit, sampled by the driver about every 2 s, so it is a
// recent reading rather than an instantaneous one. The attribute is world
// readable, so this works as an unprivileged user. ok is false on hosts without
// that hwmon device or when the attribute cannot be read or parsed. The resolved
// alarm path is cached (see cachedSensor), so the periodic host poll re-reads one
// file rather than re-globbing every hwmon.
func ReadUndervoltage() (now, ok bool) { return defaultVoltSensor.read() }

// readAlarmAt reads a cached rpi_volt in0_lcrit_alarm for cachedSensor. It first
// re-reads the sibling name and rejects the cached path unless it still reads
// rpi_volt, so a driver reload that renumbered the hwmon devices forces a fresh
// probe rather than trusting a path that now points at a different device. ok is
// false on any read, identity, or parse failure.
func readAlarmAt(alarmPath string) (now, ok bool) {
	nb, err := os.ReadFile(filepath.Join(filepath.Dir(alarmPath), "name")) //nolint:gosec // sibling of a path resolved by probeUndervoltage under /sys/class/hwmon
	if err != nil || strings.TrimSpace(string(nb)) != undervoltHwmonName {
		return false, false
	}
	b, err := os.ReadFile(alarmPath) //nolint:gosec // path resolved by probeUndervoltage under /sys/class/hwmon
	if err != nil {
		return false, false
	}
	return parseAlarm(b)
}

// probeUndervoltage scans root/hwmon*/name for the rpi_volt device (rather than a
// fixed index, because hwmonN numbering is assigned at boot and is not stable) and
// returns its in0_lcrit_alarm path and current value. It tries every rpi_volt
// match, skipping one whose alarm is unreadable or unparseable, because the kernel
// can register more than one hwmon under the same name (real firmware registers
// exactly one, so this is defensive). ok is false when no readable rpi_volt alarm
// is found.
func probeUndervoltage(root string) (path string, now, ok bool) {
	names, _ := filepath.Glob(filepath.Join(root, "hwmon*", "name"))
	for _, namePath := range names {
		b, err := os.ReadFile(namePath) //nolint:gosec // path from a glob under the caller-supplied hwmon root (/sys/class/hwmon in production)
		if err != nil || strings.TrimSpace(string(b)) != undervoltHwmonName {
			continue
		}
		alarmPath := filepath.Join(filepath.Dir(namePath), "in0_lcrit_alarm")
		ab, err := os.ReadFile(alarmPath) //nolint:gosec // sibling of a glob match under the caller-supplied hwmon root (/sys/class/hwmon in production)
		if err != nil {
			continue
		}
		if set, parsed := parseAlarm(ab); parsed {
			return alarmPath, set, true
		}
		// Unparseable alarm: fall through to any further rpi_volt match.
	}
	return "", false, false
}

// readUndervoltage is probeUndervoltage without the resolved path: a stateless
// seam the table test drives against a fake hwmon root.
func readUndervoltage(root string) (now, ok bool) {
	_, now, ok = probeUndervoltage(root)
	return
}

// readInterfaces lists non-loopback network interfaces with their addresses and
// byte counters.
func readInterfaces() []mgmtserver.NetworkInterface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]mgmtserver.NetworkInterface, 0, len(ifaces))
	for i := range ifaces {
		ifc := &ifaces[i]
		if ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		ni := mgmtserver.NetworkInterface{
			Name: ifc.Name,
			MAC:  ifc.HardwareAddr.String(),
			Up:   ifc.Flags&net.FlagUp != 0 && ifc.Flags&net.FlagRunning != 0,
		}
		if addrs, err := ifc.Addrs(); err == nil {
			for _, a := range addrs {
				ni.Addresses = append(ni.Addresses, a.String())
			}
		}
		ni.RxBytes = readCounter(ifc.Name, "rx_bytes")
		ni.TxBytes = readCounter(ifc.Name, "tx_bytes")
		out = append(out, ni)
	}
	return out
}

// sysClassNet is the base directory for per-interface kernel statistics.
const sysClassNet = "/sys/class/net"

// readCounter reads one /sys/class/net/<name>/statistics counter.
func readCounter(name, stat string) int64 {
	b, err := os.ReadFile(filepath.Join(sysClassNet, name, "statistics", stat)) //nolint:gosec // name from the kernel's own interface list
	if err != nil {
		return 0
	}
	return parseCounter(b)
}
