// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Smana/maku/internal/authn"
	"github.com/Smana/maku/internal/config"
	"github.com/Smana/maku/internal/metrics"
)

// factorySub is the factory's ServiceAccount, allowlisted to system:factory.
const factorySub = "system:serviceaccount:agent-system:agent-factory"

// reviews is a fake apiserver: it answers with status, and records every request.
type reviews struct {
	calls  int
	specs  []authv1.TokenReviewSpec
	status authv1.TokenReviewStatus
}

func (r *reviews) Create(_ context.Context, tr *authv1.TokenReview, _ metav1.CreateOptions) (*authv1.TokenReview, error) {
	r.calls++
	r.specs = append(r.specs, *tr.Spec.DeepCopy())
	out := &authv1.TokenReview{Status: r.status}
	return out, nil
}

func authenticated(sub string, audience string) authv1.TokenReviewStatus {
	return authv1.TokenReviewStatus{Authenticated: true, User: authv1.UserInfo{Username: sub}, Audiences: []string{audience}}
}

// localHuman is the local-mode human block: no IdP, so no human verifier to build.
func localHuman() config.HumanConfig {
	return config.HumanConfig{Mode: config.HumanModeLocal, Origin: "http://localhost:8080",
		Groups: config.GroupsConfig{Admin: "admin", Member: "member"}}
}

func bearer(token string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/rooms", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

// TestAuthenticatorsTokenReview: a TokenReview-only broker builds no JWKS verifier at all, and its
// system issuer, when configured, is a TokenReview one too — each asking for its own audience.
func TestAuthenticatorsTokenReview(t *testing.T) {
	m, err := metrics.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		RunIssuers: []config.IssuerConfig{{Mode: config.IssuerModeTokenReview,
			SubPattern: `^system:serviceaccount:agents:run-([a-z2-7]{8})$`}},
		Human: localHuman(),
	}
	api := &reviews{}
	var builds, provided int
	build := func(context.Context, string, string, bool) (*authn.Verifier, error) {
		builds++
		return nil, errors.New("a TokenReview-only broker builds no JWKS verifier")
	}
	provide := func() (authn.TokenReviewer, error) { provided++; return api, nil }
	a, err := authenticators(t.Context(), cfg, m, build, provide)
	switch {
	case err != nil:
		t.Fatal(err)
	case a.runs == nil || a.systems == nil || a.humans == nil:
		t.Fatal("every caller kind needs a non-nil authenticator")
	case len(a.verifiers) != 0:
		t.Fatalf("%d JWKS verifiers, want none", len(a.verifiers))
	case builds != 0:
		t.Fatalf("the JWKS builder ran %d times", builds)
	case provided != 1:
		t.Fatalf("the client provider ran %d times, want once", provided)
	}
	api.status = authenticated("system:serviceaccount:agents:run-7f3cq2xz", authn.AudienceRun)
	p, err := a.runs.Authenticate(bearer("run-token"))
	if err != nil || p.RunID != "7f3cq2xz" || p.ID != "agent:7f3cq2xz" {
		t.Fatalf("run principal %+v, %v", p, err)
	}
	if !slices.Equal(api.specs[0].Audiences, []string{authn.AudienceRun}) {
		t.Fatalf("the run path asks for %v", api.specs[0].Audiences)
	}
	// No systemIssuer: the system routes still have an authenticator, and it never authenticates.
	if _, err := a.systems.Authenticate(bearer("sys-token")); !errors.Is(err, authn.ErrUnauthenticated) {
		t.Fatalf("a broker without a system issuer: %v", err)
	}

	// The same mode serves the system issuer, with its own audience and allowlist.
	sys := cfg
	sys.SystemIssuer = &config.IssuerConfig{Mode: config.IssuerModeTokenReview}
	sys.SystemPrincipals = map[string]string{factorySub: "system:factory"}
	api.status = authenticated(factorySub, authn.AudienceSystem)
	a, err = authenticators(t.Context(), sys, m, build, provide)
	if err != nil {
		t.Fatal(err)
	}
	p, err = a.systems.Authenticate(bearer("factory-token"))
	if err != nil || p.ID != "system:factory" || p.Kind != "system" {
		t.Fatalf("system principal %+v, %v", p, err)
	}
	last := api.specs[len(api.specs)-1]
	if !slices.Equal(last.Audiences, []string{authn.AudienceSystem}) {
		t.Fatalf("the system path asks for %v", last.Audiences)
	}
}

