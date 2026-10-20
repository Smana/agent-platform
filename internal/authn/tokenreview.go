// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RunVerifier checks a run's token for audience: a JWKS Verifier, or the cluster's TokenReview
// where no issuer URL is reachable (kind, adoption 3b).
type RunVerifier interface {
	Verify(ctx context.Context, raw, audience string) (*Claims, error)
}

// TokenReviewer is client-go's TokenReviewInterface, narrowed for tests.
type TokenReviewer interface {
	Create(ctx context.Context, tr *authv1.TokenReview, opts metav1.CreateOptions) (*authv1.TokenReview, error)
}

// tokenReviewTTL caps a cached answer: a revoked ServiceAccount stops passing within it.
const tokenReviewTTL = 5 * time.Minute

// sweepEvery is how many cached entries trigger a sweep of the expired ones.
const sweepEvery = 256

// TokenReview asks the apiserver about each token, and caches the answers it accepts.
type TokenReview struct {
	c     TokenReviewer
	now   func() time.Time
	mu    sync.Mutex
	cache map[[32]byte]cached
}

type cached struct {
	sub   string
	until time.Time
}

// NewTokenReview reviews tokens through c.
func NewTokenReview(c TokenReviewer, now func() time.Time) *TokenReview {
	return &TokenReview{c: c, now: now, cache: map[[32]byte]cached{}}
}

// Verify returns the token's subject when the apiserver authenticates it for audience.
// An unauthenticated answer is ErrWrongIssuer, so a JWKS issuer listed after it may still try.
// The claims' expiry is the cache entry's end, so a principal never outlives what was vouched for.
func (t *TokenReview) Verify(ctx context.Context, raw, audience string) (*Claims, error) {
	key := sha256.Sum256([]byte(audience + "\x00" + raw))
	now := t.now()
	t.mu.Lock()
	if c, ok := t.cache[key]; ok && now.Before(c.until) {
		t.mu.Unlock()
		return claimsFor(c), nil
	}
	t.mu.Unlock()
	out, err := t.c.Create(ctx, &authv1.TokenReview{Spec: authv1.TokenReviewSpec{Token: raw, Audiences: []string{audience}}}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: token review: %w", ErrUnauthenticated, err)
	}
	if !out.Status.Authenticated {
		return nil, fmt.Errorf("%w: the cluster does not authenticate this token", ErrWrongIssuer)
	}
	if !slices.Contains(out.Status.Audiences, audience) {
		return nil, fmt.Errorf("%w: the token's audiences exclude %s", ErrUnauthenticated, audience)
	}
	until := now.Add(tokenReviewTTL)
	if exp, ok := unverifiedExp(raw); ok && exp.Before(until) {
		until = exp
	}
	c := cached{sub: out.Status.User.Username, until: until}
	t.mu.Lock()
	t.cache[key] = c
	if len(t.cache)%sweepEvery == 0 { // bounded with no goroutine
		for k, e := range t.cache {
			if !now.Before(e.until) {
				delete(t.cache, k)
			}
		}
	}
	t.mu.Unlock()
	return claimsFor(c), nil
}

func claimsFor(c cached) *Claims {
	return &Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: c.sub, ExpiresAt: jwt.NewNumericDate(c.until)}}
}

// unverifiedExp reads exp from a token the apiserver has just validated: it only shortens the cache.
func unverifiedExp(raw string) (time.Time, bool) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var p struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(b, &p) != nil || p.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(p.Exp, 0), true
}
