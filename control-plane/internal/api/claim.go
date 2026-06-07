package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/sandboxd/control-plane/internal/audit"
	"github.com/sandboxd/control-plane/internal/store"
)

// claimReq is the body of POST /sandbox/{id}/claim.
type claimReq struct {
	ExternalUserID      string `json:"external_user_id"`
	ExternalProjectID   string `json:"external_project_id,omitempty"`
	ExternalWorkspaceID string `json:"external_workspace_id,omitempty"`
}

// handleClaim moves a (typically legacy / back-filled) sandbox to a
// real upstream identity. roadmap §3: it updates external identity on
// both `sandbox` and the durable `workspace_owner` row and audit-logs
// the transition. Service-token gated by the auth middleware.
func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req claimReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if req.ExternalUserID == "" {
		writeErr(w, http.StatusBadRequest, "missing external_user_id")
		return
	}
	for _, f := range []struct{ label, val string }{
		{"external_user_id", req.ExternalUserID},
		{"external_project_id", req.ExternalProjectID},
		{"external_workspace_id", req.ExternalWorkspaceID},
	} {
		if !validExternalID(f.val) {
			writeErr(w, http.StatusBadRequest,
				"invalid "+f.label+": must be <=256 chars, no control codes, no commas")
			return
		}
	}

	// Capture the prior owner for the audit detail (may be absent on a
	// legacy sandbox that never had a workspace_owner row).
	prior := ""
	if wo, err := s.Store.GetWorkspaceOwner(r.Context(), id); err == nil {
		prior = wo.ExternalUserID
	}

	if err := s.Store.Claim(r.Context(), id,
		req.ExternalUserID, req.ExternalProjectID, req.ExternalWorkspaceID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "no sandbox row for that id")
			return
		}
		writeErr(w, http.StatusInternalServerError, "claim: "+err.Error())
		return
	}

	s.auditAction(r, audit.Entry{
		Action:         "sandbox.claim",
		Target:         id,
		ExternalUserID: req.ExternalUserID,
		Detail: map[string]any{
			"prior_external_user_id": prior,
			"new_external_user_id":   req.ExternalUserID,
		},
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"id":               id,
		"external_user_id": req.ExternalUserID,
		"claimed":          true,
	})
}
