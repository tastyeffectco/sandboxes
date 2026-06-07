package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"syscall"

	"github.com/sandboxd/control-plane/internal/audit"
	"github.com/sandboxd/control-plane/internal/docker"
	"github.com/sandboxd/control-plane/internal/metrics"
)

// purgeOne is the irreversible per-sandbox teardown shared by all
// three purge endpoints (roadmap §6): stop + remove the container,
// unmount the loopback, delete the `.img`, delete the snapshots
// directory, and delete the `sandbox` + `workspace_owner` rows. It
// returns the bytes freed on disk and the external_user_id recorded
// on the workspace_owner row at purge time (for the audit trail).
//
// purgeOne holds the per-id lock for the whole operation so a
// concurrent wake / snapshot / restore of the same id cannot race it.
func (s *Server) purgeOne(ctx context.Context, id string) (freedBytes int64, externalUserID string, err error) {
	if s.Locks != nil {
		s.Locks.Lock(id)
		defer s.Locks.Unlock(id)
	}

	// Resolve the owner first — the workspace_owner row is about to be
	// deleted, but the audit entry needs the external_user_id.
	if wo, e := s.Store.GetWorkspaceOwner(ctx, id); e == nil {
		externalUserID = wo.ExternalUserID
	}

	// Container teardown if one exists.
	name := "s-" + id
	if _, e := s.Docker.Inspect(ctx, name); e == nil {
		_ = s.Docker.Stop(ctx, name, 10)
		if e := s.Docker.Remove(ctx, name); e != nil {
			return 0, externalUserID, fmt.Errorf("docker rm: %w", e)
		}
	} else if !errors.Is(e, docker.ErrNotFound) {
		return 0, externalUserID, fmt.Errorf("docker inspect: %w", e)
	}

	// Drop any egress nftables source entry for this sandbox.
	if s.Egress != nil {
		if sb, e := s.Store.Get(ctx, id); e == nil && sb.ContainerIP.Valid {
			_ = s.Egress.Remove(ctx, id, sb.ContainerIP.String)
		}
	}
	metrics.EgressConnections.DeleteLabelValues(id, "http")
	metrics.EgressConnections.DeleteLabelValues(id, "https")
	metrics.EgressConnections.DeleteLabelValues(id, "ssh")
	metrics.EgressConnections.DeleteLabelValues(id, "other")

	// Unmount the loopback (idempotent — a no-op if already detached).
	if e := s.Loopback.Release(ctx, id); e != nil {
		return 0, externalUserID, fmt.Errorf("loopback release: %w", e)
	}

	// Delete the workspace directory.
	imgPath, _ := s.Loopback.Paths(id)
	freedBytes += diskBytes(imgPath)
	if e := os.RemoveAll(imgPath); e != nil && !os.IsNotExist(e) {
		return freedBytes, externalUserID, fmt.Errorf("remove workspace: %w", e)
	}

	// Delete the snapshots directory _snapshots/<id>/.
	if s.SnapshotsRoot != "" {
		snapDir := filepath.Join(s.SnapshotsRoot, id)
		freedBytes += diskBytes(snapDir)
		if e := os.RemoveAll(snapDir); e != nil {
			return freedBytes, externalUserID, fmt.Errorf("remove snapshots: %w", e)
		}
	}

	// Delete the DB rows (sandbox + the durable workspace_owner row).
	if e := s.Store.PurgeSandbox(ctx, id); e != nil {
		return freedBytes, externalUserID, fmt.Errorf("purge rows: %w", e)
	}
	_ = metrics.RefreshSandboxGauge(ctx, s.Store)
	return freedBytes, externalUserID, nil
}

// diskBytes returns the on-disk size (allocated blocks, not the sparse
// apparent size) of a file or, recursively, a directory. Missing
// paths contribute 0.
func diskBytes(path string) int64 {
	var total int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			total += st.Blocks * 512
		}
		return nil
	})
	return total
}

// --- POST /sandbox/{id}/purge ---------------------------------------

func (s *Server) handlePurgeSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	freed, externalUserID, err := s.purgeOne(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "purge: "+err.Error())
		return
	}
	s.auditAction(r, audit.Entry{
		Action:         "sandbox.purge",
		Target:         id,
		ExternalUserID: externalUserID,
		Detail:         map[string]any{"freed_bytes": freed},
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"purged":      true,
		"freed_bytes": freed,
	})
}

// --- POST /external-users/{external_user_id}/purge ------------------

func (s *Server) handlePurgeExternalUser(w http.ResponseWriter, r *http.Request) {
	s.purgeScope(w, r, "user", r.PathValue("external_user_id"))
}

// --- POST /external-projects/{external_project_id}/purge ------------

func (s *Server) handlePurgeExternalProject(w http.ResponseWriter, r *http.Request) {
	s.purgeScope(w, r, "project", r.PathValue("external_project_id"))
}

// purgeScope purges every sandbox owned by an external user or
// external project. Each per-sandbox purge is audit-logged
// individually, plus one summary row (roadmap §6).
func (s *Server) purgeScope(w http.ResponseWriter, r *http.Request, scope, value string) {
	if value == "" {
		writeErr(w, http.StatusBadRequest, "missing external "+scope+" id")
		return
	}
	ids, err := s.Store.WorkspaceOwnerSandboxIDs(r.Context(), scope, value)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "lookup: "+err.Error())
		return
	}
	var totalFreed int64
	purged := 0
	for _, id := range ids {
		freed, externalUserID, err := s.purgeOne(r.Context(), id)
		if err != nil {
			// Stop on the first failure rather than leave a partial,
			// unaudited teardown silent. Already-purged sandboxes stay
			// purged; the caller retries to finish the rest.
			writeErr(w, http.StatusInternalServerError,
				fmt.Sprintf("purge %s: %v (purged %d before failing)", id, err, purged))
			return
		}
		totalFreed += freed
		purged++
		s.auditAction(r, audit.Entry{
			Action:         "sandbox.purge",
			Target:         id,
			ExternalUserID: externalUserID,
			Detail:         map[string]any{"freed_bytes": freed, "via": scope + "_purge"},
		})
	}
	summaryAction := "external_user.purge"
	if scope == "project" {
		summaryAction = "external_project.purge"
	}
	externalUserID := ""
	if scope == "user" {
		externalUserID = value
	}
	s.auditAction(r, audit.Entry{
		Action:         summaryAction,
		Target:         value,
		ExternalUserID: externalUserID,
		Detail:         map[string]any{"purged_count": purged, "freed_bytes": totalFreed},
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"purged_count": purged,
		"freed_bytes":  totalFreed,
	})
}
