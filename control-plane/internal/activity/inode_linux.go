//go:build linux

package activity

import (
	"os"
	"syscall"
)

// statInode returns the inode number from an os.FileInfo. Phase 5
// target is Linux only (the production host), so this is the only
// build-tagged file. If we ever build on another GOOS the build will
// fail loudly — better than silently returning 0 and breaking
// rotation detection.
func statInode(fi os.FileInfo) uint64 {
	if s, ok := fi.Sys().(*syscall.Stat_t); ok {
		return s.Ino
	}
	return 0
}
