// Package sysinfo gathers host hardware facts and live system metrics (CPU,
// memory, storage, temperature, network) for the management API's GET /system
// endpoint. It reads only /proc, /sys, /etc/os-release, and statfs-family
// syscalls, so it stays pure Go with no cgo. The parsers here are
// platform-neutral and unit-tested; the file and syscall reads that feed them
// live in the Linux-only collector.
package sysinfo

import (
	"strconv"
	"strings"
)

// parseOSReleasePretty extracts PRETTY_NAME from /etc/os-release content.
func parseOSReleasePretty(data []byte) string {
	for line := range strings.SplitSeq(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(strings.TrimSpace(v), "\"'")
		}
	}
	return ""
}

// parseCPUModel returns a human CPU or SoC name from /proc/cpuinfo, trying the
// x86 "model name" first, then the ARM "Model", then "Hardware".
func parseCPUModel(data []byte) string {
	var modelName, model, hardware string
	for line := range strings.SplitSeq(string(data), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "model name":
			if modelName == "" {
				modelName = val
			}
		case "Model":
			if model == "" {
				model = val
			}
		case "Hardware":
			if hardware == "" {
				hardware = val
			}
		}
	}
	switch {
	case modelName != "":
		return modelName
	case model != "":
		return model
	default:
		return hardware
	}
}

// parseMemInfo returns total and used bytes from /proc/meminfo. used is total
// minus MemAvailable, clamped at zero.
func parseMemInfo(data []byte) (total, used int64, ok bool) {
	var totalKB, availKB int64
	var haveTotal, haveAvail bool
	for line := range strings.SplitSeq(string(data), "\n") {
		key, val, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "MemTotal":
			totalKB, haveTotal = parseFirstInt(val)
		case "MemAvailable":
			availKB, haveAvail = parseFirstInt(val)
		}
	}
	if !haveTotal {
		return 0, 0, false
	}
	total = totalKB * 1024
	if haveAvail {
		used = (totalKB - availKB) * 1024
		if used < 0 {
			used = 0
		}
	}
	return total, used, true
}

// parseFirstInt parses the first whitespace-delimited integer of s, e.g.
// "  16384 kB" -> 16384.
func parseFirstInt(s string) (int64, bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseCPUStat sums the aggregate "cpu" line of /proc/stat, returning idle
// (idle + iowait) and total jiffies. Only the eight standard fields (user, nice,
// system, idle, iowait, irq, softirq, steal) are summed; guest and guest_nice
// are excluded because the kernel already folds them into user and nice, so
// counting them would double the guest time. A caller compares two readings to
// derive utilization.
func parseCPUStat(data []byte) (idle, total uint64, ok bool) {
	for line := range strings.SplitSeq(string(data), "\n") {
		// The aggregate line is "cpu  ..."; per-core lines are "cpu0 ...".
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line) // fields[0]=="cpu", rest are counters
		if len(fields) < 6 {
			return 0, 0, false
		}
		var sum, idleJ uint64
		for i, f := range fields[1:] {
			if i >= 8 { // ignore guest/guest_nice; already in user/nice
				break
			}
			n, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			sum += n
			switch i {
			case 3: // idle
				idleJ += n
			case 4: // iowait
				idleJ += n
			}
		}
		return idleJ, sum, true
	}
	return 0, 0, false
}

// cpuBusyPercent derives busy utilization across two /proc/stat readings. It
// returns false when no time elapsed (no basis for a ratio).
func cpuBusyPercent(prevIdle, prevTotal, idle, total uint64) (float64, bool) {
	// Both counters are monotonic; a decrease means a reboot, wraparound, or a
	// suspend/resume gap. Drop such a sample rather than derive a bogus ratio
	// (an idle-only rollback would otherwise underflow dIdle and report 0%).
	if total <= prevTotal || idle < prevIdle {
		return 0, false
	}
	dTotal := total - prevTotal
	dIdle := idle - prevIdle
	if dIdle > dTotal {
		dIdle = dTotal
	}
	pct := float64(dTotal-dIdle) / float64(dTotal) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct, true
}

// tempCandidate is one thermal zone's type label and its temperature.
type tempCandidate struct {
	Type    string
	Celsius float64
}

// selectTemp picks the SoC/CPU temperature from the readable thermal zones,
// preferring a zone whose type names a CPU or SoC sensor and otherwise falling
// back to the first zone. ok is false when there are no candidates.
func selectTemp(candidates []tempCandidate) (float64, bool) {
	if len(candidates) == 0 {
		return 0, false
	}
	for i := range candidates {
		t := strings.ToLower(candidates[i].Type)
		if strings.Contains(t, "cpu") || strings.Contains(t, "soc") {
			return candidates[i].Celsius, true
		}
	}
	return candidates[0].Celsius, true
}

// parseMilliCelsius parses a /sys thermal-zone temp file ("48123\n" -> 48.123).
func parseMilliCelsius(data []byte) (float64, bool) {
	s := strings.TrimSpace(string(data))
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return float64(n) / 1000.0, true
}

// parseCounter parses a /sys network statistics counter file.
func parseCounter(data []byte) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}
