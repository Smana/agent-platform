// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fakeReviewer struct {
	calls  int
	status authv1.TokenReviewStatus
	err    error
}

func (f *fakeReviewer) Create(_ context.Context, tr *authv1.TokenReview, _ metav1.CreateOptions) (*authv1.TokenReview, error) {
	f.calls++
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

func TestTokenReviewCache(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	f := &fakeReviewer{status: ok("system:serviceaccount:agents:run-abcdefgh", AudienceRun)}
	v := NewTokenReview(f, clock)
	tok := jwtWithExp(now.Add(10 * time.Minute))
	for range 3 {
		if _, err := v.Verify(t.Context(), tok, AudienceRun); err != nil {
			t.Fatal(err)
		}
	}
	if f.calls != 1 {
		t.Fatalf("cached: %d apiserver calls, want 1", f.calls)
	}
	now = now.Add(5*time.Minute + time.Second) // past the 5-minute cap
	_, _ = v.Verify(t.Context(), tok, AudienceRun)
	if f.calls != 2 {
		t.Fatalf("after the cap: %d calls, want 2", f.calls)
	}
	short := jwtWithExp(now.Add(30 * time.Second))
	_, _ = v.Verify(t.Context(), short, AudienceRun)
	now = now.Add(31 * time.Second) // past the token's own exp
	_, _ = v.Verify(t.Context(), short, AudienceRun)
	if f.calls != 4 {
		t.Fatalf("exp bounds the cache: %d calls, want 4", f.calls)
	}
	f.status = authv1.TokenReviewStatus{Authenticated: false}
	bad := jwtWithExp(now.Add(time.Hour))
	_, _ = v.Verify(t.Context(), bad, AudienceRun)
	_, _ = v.Verify(t.Context(), bad, AudienceRun)
	if f.calls != 6 {
		t.Fatalf("refusals are not cached: %d calls, want 6", f.calls)
	}
}

// The principal's expiry is the cache entry's end, never later, so a cached hit
// cannot outlive what the apiserver vouched for.
func TestTokenReviewExpiryBoundedByCache(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := &fakeReviewer{status: ok("system:serviceaccount:agents:run-abcdefgh", AudienceRun)}
	v := NewTokenReview(f, func() time.Time { return now })
	tok := jwtWithExp(now.Add(time.Hour))
	for i := range 2 { // a miss, then a hit
		c, err := v.Verify(t.Context(), tok, AudienceRun)
		if err != nil {
			t.Fatal(err)
		}
		if want := now.Add(tokenReviewTTL); !c.ExpiresAt.Equal(want) {
			t.Fatalf("call %d: expiry %v, want %v", i, c.ExpiresAt.Time, want)
		}
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
