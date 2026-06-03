package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// signHS256 mints a compact JWS the way the upstream backend (and
// ops/tools/sign-preview-token.sh) does — used only by these tests.
func signHS256(t *testing.T, kid, secret string, claims map[string]any) string {
	t.Helper()
	enc := base64.RawURLEncoding
	hb, _ := json.Marshal(map[string]any{"alg": "HS256", "typ": "JWT", "kid": kid})
	cb, _ := json.Marshal(claims)
	signingInput := enc.EncodeToString(hb) + "." + enc.EncodeToString(cb)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	return signingInput + "." + enc.EncodeToString(mac.Sum(nil))
}

func claims(sandboxID, sub string, exp time.Time) map[string]any {
	return map[string]any{
		"iss":        "upstream-prod",
		"iat":        time.Now().Add(-time.Minute).Unix(),
		"exp":        exp.Unix(),
		"aud":        PreviewAudience,
		"sub":        sub,
		"sandbox_id": sandboxID,
	}
}

func TestVerifyPreviewToken_Valid(t *testing.T) {
	secrets := map[string]string{"v1": "0123456789abcdef0123456789abcdef"}
	tok := signHS256(t, "v1", secrets["v1"], claims("sb1", "user-alice", time.Now().Add(time.Hour)))
	c, err := VerifyPreviewToken(tok, secrets, time.Now())
	if err != nil {
		t.Fatalf("want valid, got err %v", err)
	}
	if c.SandboxID != "sb1" || c.Sub != "user-alice" || c.Kid != "v1" {
		t.Fatalf("unexpected claims: %+v", c)
	}
}

func TestVerifyPreviewToken_Rejections(t *testing.T) {
	good := "0123456789abcdef0123456789abcdef"
	secrets := map[string]string{"v1": good}

	expired := signHS256(t, "v1", good, claims("sb1", "u", time.Now().Add(-time.Minute)))
	if _, err := VerifyPreviewToken(expired, secrets, time.Now()); err != ErrTokenExpired {
		t.Errorf("expired: want ErrTokenExpired, got %v", err)
	}

	wrongSecret := signHS256(t, "v1", "wrongwrongwrongwrongwrongwrongwr", claims("sb1", "u", time.Now().Add(time.Hour)))
	if _, err := VerifyPreviewToken(wrongSecret, secrets, time.Now()); err != ErrTokenBadSig {
		t.Errorf("wrong secret: want ErrTokenBadSig, got %v", err)
	}

	unknownKid := signHS256(t, "v9", good, claims("sb1", "u", time.Now().Add(time.Hour)))
	if _, err := VerifyPreviewToken(unknownKid, secrets, time.Now()); err != ErrTokenUnknownKid {
		t.Errorf("unknown kid: want ErrTokenUnknownKid, got %v", err)
	}

	badAud := claims("sb1", "u", time.Now().Add(time.Hour))
	badAud["aud"] = "something-else"
	if _, err := VerifyPreviewToken(signHS256(t, "v1", good, badAud), secrets, time.Now()); err != ErrTokenBadAud {
		t.Errorf("bad aud: want ErrTokenBadAud, got %v", err)
	}

	if _, err := VerifyPreviewToken("not.a.jwt.at.all", secrets, time.Now()); err != ErrTokenMalformed {
		t.Errorf("malformed: want ErrTokenMalformed, got %v", err)
	}
}

func TestCheckPreviewAccess(t *testing.T) {
	good := "0123456789abcdef0123456789abcdef"
	secrets := map[string]string{"v1": good}
	now := time.Now()
	tok := signHS256(t, "v1", good, claims("sb1", "user-alice", now.Add(time.Hour)))

	cases := []struct {
		name, cookie, sandboxID, owner, wantReason string
	}{
		{"allowed", tok, "sb1", "user-alice", ""},
		{"no cookie", "", "sb1", "user-alice", "no_cookie"},
		{"wrong sandbox", tok, "sb2", "user-alice", "wrong_sandbox"},
		{"wrong user", tok, "sb1", "user-bob", "wrong_user"},
		{"garbage cookie", "x.y.z", "sb1", "user-alice", "bad_signature"},
	}
	for _, c := range cases {
		_, reason := CheckPreviewAccess(c.cookie, c.sandboxID, c.owner, secrets, now)
		if reason != c.wantReason {
			t.Errorf("%s: want reason %q, got %q", c.name, c.wantReason, reason)
		}
	}
}

func TestMatchToken(t *testing.T) {
	toks := []NamedToken{{Name: "prod", Token: "aaa"}, {Name: "prod-next", Token: "bbb"}}
	if name, ok := MatchToken("bbb", toks); !ok || name != "prod-next" {
		t.Errorf("want prod-next/true, got %q/%v", name, ok)
	}
	if _, ok := MatchToken("ccc", toks); ok {
		t.Errorf("unknown token must not match")
	}
	if _, ok := MatchToken("", toks); ok {
		t.Errorf("empty token must not match")
	}
}

func TestParseConfig(t *testing.T) {
	env := map[string]string{
		"SANDBOXD_API_TOKENS":            "prod=tok1, prod-next=tok2",
		"SANDBOXD_PREVIEW_TOKEN_SECRETS": "v1=sec1,v2=sec2",
		"SANDBOXD_AUTH_REDIRECT_URL":     "https://app/x?sandbox_id={sandbox_id}&return={return}",
		"SANDBOXD_API_AUTH_DISABLED":     "1",
	}
	c := ParseConfig(MapGetter(env))
	if len(c.APITokens) != 2 || c.APITokens[0].Name != "prod" || c.APITokens[0].Token != "tok1" {
		t.Fatalf("api tokens parsed wrong: %+v", c.APITokens)
	}
	if c.PreviewSecrets["v2"] != "sec2" {
		t.Fatalf("preview secrets parsed wrong: %+v", c.PreviewSecrets)
	}
	if !c.Disabled {
		t.Errorf("SANDBOXD_API_AUTH_DISABLED=1 should disable auth")
	}
	got := BuildRedirectURL(c.AuthRedirectURL, "sb1", "https://s-sb1-3000.preview.example.com/")
	want := "https://app/x?sandbox_id=sb1&return=https%3A%2F%2Fs-sb1-3000.preview.example.com%2F"
	if got != want {
		t.Errorf("BuildRedirectURL\n got %q\nwant %q", got, want)
	}
}
