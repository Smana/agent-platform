// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestVerifyAuthorizedParty(t *testing.T) {
	s := newSigner(t)
	clock := newClock()
	v := s.verifier(WithClock(clock.now))
	ctx := context.Background()
	at := func(d time.Duration) *jwt.NumericDate { return jwt.NewNumericDate(clock.now().Add(d)) }
	tok := func(mut func(*Claims)) string {
		c := &Claims{RegisteredClaims: jwt.RegisteredClaims{
			Issuer: issuer, Subject: runSub, IssuedAt: at(-time.Minute), ExpiresAt: at(10 * time.Minute),
		}, AuthorizedParty: "maku-id"}
		if mut != nil {
			mut(c)
		}
		return sign(t, jwt.SigningMethodRS256, s.key, "", c)
	}
	cases := map[string]struct {
		raw, client string
		want        error // nil accepts; every refusal must also be ErrUnauthenticated
	}{
		"valid":                {tok(nil), "maku-id", nil},
		"any audience":         {tok(func(c *Claims) { c.Audience = jwt.ClaimStrings{"anything", issuer} }), "maku-id", nil},
		"another client":       {tok(nil), "rooms-proxy-id", ErrWrongAudience},
		"no azp, no client_id": {tok(func(c *Claims) { c.AuthorizedParty = "" }), "maku-id", ErrWrongAudience},
		// A ZITADEL JWT access token names its client in client_id and has no azp (review C1).
		"client_id, no azp": {tok(func(c *Claims) { c.AuthorizedParty, c.ClientID = "", "maku-id" }), "maku-id", nil},
		"another app's client_id": {tok(func(c *Claims) { c.AuthorizedParty, c.ClientID = "", "grafana" }), "maku-id",
			ErrWrongAudience},
		"azp and client_id agree": {tok(func(c *Claims) { c.ClientID = "maku-id" }), "maku-id", nil},
		// A token naming two clients names none: neither claim may outvote the other.
		"azp ours, client_id another": {tok(func(c *Claims) { c.ClientID = "grafana" }), "maku-id", ErrWrongAudience},
		"client_id ours, azp another": {tok(func(c *Claims) { c.AuthorizedParty, c.ClientID = "grafana", "maku-id" }), "maku-id",
			ErrWrongAudience},
		"no subject":         {tok(func(c *Claims) { c.Subject = "" }), "maku-id", ErrUnauthenticated},
		"expired past skew":  {tok(func(c *Claims) { c.ExpiresAt = at(-time.Minute) }), "maku-id", ErrTokenExpired},
		"wrong issuer":       {tok(func(c *Claims) { c.Issuer = "https://evil.example" }), "maku-id", ErrWrongIssuer},
		"no client to check": {tok(nil), "", ErrUnauthenticated},
		"not a JWT":          {"not.a.jwt", "maku-id", ErrUnauthenticated},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := v.VerifyAuthorizedParty(ctx, c.raw, c.client)
			if c.want == nil {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if got == nil || (got.AuthorizedParty != c.client && got.ClientID != c.client) {
					t.Fatalf("accepted a token for azp %q, client_id %q", got.AuthorizedParty, got.ClientID)
				}
				return
			}
			if !errors.Is(err, c.want) || !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			// The refusal never echoes the subject (package doc).
			if strings.Contains(err.Error(), runSub) {
				t.Errorf("the subject reached the error: %v", err)
			}
		})
	}
}
