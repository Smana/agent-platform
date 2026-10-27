// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// ErrReviewUnavailable is a TokenReview call that failed (RBAC, a missing auth-delegator binding, an
// apiserver blip): no verdict on the token. It wraps ErrUnauthenticated, so the caller refuses; the
// broker logs it apart from a bad credential.
var ErrReviewUnavailable = fmt.Errorf("%w: token review unavailable", ErrUnauthenticated)

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
	sub string
	// until is when the review is repeated, min(the token's exp, now+tokenReviewTTL). It bounds the
	// cache only: never a claim, never a stream's life.
	until time.Time
	// exp is the token's own exp, zero when it decodes to none: the claims' ExpiresAt.
	exp time.Time
}

// NewTokenReview reviews tokens through c.
func NewTokenReview(c TokenReviewer, now func() time.Time) *TokenReview {
	return &TokenReview{c: c, now: now, cache: map[[32]byte]cached{}}
}

// Verify returns the token's subject when the apiserver authenticates it for exactly audience.
// An unauthenticated answer is ErrWrongIssuer, so a JWKS issuer listed after it may still try.
// A token whose own exp has passed is ErrTokenExpired: the unverified claim can refuse, never accept.
// The claims carry the token's own exp, or a zero one a stream reads as no deadline; the cache's
// shorter lifetime is the verifier's business alone.
func (t *TokenReview) Verify(ctx context.Context, raw, audience string) (*Claims, error) {
	key := sha256.Sum256([]byte(audience + "\x00" + raw))
	now := t.now()
	t.mu.Lock()
	c, ok := t.cache[key]
	if ok && now.Before(c.until) {
		t.mu.Unlock()
		return claimsFor(c), nil
	}
	if ok {
		delete(t.cache, key) // at or past its end: reviewed again
	}
	t.mu.Unlock()
	out, err := t.c.Create(ctx, &authv1.TokenReview{Spec: authv1.TokenReviewSpec{Token: raw, Audiences: []string{audience}}}, metav1.CreateOptions{})
	if err != nil {
		// The apiserver's own text can echo the token or the subject: it never enters the returned
		// error. The caller's 401 and the broker's warning name the cause class; the apiserver's
		// audit log has the rest.
		return nil, fmt.Errorf("%w: the apiserver answered with an error", ErrReviewUnavailable)
	}
	if !out.Status.Authenticated {
		return nil, fmt.Errorf("%w: the cluster does not authenticate this token", ErrWrongIssuer)
	}
	// Exactly the one audience: a token minted for another path, or for both, is not this path's.
	if len(out.Status.Audiences) != 1 || out.Status.Audiences[0] != audience {
		return nil, fmt.Errorf("%w: the token's audiences are not exactly %s", ErrWrongAudience, audience)
	}
	exp, hasExp := unverifiedExp(raw)
	if hasExp && !exp.After(now) {
		return nil, fmt.Errorf("%w: the token's own expiry has passed", ErrTokenExpired)
	}
	until := now.Add(tokenReviewTTL)
	if hasExp && exp.Before(until) {
		until = exp
	}
	c = cached{sub: out.Status.User.Username, until: until, exp: exp}
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

// claimsFor is the principal the review vouches for: the subject, and the token's own expiry. A
// zero Time is no deadline, so a stream of a token without a decodable exp lives to its cap.
func claimsFor(c cached) *Claims {
	return &Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: c.sub, ExpiresAt: jwt.NewNumericDate(c.exp)}}
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
