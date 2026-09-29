// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/config"
	"github.com/Smana/agent-platform/internal/metrics"
)

const (
	clusterIssuer = "https://oidc.example.test/id/X"
	humanIssuer   = "https://auth.example.test"
)

func writeID(t *testing.T, path, id string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Ruling AQ: the human verifier joins the refreshed and gauged set; Ruling AS-a:
// its ids are read at use, so a new ZITADEL build needs no restart.
func TestAuthenticators(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	clientFile, projectFile := filepath.Join(dir, "client-id"), filepath.Join(dir, "project-id")
	writeID(t, clientFile, "web-1")
	writeID(t, projectFile, "project-1")
	cfg := config.Config{
		RunIssuers:   []config.IssuerConfig{{Issuer: clusterIssuer, JWKSURL: clusterIssuer + "/keys", SubPattern: `^run-(\w+)$`}},
		SystemIssuer: config.IssuerConfig{Issuer: clusterIssuer, JWKSURL: clusterIssuer + "/keys"},
		Human: config.HumanConfig{Issuer: humanIssuer, JWKSURL: humanIssuer + "/oauth/v2/keys",
			ClientIDFile: clientFile, ProjectIDFile: projectFile, Origin: "https://rooms.example.test"},
	}
	var built []string
	build := func(_ context.Context, issuer, jwksURL string) (*authn.Verifier, error) {
		built = append(built, issuer+" "+jwksURL)
		return authn.NewVerifierWithKeyfunc(issuer, func(*jwt.Token) (any, error) { return &key.PublicKey, nil }), nil
	}
	m, err := metrics.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := authenticators(t.Context(), cfg, m, build)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{clusterIssuer + " " + clusterIssuer + "/keys", humanIssuer + " " + humanIssuer + "/oauth/v2/keys"}
	if !slices.Equal(built, want) || len(a.verifiers) != 2 || a.runs == nil || a.systems == nil || a.humans == nil {
		t.Fatalf("built %v, %d verifiers; want one per issuer, the human one refreshed too", built, len(a.verifiers))
	}
	token := func(client, project string) string {
		raw, err := jwt.NewWithClaims(jwt.SigningMethodRS256, authn.Claims{
			RegisteredClaims: jwt.RegisteredClaims{Issuer: humanIssuer, Subject: "2918", Audience: jwt.ClaimStrings{client, project},
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
			AuthorizedParty: client,
		}).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if _, err := a.humans.VerifyHuman(t.Context(), token("web-1", "project-1")); err != nil {
		t.Fatalf("the ids read from the files: %v", err)
	}
	writeID(t, clientFile, "web-2")
	writeID(t, projectFile, "project-2")
	if _, err := a.humans.VerifyHuman(t.Context(), token("web-1", "project-1")); !errors.Is(err, authn.ErrUnauthenticated) {
		t.Fatalf("the previous build's ids: %v", err)
	}
	if _, err := a.humans.VerifyHuman(t.Context(), token("web-2", "project-2")); err != nil {
		t.Fatalf("a rewritten file takes effect without a restart: %v", err)
	}
	if err := os.Remove(projectFile); err != nil {
		t.Fatal(err)
	}
	if _, err := a.humans.VerifyHuman(t.Context(), token("web-2", "project-2")); !errors.Is(err, authn.ErrUnauthenticated) {
		t.Fatalf("an unreadable project id refuses: %v", err)
	}
}

func TestAuthenticatorsFailsOnAnIssuer(t *testing.T) {
	cfg := config.Config{
		RunIssuers:   []config.IssuerConfig{{Issuer: clusterIssuer, JWKSURL: clusterIssuer + "/keys", SubPattern: `^run-(\w+)$`}},
		SystemIssuer: config.IssuerConfig{Issuer: clusterIssuer, JWKSURL: clusterIssuer + "/keys"},
		Human:        config.HumanConfig{Issuer: humanIssuer, JWKSURL: humanIssuer + "/oauth/v2/keys"},
	}
	down := errors.New("jwks unreachable")
	build := func(_ context.Context, issuer, _ string) (*authn.Verifier, error) {
		if issuer == humanIssuer {
			return nil, down
		}
		return authn.NewVerifierWithKeyfunc(issuer, func(*jwt.Token) (any, error) { return nil, errors.New("unused") }), nil
	}
	m, err := metrics.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authenticators(t.Context(), cfg, m, build); !errors.Is(err, down) {
		t.Fatalf("got %v, want the human issuer's failure", err)
	}
}
