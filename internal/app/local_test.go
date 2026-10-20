// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/config"
	"github.com/Smana/agent-platform/internal/metrics"
)

func TestHumanListenAddr(t *testing.T) {
	if got := humanListenAddr(config.Config{}); got != ":8080" {
		t.Fatalf("oidc: %q", got)
	}
	if got := humanListenAddr(config.Config{Human: config.HumanConfig{Mode: "local"}}); got != "127.0.0.1:8080" {
		t.Fatalf("local: %q", got)
	}
}

// Local mode builds no human verifier (there is no IdP to fetch) and authenticates as human:local.
func TestAuthenticatorsLocal(t *testing.T) {
	cfg := config.Config{
		RunIssuers: []config.IssuerConfig{{Issuer: clusterIssuer, JWKSURL: clusterIssuer + "/keys", SubPattern: `^run-(\w+)$`}},
		Human: config.HumanConfig{Mode: "local", Origin: "http://localhost:8080",
			Groups: config.GroupsConfig{Admin: "admin", Member: "member"}},
	}
	var built int
	build := func(context.Context, string, string, bool) (*authn.Verifier, error) {
		built++
		return authn.NewVerifierWithKeyfunc(clusterIssuer, nil), nil
	}
	m, err := metrics.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := authenticators(t.Context(), cfg, m, build, nil)
	if err != nil {
		t.Fatal(err)
	}
	if built != 1 || len(a.verifiers) != 1 {
		t.Fatalf("only the run issuer's verifier: built %d, held %d", built, len(a.verifiers))
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/rooms", nil)
	r.Host = "localhost:8080"
	p, err := a.humans.Authenticate(r)
	if err != nil || p.ID != authn.LocalPrincipal {
		t.Fatalf("%+v %v", p, err)
	}
	r.Header.Set("Origin", "https://evil.example")
	if _, err := a.humans.Authenticate(r); !errors.Is(err, authn.ErrForbidden) {
		t.Fatalf("foreign origin: %v", err)
	}
}
