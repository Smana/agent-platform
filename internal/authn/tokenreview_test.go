// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fakeReviewer struct {
	mu     sync.Mutex // Create is called concurrently by the cache tests
	calls  int
	specs  []authv1.TokenReviewSpec // every request, in order
	status authv1.TokenReviewStatus
	err    error
}

func (f *fakeReviewer) Create(_ context.Context, tr *authv1.TokenReview, _ metav1.CreateOptions) (*authv1.TokenReview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.specs = append(f.specs, *tr.Spec.DeepCopy())
	if f.err != nil {
		return nil, f.err
	}
	out := tr.DeepCopy()
	out.Status = f.status
	return out, nil
}

// jwtWithExp is an unsigned JWT-shaped token whose payload carries exp: only the
// apiserver validates it; the verifier reads exp to bound its cache.
func jwtWithExp(exp time.Time) string {
	p := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + strconv.FormatInt(exp.Unix(), 10) + `}`))
	return "e30." + p + ".sig"
}

func ok(user string, aud ...string) authv1.TokenReviewStatus {
	return authv1.TokenReviewStatus{Authenticated: true, User: authv1.UserInfo{Username: user}, Audiences: aud}
}

func TestTokenReview(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tok := jwtWithExp(now.Add(10 * time.Minute))
	for name, tc := range map[string]struct {
		status  authv1.TokenReviewStatus
		err     error
		wantSub string
		wantErr error
		notErr  error // the kinds must stay apart: only ErrWrongIssuer lets the next issuer try
	}{
		"authenticated":   {status: ok("system:serviceaccount:agents:run-abcdefgh", AudienceRun), wantSub: "system:serviceaccount:agents:run-abcdefgh"},
		"wrong audience":  {status: ok("system:serviceaccount:agents:run-abcdefgh", "other"), wantErr: ErrWrongAudience, notErr: ErrWrongIssuer},
		"unauthenticated": {status: authv1.TokenReviewStatus{Authenticated: false}, wantErr: ErrWrongIssuer},
		"api error":       {err: errors.New("boom"), wantErr: ErrReviewUnavailable, notErr: ErrWrongIssuer},
	} {
		f := &fakeReviewer{status: tc.status, err: tc.err}
		c, err := NewTokenReview(f, func() time.Time { return now }).Verify(t.Context(), tok, AudienceRun)
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("%s: err %v, want %v", name, err, tc.wantErr)
			}
			if tc.notErr != nil && errors.Is(err, tc.notErr) {
				t.Errorf("%s: err %v must not be %v", name, err, tc.notErr)
			}
			if !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("%s: err %v must still be unauthenticated", name, err)
			}
			continue
		}
		if err != nil || c.Subject != tc.wantSub {
			t.Errorf("%s: claims %+v err %v", name, c, err)
		}
	}
}

// The audience is exact: the apiserver must vouch for the one audience this path serves, so a
// token minted for the other, for both or for none is refused. Neither the token nor the subject
// ever reaches the returned error.
func TestTokenReviewAudiences(t *testing.T) {
	const (
		sub = "system:serviceaccount:agents:run-7f3cq2xz"
		raw = "the-run-token"
	)
	refusals := map[string]struct{ status authv1.TokenReviewStatus }{
		"the system audience": {status: ok(sub, AudienceSystem)},
		"no audience":         {status: ok(sub)},
		"a second audience":   {status: ok(sub, AudienceRun, AudienceSystem)},
		"not authenticated":   {status: authv1.TokenReviewStatus{User: authv1.UserInfo{Username: sub}, Audiences: []string{AudienceRun}}},
	}
	for name, tc := range refusals {
		t.Run(name, func(t *testing.T) {
			f := &fakeReviewer{status: tc.status}
			now := time.Unix(1_800_000_000, 0)
			_, err := NewTokenReview(f, func() time.Time { return now }).Verify(t.Context(), raw, AudienceRun)
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err %v, want an unauthenticated refusal", err)
			}
			if tc.status.Authenticated && !errors.Is(err, ErrWrongAudience) {
				t.Fatalf("err %v, want ErrWrongAudience for an authenticated token with other audiences", err)
			}
			if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), sub) {
				t.Fatalf("the error echoes the credential: %v", err)
			}
		})
	}
	f := &fakeReviewer{status: ok(sub, AudienceRun)}
	now := time.Unix(1_800_000_000, 0)
	c, err := NewTokenReview(f, func() time.Time { return now }).Verify(t.Context(), raw, AudienceRun)
	if err != nil || c.Subject != sub {
		t.Fatalf("claims %+v, err %v", c, err)
	}
	if len(f.specs) != 1 || f.specs[0].Token != raw || !slices.Equal(f.specs[0].Audiences, []string{AudienceRun}) {
		t.Fatalf("the request carries exactly the token and the one audience: %+v", f.specs)
	}
	// A failed call is the broker's problem, not the credential's, and its text can echo the token.
	f = &fakeReviewer{err: fmt.Errorf("cannot create tokenreviews for %s (%s): forbidden", raw, sub)}
	_, err = NewTokenReview(f, func() time.Time { return now }).Verify(t.Context(), raw, AudienceRun)
	if !errors.Is(err, ErrReviewUnavailable) || strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), sub) {
		t.Fatalf("err %v, want an unavailable review that echoes neither the token nor the subject", err)
	}
}

