package redact

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Random alphanumerics: gitleaks' GitHub rules check entropy, so a repeated
// pattern would not fire and the test would pass for the wrong reason.
func alnum(t *testing.T, n int) string {
	const set = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		k, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		if err != nil {
			t.Fatal(err)
		}
		b[i] = set[k.Int64()]
	}
	return string(b)
}

func secrets(t *testing.T) map[string]string {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Subject: "system:serviceaccount:agents:xplane-run-7f3cq2xz", Audience: jwt.ClaimStrings{"room-broker"},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	return map[string]string{
		"github-app-token": "ghs_" + alnum(t, 36), // octo-sts installation token
		"github-pat":       "ghp_" + alnum(t, 36),
		"jwt":              signed, // ServiceAccount and ZITADEL tokens
		"private-key":      pemKey,
	}
}

func TestTheFourPinnedRules(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for rule, secret := range secrets(t) {
		out, rules := r.String("tool output: " + secret + " (end)")
		if strings.Contains(out, secret) {
			t.Errorf("%s: secret survived", rule)
		}
		if !slices.Contains(rules, rule) {
			t.Errorf("%s: fired %v", rule, rules)
		}
		if !strings.Contains(out, "[REDACTED:"+rule+"]") {
			t.Errorf("%s: no marker in %q", rule, out)
		}
	}
}

func TestPayloadKeepsItsShape(t *testing.T) {
	r, _ := New()
	s := secrets(t)
	in := map[string]any{"callId": "c1", "output": "token=" + s["github-app-token"],
		"nested": []any{map[string]any{"env": s["jwt"]}}, "bytes": 12}
	raw, _ := json.Marshal(in)
	out, rules, err := r.Payload(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), s["github-app-token"]) || strings.Contains(string(out), s["jwt"]) {
		t.Fatalf("secret survived: %s", out)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil || back["callId"] != "c1" || back["bytes"] != float64(12) {
		t.Fatalf("shape changed: %s", out)
	}
	if !slices.Contains(rules, "github-app-token") || !slices.Contains(rules, "jwt") {
		t.Fatalf("rules = %v", rules)
	}
}

// Review I6: jsonb refuses \u0000 (SQLSTATE 22P05), and one refused event would stall a
// bridge's cursor for good, so NULs are stripped from every string and every key.
func TestNULsAreStripped(t *testing.T) {
	r, _ := New()
	out, _, err := r.Payload([]byte(`{"output":"a\u0000b","k\u0000":1}`))
	if err != nil || string(out) != `{"k":1,"output":"ab"}` {
		t.Fatalf("%s %v", out, err)
	}
}

func TestCleanTextIsUntouched(t *testing.T) {
	r, _ := New()
	in := "Fixed the broken link in docs/README.md"
	if out, rules := r.String(in); out != in || len(rules) != 0 {
		t.Fatalf("clean text changed: %q %v", out, rules)
	}
}
