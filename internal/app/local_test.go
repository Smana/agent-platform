// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/maku/internal/authn"
	"github.com/Smana/maku/internal/config"
	"github.com/Smana/maku/internal/fanout"
	"github.com/Smana/maku/internal/humanapi"
	"github.com/Smana/maku/internal/metrics"
	"github.com/Smana/maku/internal/policy"
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

// Local mode tells the listener what serves it: the fixed groups, the origin the every-route CSRF
// gate compares against, and the mode roomctl's discovery reports. OIDC keeps the configured groups
// and leaves the gate off.
func TestHumanServerModes(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	m, err := metrics.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	server := func(cfg config.Config, humans humanapi.Authenticator) *humanapi.Server {
		return humanServer(cfg, humans, fake.NewClientBuilder().Build(), "agent-system", fakeRoomLog{},
			fanout.New(fakeRoomLog{}, nil, log), fakeRuns{}, &humanapi.Actor{}, m, log)
	}
	local := config.Config{PublicURL: "http://localhost:8080", Human: config.HumanConfig{Mode: config.HumanModeLocal}}
	s := server(local, authn.NewLocal(local.PublicURL, []string{"admin", "member"}))
	if s.Groups != (policy.Groups{Admin: "admin", Member: "member"}) || !slices.Equal(s.Origins, authn.LoopbackOrigins(local.PublicURL)) ||
		s.HumanMode != config.HumanModeLocal || s.WebClient() != authn.LocalClientID {
		t.Fatalf("local server: groups %+v origins %q mode %q web %q", s.Groups, s.Origins, s.HumanMode, s.WebClient())
	}
	oidc := config.Config{PublicURL: "https://rooms.example", Human: config.HumanConfig{Issuer: humanIssuer,
		Origin: "https://rooms.example", Groups: config.GroupsConfig{Admin: "agents-admin", Member: "agents-member"}}}
	o := server(oidc, authn.NewHumans(authn.NewVerifierWithKeyfunc(humanIssuer, nil), idFile(""), idFile(""), idFile(""), oidc.Human.Origin))
	if o.Groups != (policy.Groups{Admin: "agents-admin", Member: "agents-member"}) || o.Origins != nil || o.HumanMode != config.HumanModeOIDC {
		t.Fatalf("oidc server: groups %+v origins %q mode %q", o.Groups, o.Origins, o.HumanMode)
	}
}
