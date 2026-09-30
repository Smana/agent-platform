// SPDX-License-Identifier: Apache-2.0

package runwatch

import (
	"crypto/rand"
	"encoding/json"
	"math/big"
	"slices"
	"strings"
	"testing"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/redact"
)

// githubPAT is a fresh token-shaped secret: random, since gitleaks' rule checks
// entropy, and never a literal in the repository.
func githubPAT(t *testing.T) string {
	const set = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 36)
	for i := range b {
		k, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		if err != nil {
			t.Fatal(err)
		}
		b[i] = set[k.Int64()]
	}
	return "ghp_" + string(b)
}

// Review M10: a revocation's reason is the claim's annotation as written, free
// text of up to 256 KiB. It reaches the log redacted and cut, and never as sent.
func TestARevocationReasonIsRedactedAndCut(t *testing.T) {
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	pat := githubPAT(t)
	annotation := "stopped by the owner, token=" + pat + " " + strings.Repeat("x", 256<<10)
	r, _ := FromUnstructured(claim("xplane-run-7f3cq2xz", "3kq7x2ma", "BudgetExhausted", annotation))
	redacted := "stopped by the owner, token=[REDACTED:github-pat] "
	redacted += strings.Repeat("x", 64-len(redacted))
	cases := []struct {
		name       string
		redactor   Redactor
		want       string
		redactions []string
	}{
		{"redacted, then cut", red, redacted, []string{"github-pat"}},
		{"no redactor: the text is dropped", nil, "revoked", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &fakeStore{keys: map[string]bool{}}
			e := &Events{Store: fs, Redactor: c.redactor}
			if err := e.Observe(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(fs.drafts, func(d envelope.Draft) bool { return strings.Contains(string(d.Payload), `"BudgetExhausted"`) })
			if i < 0 {
				t.Fatalf("no end event in %v", payloads(fs.drafts))
			}
			var p struct{ Reason string }
			if err := json.Unmarshal(fs.drafts[i].Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.Reason != c.want || len(p.Reason) > maxReasonBytes {
				t.Fatalf("reason = %.100q (%d bytes), want %q", p.Reason, len(p.Reason), c.want)
			}
			if !slices.Equal(fs.drafts[i].Redactions, c.redactions) {
				t.Fatalf("redactions = %v, want %v", fs.drafts[i].Redactions, c.redactions)
			}
			for _, d := range fs.drafts {
				if strings.Contains(string(d.Payload), pat) {
					t.Fatalf("the token reached the log: %s", d.Payload)
				}
			}
		})
	}
}
