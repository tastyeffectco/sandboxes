package api

import (
	"errors"
	"html"
	"io"
	"net/http"
	"time"

	"github.com/sandboxd/control-plane/internal/audit"
	"github.com/sandboxd/control-plane/internal/auth"
	"github.com/sandboxd/control-plane/internal/metrics"
	"github.com/sandboxd/control-plane/internal/store"
)

// handleForwardAuth is called by Traefik's forwardAuth middleware on
// every request to a private sandbox (roadmap §8). A 2xx response
// means allow; the deny path is a 302 (or, with
// SANDBOXD_FORWARD_AUTH_DENY_MODE=meta-refresh, a 401 + <meta refresh>
// fallback) to the upstream auth URL. Reachable externally without the
// service token — it validates its own cookie JWT.
func (s *Server) handleForwardAuth(w http.ResponseWriter, r *http.Request) {
	// Phase 9 step 8 — time every /forward-auth invocation. This is the
	// hot path Traefik calls on every request to a private sandbox; the
	// capacity report's forward-auth p95 (< 50 ms target) reads this.
	start := time.Now()
	defer func() { metrics.ForwardAuthDuration.Observe(time.Since(start).Seconds()) }()

	fwdHost := r.Header.Get("X-Forwarded-Host")
	fwdURI := r.Header.Get("X-Forwarded-Uri")

	id := parseSandboxIDFromHost(fwdHost, s.PreviewDomain)
	if id == "" {
		// Unparseable host — deny, no redirect (roadmap §8 step 1).
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	sb, err := s.Store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeForwardAuthError(w, http.StatusNotFound, id, "no such sandbox")
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	// Public sandboxes should never route through forward-auth; allow
	// as a no-op safety net (roadmap §8 step 2).
	if sb.Visibility != "private" {
		metrics.PreviewAccess.WithLabelValues("allowed").Inc()
		w.WriteHeader(http.StatusOK)
		return
	}

	ownerUID := ""
	if wo, e := s.Store.GetWorkspaceOwner(r.Context(), id); e == nil {
		ownerUID = wo.ExternalUserID
	}
	cookieVal := ""
	if c, e := r.Cookie("sandbox_preview"); e == nil {
		cookieVal = c.Value
	}
	claims, reason := auth.CheckPreviewAccess(cookieVal, id, ownerUID,
		s.authCfg().PreviewSecrets, time.Now())
	if reason != "" {
		metrics.PreviewAccess.WithLabelValues("denied").Inc()
		s.auditAction(r, audit.Entry{
			Action:         "preview.access_denied",
			Target:         id,
			ExternalUserID: ownerUID,
			Detail:         map[string]any{"reason": reason},
		})
		s.forwardAuthDeny(w, r, id, fwdHost, fwdURI)
		return
	}

	// Allowed. Forward the viewer identity to Traefik (authResponse-
	// Headers in auth.yml passes it on to the sandbox).
	metrics.PreviewAccess.WithLabelValues("allowed").Inc()
	w.Header().Set("X-Sandbox-External-User-Id", claims.Sub)
	if s.Audit != nil {
		s.Audit.WriteSampled(r.Context(), claims.Sub+"|"+id, audit.Entry{
			ActorKind:      "system",
			ExternalUserID: claims.Sub,
			Action:         "preview.access_allowed",
			Target:         id,
		})
	}
	w.WriteHeader(http.StatusOK)
}

// forwardAuthDeny issues the deny response. Primary strategy: a 302 to
// the upstream auth URL. SANDBOXD_FORWARD_AUTH_DENY_MODE=meta-refresh
// switches to the documented 401 + <meta refresh> fallback for Traefik
// builds that do not pass 3xx from the auth service through cleanly
// (roadmap §8 / §Risks "Traefik forward-auth 3xx passthrough").
func (s *Server) forwardAuthDeny(w http.ResponseWriter, r *http.Request, id, fwdHost, fwdURI string) {
	cfg := s.authCfg()
	returnURL := "https://" + fwdHost + fwdURI
	if cfg.AuthRedirectURL == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	target := auth.BuildRedirectURL(cfg.AuthRedirectURL, id, returnURL)
	if s.ForwardAuthDenyMode == "meta-refresh" {
		w.Header().Set("Location", target)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, metaRefreshPage(target))
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func writeForwardAuthError(w http.ResponseWriter, code int, id, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, "<!doctype html><html><head><meta charset=\"utf-8\">"+
		"<title>"+html.EscapeString(msg)+"</title></head><body><h1>"+
		html.EscapeString(msg)+"</h1><p>sandbox "+html.EscapeString(id)+
		"</p></body></html>\n")
}

func metaRefreshPage(target string) string {
	esc := html.EscapeString(target)
	return "<!doctype html><html><head><meta charset=\"utf-8\">" +
		"<meta http-equiv=\"refresh\" content=\"0;url=" + esc + "\">" +
		"<title>Redirecting…</title></head><body>" +
		"<p>Redirecting you to sign in… <a href=\"" + esc + "\">continue</a></p>" +
		"</body></html>\n"
}
