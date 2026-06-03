// Package cgroup writes memory.high for running containers by
// discovering the cgroup v2 path via /proc/<pid>/cgroup. CLAUDE.md
// "Known foot-guns" entry: "Hardcoded systemd cgroup paths. Don't.
// Discover via /proc/<pid>/cgroup." This package is the only place
// in sandboxd that reads /proc and writes /sys/fs/cgroup.
//
// CLAUDE.md "How `memory.high` is set" entry: "discovers the cgroup
// v2 path via /proc/<container-pid>/cgroup, parsing the line that
// starts with `0::`. It then writes `4G` to
// /sys/fs/cgroup<path>/memory.high. This is driver-agnostic — works
// under both `systemd` and `cgroupfs` Docker cgroup drivers."
package cgroup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// SetMemoryHigh writes memory.high for a running container.
// It discovers the cgroup v2 path via /proc/<pid>/cgroup and returns
// the relative path so the caller can persist it (the reconciler
// re-applies on boot without re-discovering, though re-discovery is
// cheap if the stored path is missing).
//
// Idempotent: writing the same value twice is fine. The kernel parses
// the value on write, so "4G" and "4294967296" are both accepted.
//
// Races: a container can be running but /proc/<pid>/cgroup briefly
// empty during cgroup setup. Retry with a short bounded backoff
// (5×100 ms). Longer waits indicate a real problem worth surfacing.
func SetMemoryHigh(ctx context.Context, pid int, value string) (cgroupRel string, err error) {
	if pid <= 0 {
		return "", errors.New("cgroup.SetMemoryHigh: pid must be > 0")
	}
	for attempt := 0; attempt < 5; attempt++ {
		rel, err := discover(pid)
		if err == nil && rel != "" {
			path := "/sys/fs/cgroup" + rel + "/memory.high"
			if err := os.WriteFile(path, []byte(value+"\n"), 0o644); err != nil {
				return rel, fmt.Errorf("write %s: %w", path, err)
			}
			return rel, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return "", fmt.Errorf("cgroup.SetMemoryHigh: cgroup not visible at /proc/%d/cgroup after 5 attempts", pid)
}

// Discover parses /proc/<pid>/cgroup and returns the relative path
// from the "0::<path>" line. Exported for testing and for the
// reconciler when it wants to re-derive without writing.
func Discover(pid int) (string, error) { return discover(pid) }

func discover(pid int) (string, error) {
	path := fmt.Sprintf("/proc/%d/cgroup", pid)
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// cgroup v2 unified hierarchy: "0::<path>"
		line := sc.Text()
		if strings.HasPrefix(line, "0::") {
			return strings.TrimSpace(line[3:]), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", nil
}
