// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// R2: a sponsored token is recognised by the client it was issued to, read at use; nothing else
// about it is trusted, and a human client's id can never name a principal.
func TestSponsored(t *testing.T) {
	sg := newSigner(t)
	id := "345@platform"
	s := &Sponsored{V: sg.verifier(), Clients: []SponsoredClient{{Name: "slack-app", ClientID: func() string { return id }}},
		Exclude: func() []string { return []string{"web-1", "roomctl-1"} }}
	tok := func(client, sub string) string {
		return sg.accessToken(t, sub, []string{"proj"}, client, nil, time.Hour)
	}
	p, err := s.Authenticate(t.Context(), tok(id, "345"))
	if err != nil || p.ID != "system:slack-app" || !p.Sponsored || p.Sub != "345" || p.ClientID != id || p.AccessToken == "" {
		t.Fatalf("%+v %v", p, err)
	}
	other := sign(t, jwt.SigningMethodRS256, sg.key, "", Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "https://evil.example",
		Subject: "345", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}, ClientID: id})
	for name, raw := range map[string]string{
		"another client": tok("roomctl-1", "345"),
		"no subject":     tok(id, ""),
		"another issuer": other,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Authenticate(t.Context(), raw); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	// signed by a key the verifier does not trust, with every claim right
	rogue := newSigner(t).accessToken(t, "345", []string{"proj"}, id, nil, time.Hour)
	if _, err := s.Authenticate(t.Context(), rogue); err == nil {
		t.Fatal("a token from an untrusted key was admitted")
	}
	id = "roomctl-1" // a rotation that lands on a human client's id must refuse, never admit roomctl's tokens
	if _, err := s.Authenticate(t.Context(), tok("roomctl-1", "345")); err == nil {
		t.Fatal("a principal sharing a human client id was admitted")
	}
	id = "" // an unreadable file refuses
	if _, err := s.Authenticate(t.Context(), tok("", "345")); err == nil {
		t.Fatal("an empty client id was admitted")
	}
}

// Two principals resolving to one client id is ambiguous: the first would win by order, so both
// refuse, and an unambiguous principal beside them still works. A missing verifier refuses too.
func TestSponsoredDuplicateClientID(t *testing.T) {
	sg := newSigner(t)
	same := func() string { return "shared@platform" }
	s := &Sponsored{V: sg.verifier(), Clients: []SponsoredClient{
		{Name: "a", ClientID: same}, {Name: "b", ClientID: same}, {Name: "c", ClientID: func() string { return "own@platform" }}}}
	tok := func(client string) string { return sg.accessToken(t, "345", []string{"proj"}, client, nil, time.Hour) }
	if p, err := s.Authenticate(t.Context(), tok("shared@platform")); err == nil {
		t.Fatalf("an ambiguous client id was admitted as %s", p.ID)
	}
	if p, err := s.Authenticate(t.Context(), tok("own@platform")); err != nil || p.ID != "system:c" {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := (&Sponsored{Clients: s.Clients}).Authenticate(t.Context(), tok("own@platform")); err == nil {
		t.Fatal("no verifier admitted")
	}
}
