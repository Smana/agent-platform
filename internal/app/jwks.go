// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Smana/maku/internal/authn"
	"github.com/Smana/maku/internal/config"
	"github.com/Smana/maku/internal/humanapi"
	"github.com/Smana/maku/internal/metrics"
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

// newVerifier builds one issuer's verifier. An eager one fetches the JWKS now; a
// lazy one on its first token or refresh.
type newVerifier func(ctx context.Context, issuer, jwksURL string, lazy bool) (*authn.Verifier, error)

// jwksVerifier is the broker's newVerifier: a JWKS fetched through httpx, its
// refreshes logged.
func jwksVerifier(log *slog.Logger) newVerifier {
	return func(ctx context.Context, issuer, jwksURL string, lazy bool) (*authn.Verifier, error) {
		if lazy {
			return authn.NewLazyVerifier(issuer, jwksURL, authn.WithLogger(log))
		}
		return authn.NewVerifier(ctx, issuer, jwksURL, authn.WithLogger(log))
	}
}

// auth is every caller kind's authenticator, and the distinct verifiers behind
// them for refreshJWKS.
type auth struct {
	runs      *authn.Runs
	systems   *authn.Systems
	humans    humanapi.Authenticator // *authn.Humans, or *authn.Local in local mode
	sponsored *authn.Sponsored       // nil while human.mcp.sponsored is empty
	verifiers []*authn.Verifier
}

// authenticators builds the run, system and human verifiers. The machine ones
// fetch their JWKS now, so an unreachable cluster issuer fails the rollout. The
// human one is lazy: the IdP being down must not fail the rollout, nor :8443
// with it (FORWARD 2.6); until it answers, humans are refused. One verifier
// serves each (issuer, JWKS URL): the cluster's issuer usually signs both kinds
// of machine token, and one cache then refreshes once. reviews supplies the
// apiserver client a tokenreview issuer asks for, and is called at most once,
// only when one is configured. Every distinct JWKS verifier, the human one
// included, is refreshed and gauged (Ruling AQ); a TokenReview verifier is not
// one, and adds no series of its own.
func authenticators(ctx context.Context, cfg config.Config, m *metrics.Set, build newVerifier, reviews tokenReviewClient) (auth, error) {
	byKey := map[[2]string]*authn.Verifier{}
	var all []*authn.Verifier
	verifier := func(issuer, jwksURL string, lazy bool) (*authn.Verifier, error) {
		if v, ok := byKey[[2]string{issuer, jwksURL}]; ok {
			return v, nil
		}
		v, err := build(ctx, issuer, jwksURL, lazy)
		if err != nil {
			return nil, err
		}
		byKey[[2]string{issuer, jwksURL}] = v
		all = append(all, v)
		return v, nil
	}
	// The apiserver client is built on the first tokenreview issuer, so a broker that has none
	// never asks for a kubeconfig.
	var reviewer authn.TokenReviewer
	tokenReview := func() (authn.TokenReviewer, error) {
		if reviewer != nil {
			return reviewer, nil
		}
		if reviews == nil {
			return nil, errors.New("a tokenreview issuer needs the in-cluster client")
		}
		var err error
		if reviewer, err = reviews(); err != nil {
			return nil, err
		}
		return reviewer, nil
	}
	// machine returns the verifier behind one issuer: its JWKS, or the apiserver's TokenReview.
	machine := func(is config.IssuerConfig) (authn.RunVerifier, error) {
		if is.EffectiveMode() == config.IssuerModeTokenReview {
			tr, err := tokenReview()
			if err != nil {
				return nil, err
			}
			return authn.NewTokenReview(tr, time.Now), nil
		}
		return verifier(is.Issuer, is.JWKSURL, false)
	}
	var issuers []authn.RunIssuer
	for _, is := range cfg.RunIssuers {
		v, err := machine(is)
		if err != nil {
			return auth{}, err
		}
		// config.Load compiled it with one capture group already.
		issuers = append(issuers, authn.RunIssuer{Verifier: v, SubPattern: regexp.MustCompile(is.SubPattern)})
	}
	runs, err := authn.NewRuns(issuers...)
	if err != nil {
		return auth{}, err
	}
	// A nil RunVerifier is the intentional never-authenticating path: an absent system issuer
	// keeps the system routes at 401, and never stores a typed-nil *authn.Verifier in an interface.
	var sys authn.RunVerifier
	if is := cfg.SystemIssuer; is != nil {
		if sys, err = machine(*is); err != nil {
			return auth{}, err
		}
	}
	h := cfg.Human
	var hv *authn.Verifier
	if h.EffectiveMode() != config.HumanModeLocal { // local mode has no IdP: nothing to verify, nothing to refresh
		var err error
		if hv, err = verifier(h.Issuer, h.JWKSURL, true); err != nil {
			return auth{}, err
		}
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
		return auth{}, err
	}
	var humans humanapi.Authenticator
	if h.EffectiveMode() == config.HumanModeLocal {
		groups := h.EffectiveGroups()
		humans = authn.NewLocal(h.Origin, []string{groups.Admin, groups.Member})
	} else {
		humans = authn.NewHumans(hv, idFile(h.ProjectIDFile), idFile(h.ClientIDFile), idFile(h.RoomctlClientIDFile), h.Origin)
	}
	var sponsored *authn.Sponsored
	if len(h.MCP.Sponsored) > 0 {
		clients := make([]authn.SponsoredClient, 0, len(h.MCP.Sponsored))
		for _, sp := range h.MCP.Sponsored {
			clients = append(clients, authn.SponsoredClient{Name: sp.Name, ClientID: idFile(sp.ClientIDFile)})
		}
		web, roomctl := idFile(h.ClientIDFile), idFile(h.RoomctlClientIDFile)
		sponsored = &authn.Sponsored{V: hv, Clients: clients, Exclude: func() []string { return []string{web(), roomctl()} },
			Audience: idFile(h.ProjectIDFile)}
	}
	return auth{runs: runs, systems: authn.NewSystems(sys, cfg.SystemPrincipals), humans: humans, verifiers: all,
		sponsored: sponsored}, nil
}

// idFile reads an id from a mounted file on every call, so an id the IdP mints
// anew needs no restart (Ruling AS-a). An unset or unreadable file reads as "",
// which refuses every token that needs it.
func idFile(path string) func() string {
	return func() string {
		if path == "" {
			return ""
		}
		raw, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(raw))
	}
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

// refreshJWKS refreshes every verifier at start, which is a lazy one's first
// fetch (an eager one just fetched, so its refresh is a no-op), then each
// jwksRefreshEvery plus up to jwksJitter, on every replica (each verifies
// tokens), until ctx ends. A failed refresh keeps the held keys up to the stale
// cap and leaves LastRefresh, so its gauge ages; the cache logs the failure.
// after is the timer; nil means time.After.
func refreshJWKS(ctx context.Context, verifiers []refresher, after func(time.Duration) <-chan time.Time) error {
	if after == nil {
		after = time.After
	}
	for {
		for _, v := range verifiers {
			rctx, cancel := context.WithTimeout(ctx, jwksRefreshTimeout)
			_ = v.Refresh(rctx) // the cache logs a failure, with its URL
			cancel()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-after(jwksRefreshEvery + jitter(jwksJitter)):
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
