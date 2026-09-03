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
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		if total, used, ok := parseMemInfo(b); ok {
			si.MemTotal, si.MemUsed = total, used
		}
	}
	if total, used, ok := diskUsage(dataPath); ok {
		si.DiskTotal, si.DiskUsed = total, used
	}
	if c, ok := readTemp(); ok {
		si.TempCelsius = &c
	}
	if p, ok := sampler.Percent(); ok {
		si.CPUPercent = &p
	}
	si.Network = readInterfaces()
	return si
}

// diskUsage returns total and used bytes of the filesystem holding path.
func diskUsage(path string) (total, used int64, ok bool) {
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

// readTemp returns the SoC/CPU temperature in Celsius. It prefers a thermal
// zone whose type names a CPU or SoC sensor, falling back to the first readable
// zone. ok is false when no thermal zone is exposed.
func readTemp() (float64, bool) {
	zones, _ := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
	candidates := make([]tempCandidate, 0, len(zones))
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
	}
	return selectTemp(candidates)
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
