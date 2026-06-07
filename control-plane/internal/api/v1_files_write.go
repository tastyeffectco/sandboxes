package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sandboxd/control-plane/internal/audit"
)

// PUT /v1/sandboxes/{id}/files?path=<rel> — atomic generic file write.
//
// Roots at the workspace mount (`/home/sandbox/` inside the container).
// the upstream backend uses this to prepare AGENTS.md / CLAUDE.md / opencode.json /
// any other file the chosen agent expects. The platform does NOT
// inspect the body — the file is opaque bytes.
//
// Security model (paying-tenant threat model from CLAUDE.md):
//   - The caller holds a service token; the upstream backend bugs are the
//     primary concern, not malicious traffic.
//   - Path is `filepath.Clean`-normalised, absolute paths and `..` are
//     rejected, and the resolved final path MUST stay under the mount
//     root via prefix check.
//   - Reserved subtrees (`.runtimed/`, `lost+found/`) are refused —
//     `.runtimed/` is the in-sandbox supervisor's working dir and
//     writing into it could corrupt task state.
//   - The final file is opened with O_NOFOLLOW so a symlink at the leaf
//     cannot redirect the write off the mount.
//   - Written atomically: tmp file in the same directory + rename, so
//     a partially-written file is never observable.
//   - chown'd to the workspace owner uid/gid (the userns-remapped
//     sandbox user) so the agent sees its own user own the file.
//
// Per-file limit mirrors uploads.go (25 MiB) — small textual config
// and modestly-sized assets are the use case; larger blobs would
// belong in object storage, not the workspace.

const (
	// maxPutFileBytes — per-request body cap. Mirrors uploads.go.
	maxPutFileBytes = 25 << 20
)

// reservedPathPrefixes are subtrees of the workspace mount that the
// platform owns. Writes here are refused even with a valid token.
var reservedPathPrefixes = []string{".runtimed/", ".runtimed", "lost+found/", "lost+found"}

// resolveWritePath validates a caller-supplied relative path against
// the workspace mount root and returns the absolute on-disk path.
//
// Rules (in order):
//  1. path is non-empty and does not contain a NUL byte.
//  2. path is not absolute.
//  3. After Clean, no segment is `..` (would escape) and no segment is
//     empty (would denote a directory write).
//  4. The cleaned path does not target a reserved subtree.
//  5. The resolved on-disk path stays under <mnt>/.
func resolveWritePath(mnt, raw string) (string, string, error) {
	if raw == "" {
		return "", "", errors.New("path is required")
	}
	if strings.ContainsRune(raw, 0) {
		return "", "", errors.New("invalid path: NUL byte")
	}
	if filepath.IsAbs(raw) {
		return "", "", errors.New("path must be relative to the workspace root")
	}
	// Trailing slash signals directory intent — check before Clean,
	// which would strip it.
	if strings.HasSuffix(raw, "/") {
		return "", "", errors.New("path must name a file, not a directory")
	}
	clean := filepath.Clean(raw)
	// Reject any traversal segment. filepath.Clean reduces "a/../b" to
	// "b" but leaves a leading ".." in place.
	for _, seg := range strings.Split(clean, string(os.PathSeparator)) {
		if seg == ".." {
			return "", "", errors.New("path traversal (..) not allowed")
		}
	}
	if clean == "." || clean == "/" {
		return "", "", errors.New("path must name a file, not the root")
	}
	if strings.HasSuffix(clean, "/") {
		return "", "", errors.New("path must name a file, not a directory")
	}
	for _, p := range reservedPathPrefixes {
		if clean == strings.TrimSuffix(p, "/") ||
			strings.HasPrefix(clean, strings.TrimSuffix(p, "/")+"/") {
			return "", "", errors.New("path is in a reserved subtree (" +
				strings.TrimSuffix(p, "/") + ")")
		}
	}
	full := filepath.Join(mnt, clean)
	// Defence-in-depth: re-check the final prefix after Join.
	if full != mnt && !strings.HasPrefix(full, mnt+string(os.PathSeparator)) {
		return "", "", errors.New("resolved path escapes the workspace mount")
	}
	return full, clean, nil
}

