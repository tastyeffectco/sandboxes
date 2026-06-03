package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// PreviewAudience is the required `aud` claim on a preview token.
const PreviewAudience = "sandbox-preview"

// Verification errors. CheckPreviewAccess buckets these into the
// machine-readable forward-auth denial reasons.
var (
	ErrTokenMalformed  = errors.New("preview token malformed")
	ErrTokenBadAlg     = errors.New("preview token alg is not HS256")
	ErrTokenUnknownKid = errors.New("preview token kid not configured")
	ErrTokenBadSig     = errors.New("preview token signature invalid")
	ErrTokenExpired    = errors.New("preview token expired")
	ErrTokenBadAud     = errors.New("preview token aud mismatch")
)

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// PreviewClaims is the validated payload of an upstream-signed preview
// token (roadmap §7). sandboxd verifies the signature, exp and aud; it
// does not check `sub` beyond presence — the upstream has already
// decided this viewer may see this sandbox.
type PreviewClaims struct {
	Iss       string `json:"iss"`
	Iat       int64  `json:"iat"`
	Exp       int64  `json:"exp"`
	Aud       string `json:"aud"`
	Sub       string `json:"sub"`
	SandboxID string `json:"sandbox_id"`
	Kid       string `json:"-"` // copied from the JWS header
}

// VerifyPreviewToken parses and verifies an HS256 JWS. `secrets` maps
// the `kid` header to the shared HMAC secret; `now` is the reference
// time for the `exp` check. The token format is the standard compact
// JWS (`header.payload.signature`, each base64url, no padding).
func VerifyPreviewToken(token string, secrets map[string]string, now time.Time) (*PreviewClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrTokenMalformed
	}
	hdrRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrTokenMalformed
	}
	var hdr jwtHeader
	if err := json.Unmarshal(hdrRaw, &hdr); err != nil {
		return nil, ErrTokenMalformed
	}
	if !strings.EqualFold(hdr.Alg, "HS256") {
		return nil, ErrTokenBadAlg
	}
	secret, ok := secrets[hdr.Kid]
	if !ok || secret == "" {
		return nil, ErrTokenUnknownKid
	}

	// Recompute HMAC-SHA256 over the signing input and constant-time
	// compare against the presented signature.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrTokenMalformed
	}
	if subtle.ConstantTimeCompare(want, got) != 1 {
		return nil, ErrTokenBadSig
	}

	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrTokenMalformed
	}
	var c PreviewClaims
	if err := json.Unmarshal(payloadRaw, &c); err != nil {
		return nil, ErrTokenMalformed
	}
	c.Kid = hdr.Kid
	if c.Aud != PreviewAudience {
		return nil, ErrTokenBadAud
	}
	if c.Exp <= now.Unix() {
		return nil, ErrTokenExpired
	}
	return &c, nil
}

// CheckPreviewAccess is the shared access decision used by both
// GET /forward-auth and the private-sandbox wake path. It returns the
// validated claims and "" when access is allowed, or nil and a
// machine-readable denial reason (one of: no_cookie, bad_signature,
// expired, wrong_sandbox, wrong_user — roadmap §8).
//
// ownerExternalUserID is workspace_owner.external_user_id for the
// sandbox; an empty string skips the owner check (used when the owner
// row is absent, e.g. an un-backfilled legacy private sandbox — the
// signature + sandbox-id checks still apply).
func CheckPreviewAccess(cookieVal, sandboxID, ownerExternalUserID string, secrets map[string]string, now time.Time) (*PreviewClaims, string) {
	if cookieVal == "" {
		return nil, "no_cookie"
	}
	claims, err := VerifyPreviewToken(cookieVal, secrets, now)
	if err != nil {
		if errors.Is(err, ErrTokenExpired) {
			return nil, "expired"
		}
		// malformed / bad alg / unknown kid / bad sig / bad aud all
		// collapse to the single "bad_signature" bucket.
		return nil, "bad_signature"
	}
	if claims.SandboxID != sandboxID {
		return nil, "wrong_sandbox"
	}
	if ownerExternalUserID != "" && claims.Sub != ownerExternalUserID {
		return nil, "wrong_user"
	}
	return claims, ""
}
