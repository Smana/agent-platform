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
		}, AuthorizedParty: "roomctl-id"}
		if mut != nil {
			mut(c)
		}
		return sign(t, jwt.SigningMethodRS256, s.key, "", c)
	}
	cases := map[string]struct {
		raw, client string
		want        error // nil accepts; every refusal must also be ErrUnauthenticated
	}{
		"valid":              {tok(nil), "roomctl-id", nil},
		"any audience":       {tok(func(c *Claims) { c.Audience = jwt.ClaimStrings{"anything", issuer} }), "roomctl-id", nil},
		"another client":     {tok(nil), "rooms-proxy-id", ErrWrongAudience},
		"no azp":             {tok(func(c *Claims) { c.AuthorizedParty = "" }), "roomctl-id", ErrWrongAudience},
		"no subject":         {tok(func(c *Claims) { c.Subject = "" }), "roomctl-id", ErrUnauthenticated},
		"expired past skew":  {tok(func(c *Claims) { c.ExpiresAt = at(-time.Minute) }), "roomctl-id", ErrTokenExpired},
		"wrong issuer":       {tok(func(c *Claims) { c.Issuer = "https://evil.example" }), "roomctl-id", ErrWrongIssuer},
		"no client to check": {tok(nil), "", ErrUnauthenticated},
		"not a JWT":          {"not.a.jwt", "roomctl-id", ErrUnauthenticated},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := v.VerifyAuthorizedParty(ctx, c.raw, c.client)
			if c.want == nil {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if c.client != "" && got != nil && got.AuthorizedParty != c.client {
					t.Fatalf("accepted a token for %q", got.AuthorizedParty)
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
