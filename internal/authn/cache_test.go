// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// longLived mints a token that outlives any clock the test advances, so it can
// be minted up front, off the goroutines that use it.
func longLived(t *testing.T, clock *fakeClock, m jwt.SigningMethod, key any, kid string) string {
	return sign(t, m, key, kid, jwt.RegisteredClaims{
		Issuer: issuer, Subject: runSub, Audience: jwt.ClaimStrings{AudienceRun},
		ExpiresAt: jwt.NewNumericDate(clock.now().Add(72 * time.Hour)),
	})
}

// The review's probe: a token with the cluster's iss and a random kid, whose
// client disconnects mid-fetch. The fetch it started must still complete and
// load a rotated key; the interval must not be spent on nothing.
func TestACancelledCallerDoesNotBurnTheRefresh(t *testing.T) {
	k, rotated, junk := newKeyring(t), newKeyring(t), newKeyring(t)
	s := newIssuerServer(t, rsaJWK("r1", &k.rsa.PublicKey))
	clock := newClock()
	v := newJWKSVerifier(t, s, clock)
	clock.advance(time.Minute)
	s.drain()

	t.Run("cancelled mid-fetch", func(t *testing.T) {
		s.publish(t, rsaJWK("r1", &k.rsa.PublicKey), rsaJWK("r2", &rotated.rsa.PublicKey))
		release := s.stall()
		ctx, cancel := context.WithCancel(t.Context())
		probe := longLived(t, clock, jwt.SigningMethodRS256, junk.rsa, "random-kid")
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = v.Verify(ctx, probe, AudienceRun)
		}()
		<-s.arrived
		cancel()
		release()
		<-done

		if _, err := v.Verify(t.Context(), longLived(t, clock, jwt.SigningMethodRS256, rotated.rsa, "r2"), AudienceRun); err != nil {
			t.Fatalf("the rotated key is refused after a cancelled probe: %v", err)
		}
	})

	t.Run("cancelled before the fetch", func(t *testing.T) {
		clock.advance(time.Minute)
		third := newKeyring(t)
		s.publish(t, rsaJWK("r1", &k.rsa.PublicKey), rsaJWK("r3", &third.rsa.PublicKey))
		before := s.hits.Load()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		probe := longLived(t, clock, jwt.SigningMethodRS256, junk.rsa, "random-kid")
		// get's select picks the fetch slot or ctx.Done at random; repeat so the
		// slot path, where the attempt could be stamped, is taken.
		for range 32 {
			if _, err := v.Verify(ctx, probe, AudienceRun); err == nil {
				t.Fatal("an unknown kid was accepted")
			}
		}
		if got := s.hits.Load(); got != before {
			t.Fatalf("a cancelled caller fetched: %d → %d", before, got)
		}
		if _, err := v.Verify(t.Context(), longLived(t, clock, jwt.SigningMethodRS256, third.rsa, "r3"), AudienceRun); err != nil {
			t.Fatalf("the interval was spent by a caller that never fetched: %v", err)
		}
	})
}

// A fresh cached kid is served from the snapshot while a fetch for an unknown
// kid is stalled at the issuer.
func TestAFreshKidNeverWaitsForAFetch(t *testing.T) {
	k, junk := newKeyring(t), newKeyring(t)
	s := newIssuerServer(t, rsaJWK("r1", &k.rsa.PublicKey))
	clock := newClock()
	v := newJWKSVerifier(t, s, clock)
	clock.advance(time.Minute)
	s.drain()

	fresh := longLived(t, clock, jwt.SigningMethodRS256, k.rsa, "r1")
	probe := longLived(t, clock, jwt.SigningMethodRS256, junk.rsa, "random-kid")
	release := s.stall()
	stalled := make(chan struct{})
	go func() {
		defer close(stalled)
		_, _ = v.Verify(t.Context(), probe, AudienceRun)
	}()
	<-s.arrived

	served := make(chan error, 1)
	go func() {
		_, err := v.Verify(t.Context(), fresh, AudienceRun)
		served <- err
	}()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("fresh kid: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("a fresh kid waited on the stalled fetch")
	}
	release()
	<-stalled
}

