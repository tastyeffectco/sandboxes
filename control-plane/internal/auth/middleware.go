package auth

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
)

// Actor identifies the authenticated caller of a request. The auth
// middleware attaches it to the request context; handlers read it via
// ActorFrom to populate the audit log.
type Actor struct {
	Kind string // service | operator | system | unknown
	Name string // token name, or "loopback" for the operator path
	IP   string
}

type actorCtxKey struct{}

// WithActor stores an Actor in ctx.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorCtxKey{}, a)
}

// ActorFrom returns the Actor stored in ctx, or {Kind:"unknown"}.
func ActorFrom(ctx context.Context) Actor {
	if a, ok := ctx.Value(actorCtxKey{}).(Actor); ok {
		return a
	}
	return Actor{Kind: "unknown"}
}

// AuditWriter is the slice of the audit logger the middleware needs.
// Declared here so internal/auth does not import internal/audit
// (internal/audit imports internal/store; keeping the dependency one-
// directional avoids a cycle).
type AuditWriter interface {
	TokenInvalid(ctx context.Context, ip string)
}

// Middleware is the service-token gate for the external (Traefik-
// routed) API path. The loopback path bypasses it (roadmap §11: shell
// access to the host is the operator gate).
type Middleware struct {
	cfg   atomic.Pointer[Config]
	audit AuditWriter
	log   *slog.Logger
}

// NewMiddleware constructs the middleware around an initial config.
func NewMiddleware(initial *Config, audit AuditWriter, log *slog.Logger) *Middleware {
	m := &Middleware{audit: audit, log: log}
	m.cfg.Store(initial)
	return m
}

// Reload atomically swaps the config — the SIGHUP token-rotation path.
func (m *Middleware) Reload(c *Config) { m.cfg.Store(c) }

// Snapshot returns the current config; callers treat it as read-only.
func (m *Middleware) Snapshot() *Config { return m.cfg.Load() }

// exemptPaths are reachable on the external path without a bearer
// token (roadmap §1). /preview-auth and /forward-auth validate their
// own JWTs; /healthz and /readyz carry nothing sensitive.
var exemptPaths = map[string]bool{
	"/healthz":      true,
	"/readyz":       true,
	"/preview-auth": true,
	"/forward-auth": true,
	"/llm.txt":      true, // public API contract for integrators (no token)
}

// Wrap returns next gated by the service-token check.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := m.cfg.Load()
		ip := ClientIP(r)

		// Loopback / operator path — no token. The host's SSH lockdown
		// (Phase 0) is the gate. Traefik always sets X-Forwarded-For,
		// so a forwarded request can never look loopback here.
		if isLoopbackReq(r) {
			next.ServeHTTP(w, r.WithContext(WithActor(r.Context(),
				Actor{Kind: "operator", Name: "loopback", IP: ip})))
			return
		}

		// External (Traefik-routed) path.
		// /metrics is loopback-only — never exposed externally.
		if r.URL.Path == "/metrics" {
			http.NotFound(w, r)
			return
		}
		if exemptPaths[r.URL.Path] {
			next.ServeHTTP(w, r.WithContext(WithActor(r.Context(),
				Actor{Kind: "system", IP: ip})))
			return
		}
		if cfg.Disabled {
			// SANDBOXD_API_AUTH_DISABLED rollback path — every external
			// request runs unauthenticated. Emergency use only.
			next.ServeHTTP(w, r.WithContext(WithActor(r.Context(),
				Actor{Kind: "service", Name: "auth-disabled", IP: ip})))
			return
		}
		name, ok := MatchToken(bearerToken(r), cfg.APITokens)
		if !ok {
			if m.audit != nil {
				m.audit.TokenInvalid(r.Context(), ip)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}` + "\n"))
			return
		}
		next.ServeHTTP(w, r.WithContext(WithActor(r.Context(),
			Actor{Kind: "service", Name: name, IP: ip})))
	})
}

// bearerToken extracts the token from an `Authorization: Bearer <t>`
// header, or "" when absent / malformed.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// ClientIP returns the best-effort caller IP: the first hop of
// X-Forwarded-For when present, else the RemoteAddr host.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isLoopbackReq reports whether the request arrived directly over the
// loopback socket with no X-Forwarded-For — i.e. an on-host operator
// call, not a Traefik-forwarded one (roadmap §11).
func isLoopbackReq(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
