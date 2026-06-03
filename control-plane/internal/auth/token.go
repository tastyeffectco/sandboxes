// Package auth implements Phase 8 service-to-service authentication
// for the sandboxd API and verification of upstream-signed preview
// tokens.
//
// Two trust classes (roadmap/phase-8 §"Steps" preamble):
//
//   - the upstream backend — bearer token on the Traefik-routed path;
//   - the operator — shell access to the host, loopback, no token.
//
// End users never call sandboxd directly; their preview access is
// gated by upstream-signed JWTs (preview_token.go) validated
// statelessly. This file holds the service-token model.
package auth

import "crypto/subtle"

// NamedToken pairs an audit name with a secret bearer-token value.
// The name is for the audit log; the token is the secret.
type NamedToken struct {
	Name  string
	Token string
}

// MatchToken constant-time compares the presented token against every
// configured token (roadmap §1: "Constant-time compare ... using
// subtle.ConstantTimeCompare"). It does NOT break early on a match so
// the loop's timing does not leak the matched position. Returns the
// matching token's audit name and true, or "" and false.
func MatchToken(presented string, tokens []NamedToken) (string, bool) {
	name := ""
	found := false
	for _, t := range tokens {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(t.Token)) == 1 {
			name = t.Name
			found = true
		}
	}
	return name, found
}
