// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"crypto/rand"
	"crypto/rsa"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const issuer = "https://oidc.eks.eu-west-3.amazonaws.com/id/TEST"

type signer struct{ key *rsa.PrivateKey }

func newSigner(t *testing.T) signer {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return signer{k}
}

func (s signer) verifier(opts ...Option) *Verifier {
	return NewVerifierWithKeyfunc(issuer, func(*jwt.Token) (any, error) { return &s.key.PublicKey, nil }, opts...)
}

func (s signer) token(t *testing.T, iss, sub, aud string, ttl time.Duration) string {
	return sign(t, jwt.SigningMethodRS256, s.key, "", jwt.RegisteredClaims{
		Issuer: iss, Subject: sub, Audience: jwt.ClaimStrings{aud},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
	})
}

// sign mints a token with any method, key and kid; kid "" leaves the header out.
func sign(t *testing.T, m jwt.SigningMethod, key any, kid string, claims jwt.Claims) string {
	t.Helper()
	tok := jwt.NewWithClaims(m, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	raw, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// fakeClock is the injected clock; the mutex keeps -race quiet when a test
// verifies from several goroutines.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
