package api

import (
	"regexp"
	"strings"
	"sync"

	"github.com/sandboxd/control-plane/internal/auth"
)

// authCfg returns the current auth config snapshot, or an empty config
// when auth is not wired (nil-safe for tests / partial wiring).
func (s *Server) authCfg() *auth.Config {
	if s.Auth != nil {
		if c := s.Auth.Snapshot(); c != nil {
			return c
		}
	}
	return &auth.Config{}
}

// reCache memoizes the per-domain regexes so /forward-auth (called on
// every request to a private sandbox) does not recompile them.
var reCache sync.Map // key -> *regexp.Regexp

func cachedRE(key, pattern string) *regexp.Regexp {
	if v, ok := reCache.Load(key); ok {
		return v.(*regexp.Regexp)
	}
	re := regexp.MustCompile(pattern)
	reCache.Store(key, re)
	return re
}

// parseSandboxIDFromHost extracts the sandbox id from a preview host
// `s-<id>-<port>.preview.<domain>` (optional :port), or "" if the host
// does not match the preview shape.
func parseSandboxIDFromHost(host, domain string) string {
	re := cachedRE("host|"+domain,
		`^s-([0-9A-Za-z]+)-[0-9]+\.preview\.`+regexp.QuoteMeta(domain)+`(?::\d+)?$`)
	m := re.FindStringSubmatch(strings.TrimSpace(host))
	if m == nil {
		return ""
	}
	return m[1]
}

// returnSandboxRE matches an allowed `s-<id>-<port>` preview return URL
// and captures the id.
func returnSandboxRE(domain string) *regexp.Regexp {
	return cachedRE("ret-sb|"+domain,
		`^https://s-([0-9A-Za-z]+)-[0-9]+\.preview\.`+regexp.QuoteMeta(domain)+`(/.*)?$`)
}

// validateReturnURL enforces roadmap §8 step 2 (no open redirects: the
// URL must be a preview host or the api.preview host) and step 3 (an
// `s-<id>` return host must carry the same sandbox_id as the JWT).
func validateReturnURL(returnURL, jwtSandboxID, domain string) bool {
	if m := returnSandboxRE(domain).FindStringSubmatch(returnURL); m != nil {
		return m[1] == jwtSandboxID
	}
	apiRE := cachedRE("ret-api|"+domain,
		`^https://api\.preview\.`+regexp.QuoteMeta(domain)+`(/.*)?$`)
	return apiRE.MatchString(returnURL)
}

// parseReturnSandboxID best-effort extracts a sandbox id from a return
// URL, for the redirect-on-failure path (sandbox_id "if discoverable").
func parseReturnSandboxID(returnURL, domain string) string {
	if m := returnSandboxRE(domain).FindStringSubmatch(returnURL); m != nil {
		return m[1]
	}
	return ""
}