// Lookups, stale refreshes, unknown-kid refreshes and rotations all at once;
// -race is the assertion, plus a held key that must never be refused.
func TestRefreshAndLookupsRace(t *testing.T) {
	k, junk := newKeyring(t), newKeyring(t)
	s := newIssuerServer(t, rsaJWK("r1", &k.rsa.PublicKey))
	clock := newClock()
	v := newJWKSVerifier(t, s, clock)
	good := longLived(t, clock, jwt.SigningMethodRS256, k.rsa, "r1")
	unknown := longLived(t, clock, jwt.SigningMethodRS256, junk.rsa, "random-kid")
	body := func(keys ...map[string]string) []byte {
		b, err := json.Marshal(map[string]any{"keys": keys})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	withExtra := body(rsaJWK("r1", &k.rsa.PublicKey), rsaJWK("r2", &junk.rsa.PublicKey))
	without := body(rsaJWK("r1", &k.rsa.PublicKey))

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if _, err := v.Verify(t.Context(), good, AudienceRun); err != nil {
					t.Errorf("held key refused: %v", err)
					return
				}
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			for range 20 {
				_, _ = v.Verify(t.Context(), unknown, AudienceRun)
			}
		})
	}
	wg.Go(func() {
		for i := range 20 {
			clock.advance(5 * time.Minute)
			if i%2 == 0 {
				s.serve(http.StatusOK, withExtra)
			} else {
				s.serve(http.StatusOK, without)
			}
		}
	})
	wg.Wait()
}

func TestHeldKeysStopAtTheStaleCap(t *testing.T) {
	k := newKeyring(t)
	s := newIssuerServer(t, rsaJWK("r1", &k.rsa.PublicKey))
	clock := newClock()
	t0 := clock.now()
	var logs bytes.Buffer
	v, err := NewVerifier(t.Context(), issuer, s.srv.URL, WithHTTPClient(s.client()), WithClock(clock.now),
		WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	if !v.LastRefresh().Equal(t0) {
		t.Fatalf("LastRefresh = %v, want %v", v.LastRefresh(), t0)
	}
	token := longLived(t, clock, jwt.SigningMethodRS256, k.rsa, "r1")
	s.serve(http.StatusServiceUnavailable, nil)

	clock.advance(23 * time.Hour)
	if _, err := v.Verify(t.Context(), token, AudienceRun); err != nil {
		t.Fatalf("within the cap: %v", err)
	}
	if !strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Errorf("no Warn while serving held keys: %s", logs.String())
	}

	clock.advance(2 * time.Hour)
	if _, err := v.Verify(t.Context(), token, AudienceRun); !errors.Is(err, ErrKeysStale) || !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("past the cap: err = %v, want ErrKeysStale", err)
	}
	if !strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Errorf("no Error once past the cap: %s", logs.String())
	}
	if !v.LastRefresh().Equal(t0) {
		t.Fatalf("a failed fetch moved LastRefresh to %v", v.LastRefresh())
	}

	s.publish(t, rsaJWK("r1", &k.rsa.PublicKey))
	clock.advance(time.Minute)
	if _, err := v.Verify(t.Context(), token, AudienceRun); err != nil {
		t.Fatalf("after the issuer recovers: %v", err)
	}
	if !v.LastRefresh().Equal(clock.now()) {
		t.Fatalf("LastRefresh = %v, want %v", v.LastRefresh(), clock.now())
	}
	if strings.Contains(logs.String(), token) {
		t.Fatal("a token reached the log")
	}
}

// A kid pinned to RS256 refuses a token whose header says ES256 at key
// selection, before any signature is checked.
func TestAKeyVerifiesOnlyItsAlgorithm(t *testing.T) {
	k := newKeyring(t)
	s := newIssuerServer(t, rsaJWK("r1", &k.rsa.PublicKey))
	clock := newClock()
	v := newJWKSVerifier(t, s, clock)
	_, err := v.Verify(t.Context(), mint(t, clock, jwt.SigningMethodES256, k.ec, "r1"), AudienceRun)
	if !errors.Is(err, ErrUnauthenticated) || !errors.Is(err, jwt.ErrTokenUnverifiable) || errors.Is(err, jwt.ErrTokenSignatureInvalid) {
		t.Fatalf("want a refusal at key selection, got %v", err)
	}
}

func TestLastRefreshIsZeroOnAKeyfunc(t *testing.T) {
	if got := newSigner(t).verifier().LastRefresh(); !got.IsZero() {
		t.Fatalf("LastRefresh = %v", got)
	}
}
