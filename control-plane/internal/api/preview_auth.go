package api

import (
	"net/http"
	"time"

	"github.com/sandboxd/control-plane/internal/audit"
	"github.com/sandboxd/control-plane/internal/auth"
)

// handlePreviewAuth is the landing endpoint for a freshly minted
// upstream preview token (roadmap §8). It validates the HS256 JWS,
// sets the `sandbox_preview` cookie on `.preview.<domain>`, and 302s
// to the allowlisted `return` URL. Reachable externally without the
// service token (the auth middleware exempts it) — it validates its
// own JWT.
func (s *Server) handlePreviewAuth(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tokenStr := q.Get("token")
	returnURL := q.Get("return")
	cfg := s.authCfg()

	claims, err := auth.VerifyPreviewToken(tokenStr, cfg.PreviewSecrets, time.Now())
	if err != nil {
		// Do not surface the validation reason to the browser — 302
		// the user to the upstream auth URL to obtain a fresh token.
		sbID := parseReturnSandboxID(returnURL, s.PreviewDomain)
		s.auditAction(r, audit.Entry{
			Action: "preview.access_denied",
			Target: sbID,
			Detail: map[string]any{"reason": "preview_auth_token_invalid"},
		})
		s.redirectToUpstreamAuth(w, r, cfg, sbID, returnURL)
		return
	}
	if !validateReturnURL(returnURL, claims.SandboxID, s.PreviewDomain) {
		// An attacker-supplied `return` cannot point anywhere outside
		// this sandbox's own preview host or the api.preview host.
		writeErr(w, http.StatusBadRequest, "return url not allowed for this sandbox")
		return
	}

	maxAge := int(claims.Exp - time.Now().Unix())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "sandbox_preview",
		Value:    tokenStr,
		Path:     "/",
		Domain:   ".preview." + s.PreviewDomain,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	s.auditAction(r, audit.Entry{
		Action:         "preview.session_issued",
		Target:         claims.SandboxID,
		ExternalUserID: claims.Sub,
	})
	http.Redirect(w, r, returnURL, http.StatusFound)
}

// redirectToUpstreamAuth 302s the browser to SANDBOXD_AUTH_REDIRECT_URL
// with {sandbox_id} and {return} substituted. When no redirect URL is
// configured it falls back to a plain 401.
func (s *Server) redirectToUpstreamAuth(w http.ResponseWriter, r *http.Request, cfg *auth.Config, sandboxID, returnURL string) {
	if cfg.AuthRedirectURL == "" {
		writeErr(w, http.StatusUnauthorized, "preview token invalid and no auth redirect configured")
		return
	}
	http.Redirect(w, r,
		auth.BuildRedirectURL(cfg.AuthRedirectURL, sandboxID, returnURL),
		http.StatusFound)
}
