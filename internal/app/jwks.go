// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"crypto/rand"
	"log/slog"
	"math/big"
	"regexp"
	"time"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/config"
	"github.com/Smana/agent-platform/internal/metrics"
)

const (
	// jwksRefreshEvery keeps held keys and the LastRefresh gauge fresh on an idle
	// broker (Ruling AQ): the cache otherwise refetches only when a token arrives.
	jwksRefreshEvery = time.Hour
	// jwksJitter spreads the replicas' refreshes over a tenth of the period.
	jwksJitter = jwksRefreshEvery / 10
	// jwksRefreshTimeout bounds one refresh, a wait for one in flight included.
	jwksRefreshTimeout = 30 * time.Second
)

// authenticators builds the run and system verifiers; each fetches its JWKS now,
// so an unreachable issuer fails the rollout. One verifier serves each (issuer,
// JWKS URL): the cluster's issuer usually signs both kinds of token, and one
// cache then refreshes once. It returns every distinct verifier, for refreshJWKS.
func authenticators(ctx context.Context, log *slog.Logger, cfg config.Config, m *metrics.Set) (*authn.Runs, *authn.Systems, []*authn.Verifier, error) {
	byKey := map[[2]string]*authn.Verifier{}
	var all []*authn.Verifier
	verifier := func(issuer, jwksURL string) (*authn.Verifier, error) {
		if v, ok := byKey[[2]string{issuer, jwksURL}]; ok {
			return v, nil
		}
		v, err := authn.NewVerifier(ctx, issuer, jwksURL, authn.WithLogger(log))
		if err != nil {
			return nil, err
		}
		byKey[[2]string{issuer, jwksURL}] = v
		all = append(all, v)
		return v, nil
	}
	var issuers []authn.RunIssuer
	for _, is := range cfg.RunIssuers {
		v, err := verifier(is.Issuer, is.JWKSURL)
		if err != nil {
			return nil, nil, nil, err
		}
		// config.Load compiled it with one capture group already.
		issuers = append(issuers, authn.RunIssuer{Verifier: v, SubPattern: regexp.MustCompile(is.SubPattern)})
	}
	runs, err := authn.NewRuns(issuers...)
	if err != nil {
		return nil, nil, nil, err
	}
	sys, err := verifier(cfg.SystemIssuer.Issuer, cfg.SystemIssuer.JWKSURL)
	if err != nil {
		return nil, nil, nil, err
	}
	// An issuer behind two JWKS URLs is one series: the fresher fetch.
	lastRefresh := map[string]func() time.Time{}
	for key, v := range byKey {
		if prev, ok := lastRefresh[key[0]]; ok {
			lastRefresh[key[0]] = func() time.Time { return later(prev(), v.LastRefresh()) }
		} else {
			lastRefresh[key[0]] = v.LastRefresh
		}
	}
	if err := m.WatchJWKS(lastRefresh); err != nil {
		return nil, nil, nil, err
	}
	return runs, authn.NewSystems(sys, cfg.SystemPrincipals), all, nil
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// refresher is the one Verifier method refreshJWKS calls.
type refresher interface {
	Refresh(ctx context.Context) error
}

// refreshJWKS refreshes every verifier each jwksRefreshEvery plus up to
// jwksJitter, on every replica (each verifies tokens), until ctx ends. A failed
// refresh keeps the held keys up to the stale cap and leaves LastRefresh, so its
// gauge ages; the cache logs the failure. after is the timer; nil means time.After.
func refreshJWKS(ctx context.Context, verifiers []refresher, after func(time.Duration) <-chan time.Time) error {
	if after == nil {
		after = time.After
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-after(jwksRefreshEvery + jitter(jwksJitter)):
		}
		for _, v := range verifiers {
			rctx, cancel := context.WithTimeout(ctx, jwksRefreshTimeout)
			_ = v.Refresh(rctx) // the cache logs a failure, with its URL
			cancel()
		}
	}
}

// jitter is a uniform duration in [0, upTo).
func jitter(upTo time.Duration) time.Duration {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(upTo)))
	if err != nil {
		return 0
	}
	return time.Duration(n.Int64())
}
