package auth

import (
	"net/url"
	"os"
	"strings"
)

// Config is the reloadable auth configuration. It is held behind an
// atomic.Pointer in Middleware so a SIGHUP can swap it without locks.
type Config struct {
	APITokens       []NamedToken      // SANDBOXD_API_TOKENS
	PreviewSecrets  map[string]string // SANDBOXD_PREVIEW_TOKEN_SECRETS: kid -> secret
	AuthRedirectURL string            // SANDBOXD_AUTH_REDIRECT_URL
	Disabled        bool              // SANDBOXD_API_AUTH_DISABLED rollback path
}

// ParseConfig builds a Config from a key->value getter. At startup the
// getter is os.Getenv (systemd has already loaded the EnvironmentFile
// into the process environment); on SIGHUP it is a map populated by
// LoadEnvFile, because the process environment is stale by then.
func ParseConfig(get func(string) string) *Config {
	c := &Config{
		APITokens:       parseNamedPairs(get("SANDBOXD_API_TOKENS")),
		PreviewSecrets:  pairsToMap(get("SANDBOXD_PREVIEW_TOKEN_SECRETS")),
		AuthRedirectURL: strings.TrimSpace(get("SANDBOXD_AUTH_REDIRECT_URL")),
	}
	switch strings.ToLower(strings.TrimSpace(get("SANDBOXD_API_AUTH_DISABLED"))) {
	case "", "0", "false", "no":
		c.Disabled = false
	default:
		c.Disabled = true
	}
	return c
}

// LoadEnvFile parses a systemd EnvironmentFile-style file (KEY=value
// per line, `#` comments, optional surrounding quotes on the value)
// into a map. Used by the SIGHUP reload path.
func LoadEnvFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		m[key] = val
	}
	return m, nil
}

// MapGetter adapts a map to the func(string)string ParseConfig wants.
func MapGetter(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// BuildRedirectURL substitutes {sandbox_id} and {return} (URL-encoded)
// into the SANDBOXD_AUTH_REDIRECT_URL template.
func BuildRedirectURL(template, sandboxID, returnURL string) string {
	return strings.NewReplacer(
		"{sandbox_id}", url.QueryEscape(sandboxID),
		"{return}", url.QueryEscape(returnURL),
	).Replace(template)
}

// parseNamedPairs splits a "name=value,name=value" list. Whitespace
// around each element and around `name`/`value` is trimmed; empty
// elements and elements without an `=` are skipped.
func parseNamedPairs(s string) []NamedToken {
	var out []NamedToken
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.IndexByte(part, '=')
		if eq <= 0 {
			continue
		}
		out = append(out, NamedToken{
			Name:  strings.TrimSpace(part[:eq]),
			Token: strings.TrimSpace(part[eq+1:]),
		})
	}
	return out
}

func pairsToMap(s string) map[string]string {
	m := map[string]string{}
	for _, nt := range parseNamedPairs(s) {
		m[nt.Name] = nt.Token
	}
	return m
}
