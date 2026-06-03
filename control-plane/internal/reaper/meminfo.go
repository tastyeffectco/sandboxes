package reaper

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// MemInfo holds the slim subset of /proc/meminfo Phase 5 cares about.
// CLAUDE.md "Host memory pressure reaper" specifies the trigger as
// `MemAvailable` (not `MemFree`).
type MemInfo struct {
	Total     uint64 // KB
	Available uint64 // KB
}

// AvailablePct returns MemAvailable as a percentage of MemTotal.
// Returns 100.0 when total is 0 (defensive — avoids div-by-zero).
func (m MemInfo) AvailablePct() float64 {
	if m.Total == 0 {
		return 100
	}
	return float64(m.Available) * 100.0 / float64(m.Total)
}

// AvailableBytes returns MemAvailable in bytes.
func (m MemInfo) AvailableBytes() uint64 { return m.Available * 1024 }

// ReadMemInfo parses the slim subset of /proc/meminfo we need.
func ReadMemInfo() (MemInfo, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return MemInfo{}, fmt.Errorf("open /proc/meminfo: %w", err)
	}
	defer f.Close()
	var mi MemInfo
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		// Format: "MemTotal:       24561136 kB"
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		key := line[:colon]
		rest := strings.TrimSpace(line[colon+1:])
		// Drop trailing " kB" if present.
		if strings.HasSuffix(rest, " kB") {
			rest = strings.TrimSpace(rest[:len(rest)-3])
		}
		switch key {
		case "MemTotal":
			n, err := strconv.ParseUint(rest, 10, 64)
			if err == nil {
				mi.Total = n
			}
		case "MemAvailable":
			n, err := strconv.ParseUint(rest, 10, 64)
			if err == nil {
				mi.Available = n
			}
		}
	}
	return mi, sc.Err()
}
