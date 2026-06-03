package reaper

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ReadCgroupMemoryCurrent reads memory.current for the given cgroup v2
// relative path (`/system.slice/docker-<id>.scope` or
// `/docker/<id>` depending on the cgroup driver). Returns bytes.
//
// CLAUDE.md "Host memory pressure reaper" emergency band: "stop
// heaviest-RSS sandbox even if active". `memory.current` is the
// closest single number to RSS — it's the sum of every memory page
// charged to the cgroup, anon + file. We don't try to split it into
// anon-only via `memory.stat`; the ordering between candidate
// sandboxes is what the reaper cares about, and `memory.current`
// preserves it.
//
// Returns (0, nil) when the path doesn't exist — that's a stopped
// sandbox whose cgroup has already been torn down, and the caller
// uses 0 as "not a candidate" automatically.
func ReadCgroupMemoryCurrent(cgroupRel string) (uint64, error) {
	if cgroupRel == "" {
		return 0, nil
	}
	path := "/sys/fs/cgroup" + cgroupRel + "/memory.current"
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	s := strings.TrimSpace(string(b))
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	return n, nil
}
