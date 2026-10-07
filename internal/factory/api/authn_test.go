// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/policy"
)

type signer struct{ key *rsa.PrivateKey }

func newSigner(t *testing.T) signer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return signer{k}
}

func (s signer) verifier(iss string) *authn.Verifier {
	return authn.NewVerifierWithKeyfunc(iss, func(*jwt.Token) (any, error) { return &s.key.PublicKey, nil })
}

func (s signer) token(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	raw, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(s.key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAuthenticate(t *testing.T) {
	zitadel, eks := newSigner(t), newSigner(t)
	a := &Authenticator{Humans: zitadel.verifier("https://auth.ogenki.io"),
		Groups:      policy.Groups{Admin: "agents-admin", Member: "agents-member"},
		ClientIDs:   func() []string { return []string{"rooms-proxy-id", "roomctl-id"} },
		Systems:     eks.verifier("https://oidc.eks"),
		SystemAllow: map[string]string{"system:serviceaccount:agent-system:runner": "system:runner"}}
	call := func(tok string) (authn.Principal, error) {
		r := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/runs", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		return a.Authenticate(r)
	}
	human := zitadel.token(t, jwt.MapClaims{"iss": "https://auth.ogenki.io", "sub": "291847362183", "azp": "roomctl-id", "groups": []string{"agents-member"}})
	p, err := call(human)
	if err != nil || p.ID != "human:291847362183" || p.ClientID != "roomctl-id" || p.AccessToken != human {
		t.Fatalf("%+v %v", p, err)
	}
	stranger := zitadel.token(t, jwt.MapClaims{"iss": "https://auth.ogenki.io", "sub": "1", "azp": "roomctl-id", "groups": []string{"backend"}})
	if _, err := call(stranger); !errors.Is(err, authn.ErrForbidden) {
		t.Fatalf("outside the agents groups: %v", err)
	}
	// What the broker forwards and roomctl sends: a ZITADEL JWT access token,
	// which names its client in client_id and carries no azp (review C1).
	access := zitadel.token(t, jwt.MapClaims{"iss": "https://auth.ogenki.io", "sub": "291847362183", "client_id": "rooms-proxy-id",
		"groups": []string{"agents-member"}})
	if p, err := call(access); err != nil || p.ID != "human:291847362183" || p.ClientID != "rooms-proxy-id" || p.AccessToken != access {
		t.Fatalf("an access token: %+v %v", p, err)
	}
	otherAccess := zitadel.token(t, jwt.MapClaims{"iss": "https://auth.ogenki.io", "sub": "1", "client_id": "grafana", "groups": []string{"agents-admin"}})
	if _, err := call(otherAccess); !errors.Is(err, authn.ErrUnauthenticated) {
		t.Fatalf("an access token issued to another client: %v", err)
	}
	otherClient := zitadel.token(t, jwt.MapClaims{"iss": "https://auth.ogenki.io", "sub": "1", "azp": "grafana", "groups": []string{"agents-admin"}})
	if _, err := call(otherClient); !errors.Is(err, authn.ErrUnauthenticated) {
		t.Fatalf("a token issued to another client: %v", err)
	}
	sys := eks.token(t, jwt.MapClaims{"iss": "https://oidc.eks", "sub": "system:serviceaccount:agent-system:runner", "aud": AudienceSystem})
	if p, err := call(sys); err != nil || p.ID != "system:runner" {
		t.Fatalf("%+v %v", p, err)
	}
	rogue := eks.token(t, jwt.MapClaims{"iss": "https://oidc.eks", "sub": "system:serviceaccount:default:x", "aud": AudienceSystem})
	if _, err := call(rogue); !errors.Is(err, authn.ErrForbidden) {
		t.Fatalf("an unlisted ServiceAccount: %v", err)
	}
	if _, err := call("garbage"); !errors.Is(err, authn.ErrUnauthenticated) {
		t.Fatal(err)
	}
}