// A broker may list both kinds: each authenticates its own run token, and only the JWKS one is a
// JWKS verifier — the refreshed, gauged set names it alone.
func TestAuthenticatorsMixedRunIssuers(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := jwksServer(t, key, "k1")
	defer srv.Close()
	m, err := metrics.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		RunIssuers: []config.IssuerConfig{
			{Issuer: clusterIssuer, JWKSURL: srv.URL + "/keys",
				SubPattern: `^system:serviceaccount:agents:run-([a-z2-7]{8})$`},
			{Mode: config.IssuerModeTokenReview, SubPattern: `^system:serviceaccount:agents:xplane-run-([a-z2-7]{8})$`},
		},
		Human: localHuman(),
	}
	build := func(ctx context.Context, issuer, jwksURL string, _ bool) (*authn.Verifier, error) {
		return authn.NewVerifier(ctx, issuer, jwksURL, authn.WithHTTPClient(srv.Client()))
	}
	api := &reviews{}
	a, err := authenticators(t.Context(), cfg, m, build, func() (authn.TokenReviewer, error) { return api, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(a.verifiers) != 1 {
		t.Fatalf("%d JWKS verifiers, want only the JWKS run issuer's", len(a.verifiers))
	}
	sign := func(sub string) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, authn.Claims{
			RegisteredClaims: jwt.RegisteredClaims{Issuer: clusterIssuer, Subject: sub, Audience: jwt.ClaimStrings{authn.AudienceRun},
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
		})
		tok.Header["kid"] = "k1"
		raw, err := tok.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	const jwksSub, reviewSub = "system:serviceaccount:agents:run-aaaaaaaa", "system:serviceaccount:agents:xplane-run-bbbbbbbb"
	if p, err := a.runs.Authenticate(bearer(sign(jwksSub))); err != nil || p.RunID != "aaaaaaaa" {
		t.Fatalf("the JWKS issuer's run: %+v, %v", p, err)
	}
	// A token shaped like a JWT from an issuer the JWKS verifier does not trust: it declines with
	// ErrWrongIssuer, and the TokenReview issuer answers for it.
	foreign := "eyJhbGciOiJSUzI1NiIsImtpZCI6ImsxIn0." + base64.RawURLEncoding.EncodeToString([]byte(
		fmt.Sprintf(`{"iss":"https://kind.local","sub":%q,"exp":%d}`, reviewSub, time.Now().Add(time.Hour).Unix()))) + ".c2ln"
	api.status = authenticated(reviewSub, authn.AudienceRun)
	if p, err := a.runs.Authenticate(bearer(foreign)); err != nil || p.RunID != "bbbbbbbb" {
		t.Fatalf("the TokenReview issuer's run: %+v, %v", p, err)
	}
}

// A JWKS-only broker never asks for the apiserver client: no kubeconfig is needed to serve it.
func TestAuthenticatorsBuildsTokenReviewClientOnlyWhenNeeded(t *testing.T) {
	m, err := metrics.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		RunIssuers: []config.IssuerConfig{{Issuer: clusterIssuer, JWKSURL: clusterIssuer + "/keys", SubPattern: `^run-(\w+)$`}},
		Human:      localHuman(),
	}
	build := func(_ context.Context, issuer, _ string, _ bool) (*authn.Verifier, error) {
		return authn.NewVerifierWithKeyfunc(issuer, func(*jwt.Token) (any, error) { return nil, errors.New("unused") }), nil
	}
	var provided int
	if _, err := authenticators(t.Context(), cfg, m, build, func() (authn.TokenReviewer, error) {
		provided++
		return &reviews{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if provided != 0 {
		t.Fatalf("the client provider ran %d times for a JWKS-only broker", provided)
	}
}

// A TokenReview verifier is not a JWKS verifier: it adds no key gauge, so the quickstart's scrape
// carries no placeholder series.
func TestTokenReviewOnlyHasNoJWKSMetrics(t *testing.T) {
	exp, err := metrics.NewExporter(metrics.BrokerBuildInfo, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = exp.Shutdown(context.WithoutCancel(t.Context())) }()
	m, err := metrics.New(exp.Meter())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		RunIssuers: []config.IssuerConfig{{Mode: config.IssuerModeTokenReview,
			SubPattern: `^system:serviceaccount:agents:run-([a-z2-7]{8})$`}},
		Human: localHuman(),
	}
	build := func(context.Context, string, string, bool) (*authn.Verifier, error) {
		t.Fatal("a TokenReview-only config builds no JWKS verifier")
		return nil, nil
	}
	if _, err := authenticators(t.Context(), cfg, m, build, func() (authn.TokenReviewer, error) { return &reviews{}, nil }); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	exp.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	if strings.Contains(string(body), "maku_rooms_authn_jwks_last_refresh_timestamp_seconds") {
		t.Fatalf("a TokenReview-only broker exposes a JWKS key gauge:\n%s", body)
	}
}

// jwksServer serves one RSA key's JWKS over TLS, so the verifier's own fetch sets LastRefresh.
func jwksServer(t *testing.T, key *rsa.PrivateKey, kid string) *httptest.Server {
	t.Helper()
	body, err := json.Marshal(map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/keys" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	return srv
}

// The mixed issuer set's metric registration names the JWKS issuer alone.
func TestMixedIssuersNameOnlyTheJWKSIssuer(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := jwksServer(t, key, "k1")
	defer srv.Close()
	exp, err := metrics.NewExporter(metrics.BrokerBuildInfo, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = exp.Shutdown(context.WithoutCancel(t.Context())) }()
	m, err := metrics.New(exp.Meter())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		RunIssuers: []config.IssuerConfig{
			{Issuer: clusterIssuer, JWKSURL: srv.URL + "/keys", SubPattern: `^system:serviceaccount:agents:run-([a-z2-7]{8})$`},
			{Mode: config.IssuerModeTokenReview, SubPattern: `^system:serviceaccount:agents:xplane-run-([a-z2-7]{8})$`},
		},
		Human: localHuman(),
	}
	build := func(ctx context.Context, issuer, jwksURL string, _ bool) (*authn.Verifier, error) {
		return authn.NewVerifier(ctx, issuer, jwksURL, authn.WithHTTPClient(srv.Client()))
	}
	if _, err := authenticators(t.Context(), cfg, m, build, func() (authn.TokenReviewer, error) { return &reviews{}, nil }); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	exp.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	want := fmt.Sprintf(`maku_rooms_authn_jwks_last_refresh_timestamp_seconds{issuer=%q}`, clusterIssuer)
	if !strings.Contains(string(body), want) {
		t.Fatalf("the JWKS issuer's gauge is missing:\n%s", body)
	}
	if n := strings.Count(string(body), "maku_rooms_authn_jwks_last_refresh_timestamp_seconds{"); n != 1 {
		t.Fatalf("%d key-gauge series, want only the JWKS issuer's:\n%s", n, body)
	}
}
