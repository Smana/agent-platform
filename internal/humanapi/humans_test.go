// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/fanout"
)

// The real authenticator on the upgrade (2.2): a web session is an ID token in
// Authorization and a JWT access token in authn.ForwardedAccessHeader, from the
// configured origin, and it is the web UI.
func TestUpgradeWithZITADELTokens(t *testing.T) {
	const (
		issuer  = "https://zitadel.example.test"
		project = "project-1"
		web     = "web-client"
		origin  = "https://rooms.example.test"
		host    = "rooms.example.test"
	)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	mint := func(c authn.Claims) string {
		c.Issuer, c.Subject, c.Audience = issuer, "2918", jwt.ClaimStrings{web, project}
		c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(time.Hour))
		raw, err := jwt.NewWithClaims(jwt.SigningMethodRS256, c).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	id := mint(authn.Claims{AuthorizedParty: web, ClientID: web, Groups: []string{member}})
	access := mint(authn.Claims{ClientID: web})
	v := authn.NewVerifierWithKeyfunc(issuer, func(*jwt.Token) (any, error) { return &key.PublicKey, nil })
	constant := func(s string) func() string { return func() string { return s } }
	e := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) {
		s.Humans = authn.NewHumans(v, constant(project), constant(web), constant(""), origin)
		s.WebClient = constant(web)
	})
	session := func(og string) http.Header {
		h := http.Header{}
		h.Set("Authorization", "Bearer "+id)
		h.Set(authn.ForwardedAccessHeader, access)
		h.Set("Origin", og)
		return h
	}
	// dialAs returns the connection, or the status that refused it.
	dialAs := func(h http.Header) (*websocket.Conn, int, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		c, resp, err := websocket.Dial(ctx, wsURL(e.ts), &websocket.DialOptions{HTTPHeader: h, Host: host})
		if c != nil {
			t.Cleanup(func() { _ = c.CloseNow() })
		}
		code := 0
		if resp != nil {
			code = resp.StatusCode
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
		}
		return c, code, err
	}

	c, _, err := dialAs(session(origin))
	if err != nil {
		t.Fatal(err)
	}
	send(t, c, hello(nil, 0))
	if f := read(t, c); f.Snapshot.You.Principal != "human:2918" || f.Snapshot.You.Role != "watcher" || !f.Snapshot.You.WebUI {
		t.Fatalf("you = %+v", f.Snapshot.You)
	}
	if _, code, err := dialAs(session("https://evil.example")); err == nil || code != http.StatusForbidden {
		t.Fatalf("a foreign origin: %d %v", code, err)
	}
	noAccess := session(origin)
	noAccess.Del(authn.ForwardedAccessHeader)
	if _, code, err := dialAs(noAccess); err == nil || code != http.StatusUnauthorized {
		t.Fatalf("no access token: %d %v", code, err)
	}
}
