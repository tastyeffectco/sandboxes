// v1_snapshots.go — snapshots-as-templates
// (ops/design/snapshots-as-templates.md). A snapshot is a reusable,
// frozen raw copy of a sandbox's workspace .img, stored under
// LibraryRoot and cloned into new sandboxes via the existing
// ProvisionFromTemplate path. Scoped to the API tenant (auth.Actor.Name).
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sandboxd/control-plane/internal/audit"
	"github.com/sandboxd/control-plane/internal/auth"
	"github.com/sandboxd/control-plane/internal/store"
)

// v1Snapshot is the public snapshot object.
type v1Snapshot struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	SourceSandboxID string `json:"source_sandbox_id,omitempty"`
	BaseImage       string `json:"base_image"`
	Visibility      string `json:"visibility"`
	SizeBytes       int64  `json:"size_bytes,omitempty"`
	CreatedAt       string `json:"created_at"`
}

func v1SnapshotFromRow(s *store.Snapshot) v1Snapshot {
	out := v1Snapshot{
		ID:         s.ID,
		Name:       s.Name,
		Status:     s.Status,
		BaseImage:  s.BaseImage,
		Visibility: s.Visibility,
		CreatedAt:  s.CreatedAt.UTC().Format(time.RFC3339),
	}
	if s.SourceSandboxID.Valid {
		out.SourceSandboxID = s.SourceSandboxID.String
	}
	if s.SizeBytes.Valid {
		out.SizeBytes = s.SizeBytes.Int64
	}
	return out
}

// tenantToken is the snapshot ownership boundary: the authenticated
// API token's name (auth.Actor.Name). All the upstream backend traffic carries one
// token, so the upstream backend's snapshots are shared across its end-users — the
// platform cannot and does not scope by the untrusted external user_id.
func tenantToken(r *http.Request) string {
	return auth.ActorFrom(r.Context()).Name
}

type v1CreateSnapshotReq struct {
	SourceSandboxID string `json:"source_sandbox_id"`
	Name            string `json:"name"`
}

// v1CreateSnapshot — POST /v1/snapshots. Synchronous: stopped-source
// check → raw cp under the source's id-lock → row. 409 if the source
// is running (cp of a live loopback would be inconsistent).
func (s *Server) v1CreateSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.LibraryRoot == "" {
		writeV1Err(w, http.StatusServiceUnavailable, "internal", "snapshots not configured on this host")
		return
	}
	var req v1CreateSnapshotReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	if req.SourceSandboxID == "" || req.Name == "" {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "source_sandbox_id and name are required")
		return
	}

	src, err := s.Store.Get(r.Context(), req.SourceSandboxID)
	if errors.Is(err, store.ErrNotFound) {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such source sandbox")
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if src.Status == "running" {
		writeV1Err(w, http.StatusConflict, "conflict",
			"source sandbox is running; stop it first (POST /v1/sandboxes/{id}/stop) then snapshot")
		return
	}

	srcImg, _ := s.Loopback.Paths(src.ID)
	if _, err := os.Stat(srcImg); err != nil {
		writeV1Err(w, http.StatusNotFound, "not_found", "source workspace image not found on disk")
		return
	}

	snapID := newULID()
	imgPath := filepath.Join(s.LibraryRoot, snapID+".img")

	// Capture under the source's id-lock so a concurrent wake can't
	// start the container and write the loopback mid-copy. The lock is
	// released before returning; we never hold it across stop/wake.
	if s.Locks != nil {
		s.Locks.Lock(src.ID)
		defer s.Locks.Unlock(src.ID)
	}
	// Re-check running under the lock (a wake could have raced in
	// between the Get above and acquiring the lock).
	if again, err := s.Store.Get(r.Context(), src.ID); err == nil && again.Status == "running" {
		writeV1Err(w, http.StatusConflict, "conflict", "source sandbox started running; stop it first")
		return
	}

	size, capErr := captureImage(r.Context(), srcImg, imgPath, s.LibraryRoot)
	if capErr != nil {
		s.loggerFor(r, src.ID).Error("snapshot capture failed", "snapshot", snapID, "err", capErr.Error())
		writeV1Err(w, http.StatusInternalServerError, "internal", "capture: "+capErr.Error())
		return
	}

	snap := &store.Snapshot{
		ID:              snapID,
		Name:            req.Name,
		OwnerToken:      tenantToken(r),
		SourceSandboxID: sql.NullString{String: src.ID, Valid: true},
		CreatedByUserID: src.ExternalUserID, // provenance only
		BaseImage:       src.Image,
		Visibility:      "private",
		Format:          "raw",
		Status:          "ready",
		ImagePath:       imgPath,
		SizeBytes:       sql.NullInt64{Int64: size, Valid: true},
	}
	if err := s.Store.CreateSnapshot(r.Context(), snap); err != nil {
		_ = os.Remove(imgPath) // roll back the orphaned image
		writeV1Err(w, http.StatusInternalServerError, "internal", "store: "+err.Error())
		return
	}
	s.auditAction(r, audit.Entry{
		Action: "snapshot.create", Target: snapID,
		Detail: map[string]any{"source_sandbox_id": src.ID, "name": req.Name},
	})
	writeJSON(w, http.StatusCreated, v1SnapshotFromRow(snap))
}