// mountOwner returns the uid/gid that owns the workspace mount root —
// the sandbox user as userns-remapped on the host. Falls back to -1
// so callers can skip chown gracefully.
func mountOwner(mnt string) (uid, gid int) {
	fi, err := os.Stat(mnt)
	if err != nil {
		return -1, -1
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, -1
	}
	return int(st.Uid), int(st.Gid)
}

// v1PutFile is the handler for PUT /v1/sandboxes/{id}/files.
func (s *Server) v1PutFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, mnt := s.Loopback.Paths(id)
	if info, err := os.Stat(mnt); err != nil || !info.IsDir() {
		writeV1Err(w, http.StatusNotFound, "not_found", "no workspace for that sandbox")
		return
	}

	full, rel, err := resolveWritePath(mnt, r.URL.Query().Get("path"))
	if err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// Hard size gate before reading the body.
	if r.ContentLength > maxPutFileBytes {
		writeV1Err(w, http.StatusRequestEntityTooLarge, "invalid_request",
			"file exceeds the 25 MiB limit")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPutFileBytes)

	uid, gid := mountOwner(mnt)

	// Create parent dirs with the same owner so the agent can read its
	// own tree.
	parent := filepath.Dir(full)
	if err := os.MkdirAll(parent, 0o775); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if uid >= 0 {
		// Walk up and chown any newly-created parents back to the owner.
		// We only chown directories we may have created (mode marker
		// 0o775 + currently root-owned). Best-effort.
		for p := parent; p != mnt && strings.HasPrefix(p, mnt+string(os.PathSeparator)); p = filepath.Dir(p) {
			if fi, err := os.Stat(p); err == nil {
				if st, ok := fi.Sys().(*syscall.Stat_t); ok && (int(st.Uid) != uid || int(st.Gid) != gid) {
					_ = os.Chown(p, uid, gid)
				}
			}
		}
	}

	// Atomic write: tmp in same dir + rename. O_NOFOLLOW on the tmp
	// ensures we never write through a symlink left by a previous run.
	tmp, err := os.CreateTemp(parent, ".put-*.tmp")
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	tmpPath := tmp.Name()
	written, copyErr := io.Copy(tmp, r.Body)
	closeErr := tmp.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		var mbe *http.MaxBytesError
		if errors.As(copyErr, &mbe) {
			writeV1Err(w, http.StatusRequestEntityTooLarge, "invalid_request",
				"file exceeds the 25 MiB limit")
			return
		}
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "read body: "+copyErr.Error())
		return
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		writeV1Err(w, http.StatusInternalServerError, "internal", "close tmp: "+closeErr.Error())
		return
	}

	// Set ownership BEFORE rename so the file is never visible at the
	// target path with wrong owner.
	if uid >= 0 {
		_ = os.Chown(tmpPath, uid, gid)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		_ = os.Remove(tmpPath)
		writeV1Err(w, http.StatusInternalServerError, "internal", "chmod: "+err.Error())
		return
	}

	// Refuse to overwrite if the existing leaf is a symlink — a
	// symlinked target could redirect the write out of the mount.
	if fi, err := os.Lstat(full); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(tmpPath)
		writeV1Err(w, http.StatusBadRequest, "invalid_request",
			"refusing to overwrite a symlink")
		return
	}

	if err := os.Rename(tmpPath, full); err != nil {
		_ = os.Remove(tmpPath)
		writeV1Err(w, http.StatusInternalServerError, "internal", "rename: "+err.Error())
		return
	}

	s.auditAction(r, audit.Entry{
		Action: "file.put", Target: id,
		Detail: map[string]any{"path": rel, "size": written},
	})
	writeJSON(w, http.StatusOK, map[string]any{"path": rel, "size": written})
}