// The claims carry the token's own expiry, the cache carries min(exp, 5m): an hour-long token is
// re-reviewed after five minutes, a two-minute one at its expiry, and the cache deadline never
// shortens what the principal claims (a bridge stream lives min(exp, 1 h)).
func TestTokenReviewPositiveCache(t *testing.T) {
	t0 := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	const sub = "system:serviceaccount:agents:run-7f3cq2xz"

	t.Run("the token's own expiry bounds the cache", func(t *testing.T) {
		now := t0
		f := &fakeReviewer{status: ok(sub, AudienceRun)}
		v := NewTokenReview(f, func() time.Time { return now })
		raw := jwtWithExp(t0.Add(2 * time.Minute))
		for _, at := range []struct {
			now   time.Time
			calls int
		}{{t0, 1}, {t0.Add(time.Minute + 59*time.Second), 1}, {t0.Add(2 * time.Minute), 2}} {
			now = at.now
			c, err := v.Verify(t.Context(), raw, AudienceRun)
			if at.calls == 2 { // the entry ends at the token's exp, which is now past
				if !errors.Is(err, ErrTokenExpired) {
					t.Fatalf("at %s: err %v, want the token's expiry to refuse it", now.Format(time.TimeOnly), err)
				}
			} else if err != nil || c.Subject != sub || c.ExpiresAt == nil || !c.ExpiresAt.Equal(t0.Add(2*time.Minute)) {
				t.Fatalf("at %s: claims %+v, err %v", now.Format(time.TimeOnly), c, err)
			}
			if f.calls != at.calls {
				t.Fatalf("at %s: %d apiserver calls, want %d", now.Format(time.TimeOnly), f.calls, at.calls)
			}
		}
	})

	t.Run("five minutes cap the cache, not the claims", func(t *testing.T) {
		now := t0
		f := &fakeReviewer{status: ok(sub, AudienceRun)}
		v := NewTokenReview(f, func() time.Time { return now })
		raw := jwtWithExp(t0.Add(time.Hour))
		for _, at := range []struct {
			now   time.Time
			calls int
		}{{t0, 1}, {t0.Add(4*time.Minute + 59*time.Second), 1}, {t0.Add(5 * time.Minute), 2}} {
			now = at.now
			c, err := v.Verify(t.Context(), raw, AudienceRun)
			if err != nil || c.Subject != sub || c.ExpiresAt == nil || !c.ExpiresAt.Equal(t0.Add(time.Hour)) {
				t.Fatalf("at %s: claims %+v, err %v", now.Format(time.TimeOnly), c, err)
			}
			if f.calls != at.calls {
				t.Fatalf("at %s: %d apiserver calls, want %d", now.Format(time.TimeOnly), f.calls, at.calls)
			}
		}
	})

	t.Run("an undecodable exp claims none and caches five minutes", func(t *testing.T) {
		now := t0
		f := &fakeReviewer{status: ok(sub, AudienceRun)}
		v := NewTokenReview(f, func() time.Time { return now })
		for _, at := range []struct {
			now   time.Time
			calls int
		}{{t0, 1}, {t0.Add(4*time.Minute + 59*time.Second), 1}, {t0.Add(5 * time.Minute), 2}} {
			now = at.now
			c, err := v.Verify(t.Context(), "not-a-jwt", AudienceRun)
			if err != nil || c.Subject != sub || c.ExpiresAt == nil || !c.ExpiresAt.IsZero() {
				t.Fatalf("at %s: claims %+v, err %v", now.Format(time.TimeOnly), c, err)
			}
			if f.calls != at.calls {
				t.Fatalf("at %s: %d apiserver calls, want %d", now.Format(time.TimeOnly), f.calls, at.calls)
			}
		}
	})
}