// v1ListSnapshots — GET /v1/snapshots (tenant-scoped).
func (s *Server) v1ListSnapshots(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.ListSnapshotsByOwner(r.Context(), tenantToken(r))
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]v1Snapshot, 0, len(rows))
	for _, sn := range rows {
		out = append(out, v1SnapshotFromRow(sn))
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": out})
}

// v1GetSnapshot — GET /v1/snapshots/{id} (tenant-scoped).
func (s *Server) v1GetSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshotForTenant(r, r.PathValue("id"))
	if err != nil {
		s.writeSnapshotLookupErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v1SnapshotFromRow(snap))
}

// v1DeleteSnapshot — DELETE /v1/snapshots/{id}. Removes the image file
// + row. Safe: sandboxes cloned from it are independent copies (ext4,
// no CoW), so deletion never affects them.
func (s *Server) v1DeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshotForTenant(r, r.PathValue("id"))
	if err != nil {
		s.writeSnapshotLookupErr(w, err)
		return
	}
	if err := os.Remove(snap.ImagePath); err != nil && !os.IsNotExist(err) {
		writeV1Err(w, http.StatusInternalServerError, "internal", "remove image: "+err.Error())
		return
	}
	if err := s.Store.DeleteSnapshot(r.Context(), snap.ID); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.auditAction(r, audit.Entry{Action: "snapshot.delete", Target: snap.ID})
	w.WriteHeader(http.StatusNoContent)
}

// snapshotForTenant fetches a snapshot and enforces tenant ownership.
// Returns store.ErrNotFound for both missing and cross-tenant snapshots
// (don't leak existence across tenants).
func (s *Server) snapshotForTenant(r *http.Request, id string) (*store.Snapshot, error) {
	snap, err := s.Store.GetSnapshot(r.Context(), id)
	if err != nil {
		return nil, err
	}
	if snap.OwnerToken != tenantToken(r) {
		return nil, store.ErrNotFound
	}
	return snap, nil
}

func (s *Server) writeSnapshotLookupErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such snapshot")
		return
	}
	writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
}

// captureImage copies srcImg → dst (raw, sparse-aware) crash-consistently:
// sync the host pagecache, cp --reflink=auto to a .tmp, fsync the tmp,
// atomic rename, fsync the directory. The caller guarantees no writer
// (source stopped + id-lock held). Returns the captured size in bytes.
func captureImage(ctx context.Context, srcImg, dst, root string) (int64, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return 0, err
	}
	tmp := dst + ".tmp"
	_ = os.Remove(tmp) // clear any leftover from a prior crash
	_ = exec.CommandContext(ctx, "sync").Run()
	if out, err := exec.CommandContext(ctx, "cp", "--reflink=auto", "--sparse=always", srcImg, tmp).CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		return 0, errorsWrap("cp", err, out)
	}
	if err := fsyncPath(tmp); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	_ = fsyncPath(root)
	fi, err := os.Stat(dst)
	if err != nil {
		return 0, err
	}
	return allocatedBytes(fi), nil
}

// allocatedBytes returns the real on-disk (allocated) size of a file,
// not its apparent/logical size. A snapshot .img is an 8 GB sparse file
// whose apparent size is always 8 GB; the allocated size (~the source
// workspace's real usage) is the meaningful number for a consumer and
// matches `du`. Falls back to the apparent size if the syscall stat is
// unavailable.
func allocatedBytes(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512 // st_blocks is in 512-byte units
	}
	return fi.Size()
}

func fsyncPath(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func errorsWrap(stage string, err error, out []byte) error {
	if len(out) > 0 {
		return errors.New(stage + ": " + err.Error() + ": " + string(out))
	}
	return errors.New(stage + ": " + err.Error())
}
