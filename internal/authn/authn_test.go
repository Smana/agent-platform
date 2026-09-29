// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"bytes"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const runSub = "system:serviceaccount:agents:xplane-run-7f3cq2xz"

var runPattern = regexp.MustCompile(`^system:serviceaccount:agents:xplane-run-([a-z2-7]{8})$`)

func mustRuns(t *testing.T, issuers ...RunIssuer) *Runs {
	t.Helper()
	r, err := NewRuns(issuers...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNewRunsRefuses(t *testing.T) {
	v := newSigner(t).verifier()
	cases := map[string][]RunIssuer{
		"no issuer":            nil,
		"no verifier":          {{SubPattern: runPattern}},
		"no pattern":           {{Verifier: v}},
		"no capture group":     {{Verifier: v, SubPattern: regexp.MustCompile(`^xplane-run-[a-z2-7]{8}$`)}},
		"two capture groups":   {{Verifier: v, SubPattern: regexp.MustCompile(`^(xplane)-run-([a-z2-7]{8})$`)}},
		"one good, one broken": {{Verifier: v, SubPattern: runPattern}, {Verifier: v}},
	}
	for name, issuers := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewRuns(issuers...); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// Error text reaches logs; the subject claim must not ride along (package doc).
func TestRefusalsNeverEchoTheSubject(t *testing.T) {
	s := newSigner(t)
	const odd = "system:serviceaccount:elsewhere:not-a-run"
	runs := mustRuns(t, RunIssuer{Verifier: s.verifier(), SubPattern: runPattern})
	_, runErr := runs.Authenticate(request(t, s.token(t, issuer, odd, AudienceRun, time.Minute)))
	sys := NewSystems(s.verifier(), map[string]string{})
	_, sysErr := sys.Authenticate(request(t, s.token(t, issuer, odd, AudienceSystem, time.Minute)))
	for name, err := range map[string]error{"runs": runErr, "systems": sysErr} {
		if err == nil || strings.Contains(err.Error(), odd) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func request(t *testing.T, token string) *http.Request {
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/bridge/hello", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestRunTokens(t *testing.T) {
	s := newSigner(t)
	runs := mustRuns(t, RunIssuer{Verifier: s.verifier(), SubPattern: runPattern})
	cases := map[string]struct {
		token string
		ok    bool
	}{
		"valid":            {s.token(t, issuer, runSub, AudienceRun, 10*time.Minute), true},
		"gateway audience": {s.token(t, issuer, runSub, "agent-router.implementer.public", 10*time.Minute), false},
		"other namespace":  {s.token(t, issuer, "system:serviceaccount:agent-system:xplane-run-7f3cq2xz", AudienceRun, 10*time.Minute), false},
		"expired":          {s.token(t, issuer, runSub, AudienceRun, -2*time.Minute), false},
		"foreign issuer":   {s.token(t, "https://oidc.eks.eu-west-1.amazonaws.com/id/OTHER", runSub, AudienceRun, 10*time.Minute), false},
		"system audience":  {s.token(t, issuer, runSub, AudienceSystem, 10*time.Minute), false},
		"no token":         {"", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := runs.Authenticate(request(t, c.token))
			if c.ok && (err != nil || p.ID != "agent:7f3cq2xz" || p.RunID != "7f3cq2xz" || p.Kind != "agent") {
				t.Errorf("%+v %v", p, err)
			}
			if !c.ok && !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("accepted (%v)", err)
			}
		})
	}
}

// A pattern the caller forgot to anchor must not accept a subject that merely
// contains a run's name.
func TestRunSubjectMustMatchWhole(t *testing.T) {
	s := newSigner(t)
	runs := mustRuns(t, RunIssuer{Verifier: s.verifier(), SubPattern: regexp.MustCompile(`xplane-run-([a-z2-7]{8})`)})
	tok := s.token(t, issuer, "system:serviceaccount:evil:xplane-run-7f3cq2xz-x", AudienceRun, time.Minute)
	if _, err := runs.Authenticate(request(t, tok)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a partial subject match was accepted: %v", err)
	}
}

// Each issuer checks only its own tokens: another issuer's keys are never
// consulted, so a foreign kid cannot trigger a JWKS refresh there.
func TestRunsRouteByIssuer(t *testing.T) {
	a, b := newSigner(t), newSigner(t)
	const issuerB = "https://container.googleapis.com/v1/projects/p/locations/l/clusters/c"
	var aCalls int
	runs := mustRuns(t,
		RunIssuer{Verifier: NewVerifierWithKeyfunc(issuer, func(*jwt.Token) (any, error) {
			aCalls++
			return &a.key.PublicKey, nil
		}), SubPattern: runPattern},
		RunIssuer{Verifier: NewVerifierWithKeyfunc(issuerB, func(*jwt.Token) (any, error) {
			return &b.key.PublicKey, nil
		}), SubPattern: runPattern},
	)
	p, err := runs.Authenticate(request(t, b.token(t, issuerB, runSub, AudienceRun, time.Minute)))
	if err != nil || p.RunID != "7f3cq2xz" {
		t.Fatalf("%+v %v", p, err)
	}
	if aCalls != 0 {
		t.Fatalf("issuer A's keys were consulted %d times for issuer B's token", aCalls)
	}
}

func TestSystemTokens(t *testing.T) {
	s := newSigner(t)
	sys := NewSystems(s.verifier(), map[string]string{
		"system:serviceaccount:agent-system:agent-factory": "system:factory",
	})
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/rooms/3kq7x2ma/events", nil)
	r.Header.Set("Authorization", "Bearer "+s.token(t, issuer, "system:serviceaccount:agent-system:agent-factory", AudienceSystem, time.Minute))
	if p, err := sys.Authenticate(r); err != nil || p.ID != "system:factory" || p.Kind != "system" {
		t.Fatalf("factory: %+v %v", p, err)
	}
	r.Header.Set("Authorization", "Bearer "+s.token(t, issuer, "system:serviceaccount:agent-system:intruder", AudienceSystem, time.Minute))
	if _, err := sys.Authenticate(r); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a valid but unlisted subject must be forbidden, got %v", err)
	}
	r.Header.Set("Authorization", "Bearer "+s.token(t, issuer, runSub, AudienceRun, time.Minute))
	if _, err := sys.Authenticate(r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a run token on the system API must be unauthenticated, got %v", err)
	}
	r.Header.Del("Authorization")
	if _, err := sys.Authenticate(r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("no token: %v", err)
	}
}

func TestVerify(t *testing.T) {
	s := newSigner(t)
	clock := newClock()
	t0 := clock.now()
	v := s.verifier(WithClock(clock.now))
	at := func(d time.Duration) *jwt.NumericDate { return jwt.NewNumericDate(t0.Add(d)) }
	claims := func(mut func(*jwt.RegisteredClaims)) jwt.RegisteredClaims {
		c := jwt.RegisteredClaims{Issuer: issuer, Subject: runSub, Audience: jwt.ClaimStrings{AudienceRun},
			IssuedAt: at(-time.Minute), ExpiresAt: at(10 * time.Minute)}
		if mut != nil {
			mut(&c)
		}
		return c
	}
	rs256 := func(mut func(*jwt.RegisteredClaims)) string {
		return sign(t, jwt.SigningMethodRS256, s.key, "", claims(mut))
	}
	// The classic confusion: HS256 keyed with the RSA public key an attacker can read.
	pubDER, err := x509.MarshalPKIXPublicKey(&s.key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	valid := rs256(nil)
	tampered := valid[:len(valid)-4] + "AAAA"

	cases := map[string]struct {
		raw  string
		want error // nil accepts; every refusal must also be ErrUnauthenticated
	}{
		"valid":                       {raw: valid},
		"expired within the skew":     {raw: rs256(func(c *jwt.RegisteredClaims) { c.ExpiresAt = at(-20 * time.Second) })},
		"expired past the skew":       {raw: rs256(func(c *jwt.RegisteredClaims) { c.ExpiresAt = at(-31 * time.Second) }), want: ErrTokenExpired},
		"not before, within the skew": {raw: rs256(func(c *jwt.RegisteredClaims) { c.NotBefore = at(20 * time.Second) })},
		"not before, past the skew":   {raw: rs256(func(c *jwt.RegisteredClaims) { c.NotBefore = at(time.Minute) }), want: ErrTokenNotYetValid},
		"issued in the future":        {raw: rs256(func(c *jwt.RegisteredClaims) { c.IssuedAt = at(time.Minute) }), want: ErrTokenNotYetValid},
		"no expiry":                   {raw: rs256(func(c *jwt.RegisteredClaims) { c.ExpiresAt = nil }), want: ErrUnauthenticated},
		"several audiences, ours among them": {raw: rs256(func(c *jwt.RegisteredClaims) {
			c.Audience = jwt.ClaimStrings{"agent-router.implementer.public", AudienceRun, AudienceSystem}
		}), want: ErrWrongAudience},
		"wrong audience":               {raw: rs256(func(c *jwt.RegisteredClaims) { c.Audience = jwt.ClaimStrings{AudienceSystem} }), want: ErrWrongAudience},
		"wrong issuer":                 {raw: rs256(func(c *jwt.RegisteredClaims) { c.Issuer = "https://evil.example" }), want: ErrWrongIssuer},
		"alg none":                     {raw: sign(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, "", claims(nil)), want: ErrUnauthenticated},
		"HS256 keyed with the RSA key": {raw: sign(t, jwt.SigningMethodHS256, pubDER, "", claims(nil)), want: ErrUnauthenticated},
		"RS512 is not pinned":          {raw: sign(t, jwt.SigningMethodRS512, s.key, "", claims(nil)), want: ErrUnauthenticated},
		"tampered signature":           {raw: tampered, want: ErrUnauthenticated},
		"not a JWT":                    {raw: "not.a.jwt", want: ErrUnauthenticated},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := v.Verify(t.Context(), c.raw, AudienceRun)
			if c.want == nil {
				if err != nil || got.Subject != runSub {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !errors.Is(err, c.want) || !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
	if _, err := v.Verify(t.Context(), valid, ""); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("an empty audience must never match: %v", err)
	}
}

func TestBearer(t *testing.T) {
	cases := map[string]struct {
		header string
		want   string
	}{
		"bearer":          {"Bearer abc", "abc"},
		"scheme any case": {"bearer abc", "abc"},
		"no header":       {"", ""},
		"basic":           {"Basic abc", ""},
		"empty token":     {"Bearer ", ""},
		"no space":        {"Bearerabc", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			if c.header != "" {
				r.Header.Set("Authorization", c.header)
			}
			got, err := Bearer(r)
			if c.want == "" {
				if !errors.Is(err, ErrUnauthenticated) {
					t.Fatalf("accepted %q (%v)", got, err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}

// A human's access token rides on the Principal; no format or log path shows it.
func TestPrincipalNeverPrintsTheAccessToken(t *testing.T) {
	const secret = "eyJhbGciOiJSUzI1NiJ9.access.token"
	p := Principal{Kind: "human", ID: "human:123", Sub: "123", AccessToken: secret}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("caller", "principal", p)
	for name, out := range map[string]string{
		"%v": fmt.Sprintf("%v", p), "%+v": fmt.Sprintf("%+v", p), "%#v": fmt.Sprintf("%#v", p), "slog": logged.String(),
	} {
		if strings.Contains(out, secret) {
			t.Errorf("%s shows the access token: %s", name, out)
		}
		if !strings.Contains(out, "human:123") {
			t.Errorf("%s lost the principal id: %s", name, out)
		}
	}
}