// The apiserver authenticates the token, but its own exp is past: refused, and nothing cached. The
// unverified claim can only ever refuse, never accept.
func TestTokenReviewRawExpiryPast(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	f := &fakeReviewer{status: ok("system:serviceaccount:agents:run-7f3cq2xz", AudienceRun)}
	v := NewTokenReview(f, func() time.Time { return now })
	raw := jwtWithExp(now.Add(-time.Second))
	for i := range 2 {
		if _, err := v.Verify(t.Context(), raw, AudienceRun); !errors.Is(err, ErrTokenExpired) || !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("call %d: err %v, want a refusal on the token's own expiry", i, err)
		}
	}
	if f.calls != 2 {
		t.Fatalf("%d apiserver calls, want 2: a past exp is never cached", f.calls)
	}
	if n := len(v.cache); n != 0 {
		t.Fatalf("cache holds %d entries for a refused token", n)
	}
}

// Only an authenticated, exact-audience, unexpired answer is cached, and under the audience it was
// reviewed for: a run's success must not answer a system caller.
func TestTokenReviewRefusalsNotCached(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	const sub = "system:serviceaccount:agents:run-7f3cq2xz"
	f := &fakeReviewer{status: authv1.TokenReviewStatus{Authenticated: false}}
	v := NewTokenReview(f, func() time.Time { return now })
	raw := jwtWithExp(now.Add(time.Hour))
	if _, err := v.Verify(t.Context(), raw, AudienceRun); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("unauthenticated: %v", err)
	}
	f.status = ok(sub, AudienceRun, AudienceSystem)
	if _, err := v.Verify(t.Context(), raw, AudienceRun); !errors.Is(err, ErrWrongAudience) {
		t.Fatalf("two audiences: %v", err)
	}
	f.status = ok(sub, AudienceRun)
	c, err := v.Verify(t.Context(), raw, AudienceRun)
	if err != nil || c.Subject != sub {
		t.Fatalf("authenticated: claims %+v, err %v", c, err)
	}
	if f.calls != 3 {
		t.Fatalf("%d apiserver calls, want 3: refusals are not cached", f.calls)
	}
	// The cached run success answers AudienceRun only.
	if _, err := v.Verify(t.Context(), raw, AudienceSystem); !errors.Is(err, ErrWrongAudience) {
		t.Fatalf("a run's cache entry for the system audience: %v", err)
	}
	if f.calls != 4 || !slices.Equal(f.specs[3].Audiences, []string{AudienceSystem}) {
		t.Fatalf("%d calls, last audiences %v: the system audience is reviewed on its own", f.calls, f.specs[3].Audiences)
	}
}

// The cache is shared by every request: concurrent verifies must not race (-race runs this), and
// each must see a principal for the one token.
func TestTokenReviewConcurrent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	const sub = "system:serviceaccount:agents:run-7f3cq2xz"
	f := &fakeReviewer{status: ok(sub, AudienceRun)}
	v := NewTokenReview(f, func() time.Time { return now })
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			c, err := v.Verify(t.Context(), jwtWithExp(now.Add(time.Hour)), AudienceRun)
			if err != nil || c.Subject != sub {
				t.Errorf("claims %+v, err %v", c, err)
			}
		})
	}
	wg.Wait()
	if f.calls < 1 {
		t.Fatalf("%d apiserver calls, want at least 1", f.calls)
	}
}

func TestTokenReviewSweepsExpired(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := &fakeReviewer{status: ok("system:serviceaccount:agents:run-abcdefgh", AudienceRun)}
	v := NewTokenReview(f, func() time.Time { return now })
	for i := range 255 {
		if _, err := v.Verify(t.Context(), jwtWithExp(now.Add(time.Duration(i+1)*time.Second)), AudienceRun); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Hour) // all expired
	if _, err := v.Verify(t.Context(), jwtWithExp(now.Add(time.Hour)), AudienceRun); err != nil {
		t.Fatal(err)
	}
	if n := len(v.cache); n > 1 {
		t.Fatalf("cache holds %d entries after the sweep, want 1", n)
	}
}
