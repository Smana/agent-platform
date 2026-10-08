// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ZITADEL-shaped ids; the real ones are read from mounted files (Ruling AS-a).
const (
	project   = "project-1"
	webClient = "web-client"
	cliClient = "roomctl-client"
	uiOrigin  = "https://rooms.example.test"
)

// humanToken mints a ZITADEL-shaped ID token: aud holds the client and the
// project, and the client is in both azp and client_id.
func (s signer) humanToken(t *testing.T, sub string, aud []string, azp string, groups []string, ttl time.Duration) string {
	t.Helper()
	return sign(t, jwt.SigningMethodRS256, s.key, "", Claims{
		RegisteredClaims: jwt.RegisteredClaims{Issuer: issuer, Subject: sub, Audience: aud,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl))},
		Groups: groups, AuthorizedParty: azp, ClientID: azp,
	})
}

// accessToken mints a ZITADEL-shaped JWT access token: the client is in
// client_id, never in azp (zitadel/oidc NewAccessTokenClaims).
func (s signer) accessToken(t *testing.T, sub string, aud []string, clientID string, groups []string, ttl time.Duration) string {
	t.Helper()
	return sign(t, jwt.SigningMethodRS256, s.key, "", Claims{
		RegisteredClaims: jwt.RegisteredClaims{Issuer: issuer, Subject: sub, Audience: aud,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl))},
		Groups: groups, ClientID: clientID,
	})
}

// azpOnlyToken is an access token of the shape ZITADEL never mints: azp set, no client_id.
func (s signer) azpOnlyToken(t *testing.T, sub string, aud []string, azp string) string {
	t.Helper()
	return sign(t, jwt.SigningMethodRS256, s.key, "", Claims{
		RegisteredClaims: jwt.RegisteredClaims{Issuer: issuer, Subject: sub, Audience: aud,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
		AuthorizedParty: azp,
	})
}

func (s signer) humans(projectID, roomctl string) *Humans {
	return NewHumans(s.verifier(), func() string { return projectID },
		func() string { return webClient }, func() string { return roomctl }, uiOrigin)
}

func TestVerifyHuman(t *testing.T) {
	s := newSigner(t)
	web := []string{webClient, project}
	cases := []struct {
		name       string
		token      string
		projectID  string
		roomctl    string
		wantErr    error // nil: accepted
		wantClient string
	}{
		{"a web ID token", s.humanToken(t, "2918", web, webClient, nil, time.Hour), project, "", nil, webClient},
		{"a roomctl token once its client is set",
			s.humanToken(t, "2918", []string{cliClient, project}, cliClient, nil, time.Hour), project, cliClient, nil, cliClient},
		{"another project app lists our client in aud, but azp names it",
			s.humanToken(t, "2918", []string{"other-app", webClient, project}, "other-app", nil, time.Hour), project, "", ErrWrongAudience, ""},
		{"a roomctl token before roomctl has a client",
			s.humanToken(t, "2918", []string{cliClient, project}, cliClient, nil, time.Hour), project, "", ErrWrongAudience, ""},
		{"an aud without the project id",
			s.humanToken(t, "2918", []string{webClient}, webClient, nil, time.Hour), project, "", ErrWrongAudience, ""},
		{"a client outside the allowlist",
			s.humanToken(t, "2918", []string{"other-app", project}, "other-app", nil, time.Hour), project, "", ErrWrongAudience, ""},
		{"an azp that is not in aud",
			s.humanToken(t, "2918", web, cliClient, nil, time.Hour), project, cliClient, ErrWrongAudience, ""},
		{"no subject", s.humanToken(t, "", web, webClient, nil, time.Hour), project, "", ErrUnauthenticated, ""},
		{"no azp with several audiences", s.humanToken(t, "2918", web, "", nil, time.Hour), project, "", ErrWrongAudience, ""},
		{"an access token: client_id, no azp", s.accessToken(t, "2918", web, webClient, nil, time.Hour), project, "", ErrWrongAudience, ""},
		{"a machine token", s.token(t, issuer, runSub, AudienceRun, time.Hour), project, "", ErrWrongAudience, ""},
		{"no project id to check (an unreadable file)", s.humanToken(t, "2918", web, webClient, nil, time.Hour), "", "", ErrUnauthenticated, ""},
		{"an expired token", s.humanToken(t, "2918", web, webClient, nil, -time.Hour), project, "", ErrTokenExpired, ""},
		{"another issuer", sign(t, jwt.SigningMethodRS256, s.key, "", Claims{
			RegisteredClaims: jwt.RegisteredClaims{Issuer: "https://elsewhere.test", Subject: "2918", Audience: web,
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}, AuthorizedParty: webClient}),
			project, "", ErrWrongIssuer, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.humans(c.projectID, c.roomctl).VerifyHuman(t.Context(), c.token)
			if c.wantErr == nil {
				if err != nil || got.AuthorizedParty != c.wantClient {
					t.Fatalf("got %+v, %v; want accepted for %s", got, err, c.wantClient)
				}
				return
			}
			if !errors.Is(err, c.wantErr) || !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("got %v, want %v", err, c.wantErr)
			}
		})
	}
}

// A ZITADEL JWT access token names its client in client_id and never in azp
// (review C1): the client must be allowlisted and in aud, beside the project.
func TestVerifyHumanAccess(t *testing.T) {
	s := newSigner(t)
	web := []string{webClient, project}
	cases := []struct {
		name       string
		token      string
		projectID  string
		roomctl    string
		wantErr    error // nil: accepted
		wantClient string
	}{
		{"a web access token", s.accessToken(t, "2918", web, webClient, nil, time.Hour), project, "", nil, webClient},
		{"a roomctl access token once its client is set",
			s.accessToken(t, "2918", []string{cliClient, project}, cliClient, nil, time.Hour), project, cliClient, nil, cliClient},
		{"azp but no client_id, a shape ZITADEL never mints", s.azpOnlyToken(t, "2918", web, webClient), project, "", ErrWrongAudience, ""},
		{"a roomctl access token before roomctl has a client",
			s.accessToken(t, "2918", []string{cliClient, project}, cliClient, nil, time.Hour), project, "", ErrWrongAudience, ""},
		{"a client outside the allowlist",
			s.accessToken(t, "2918", []string{"other-app", webClient, project}, "other-app", nil, time.Hour), project, "", ErrWrongAudience, ""},
		{"a client_id that is not in aud",
			s.accessToken(t, "2918", []string{"other-app", project}, webClient, nil, time.Hour), project, "", ErrWrongAudience, ""},
		{"an aud without the project id", s.accessToken(t, "2918", []string{webClient}, webClient, nil, time.Hour), project, "", ErrWrongAudience, ""},
		{"no subject", s.accessToken(t, "", web, webClient, nil, time.Hour), project, "", ErrUnauthenticated, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.humans(c.projectID, c.roomctl).VerifyHumanAccess(t.Context(), c.token)
			if c.wantErr == nil {
				if err != nil || got.ClientID != c.wantClient {
					t.Fatalf("got %+v, %v; want accepted for %s", got, err, c.wantClient)
				}
				return
			}
			if !errors.Is(err, c.wantErr) || !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("got %v, want %v", err, c.wantErr)
			}
		})
	}
}

// Ruling AF stays for machine tokens: a ZITADEL token is good for two audiences.
func TestVerifyRefusesAHumanToken(t *testing.T) {
	s := newSigner(t)
	tok := s.humanToken(t, "2918", []string{webClient, project}, webClient, nil, time.Hour)
	for _, aud := range []string{webClient, project} {
		t.Run(aud, func(t *testing.T) {
			if _, err := s.verifier().Verify(t.Context(), tok, aud); !errors.Is(err, ErrWrongAudience) {
				t.Fatalf("got %v, want ErrWrongAudience", err)
			}
		})
	}
}

func TestHumansAuthenticate(t *testing.T) {
	s := newSigner(t)
	web := []string{webClient, project}
	id := s.humanToken(t, "2918", web, webClient, []string{"agents-member"}, time.Hour)
	access := s.accessToken(t, "2918", web, webClient, nil, 30*time.Minute)
	cli := s.accessToken(t, "2918", []string{cliClient, project}, cliClient, []string{"agents-member"}, time.Hour)
	type want struct {
		err         error // nil: accepted
		client      string
		accessToken string
	}
	cases := []struct {
		name               string
		bearer, access, og string
		want               want
	}{
		{"a web session from our origin", id, access, uiOrigin, want{client: webClient, accessToken: access}},
		{"a roomctl bearer", cli, "", "", want{client: cliClient, accessToken: cli}},
		{"a cross-site WebSocket (T9)", id, access, "https://evil.example", want{err: ErrForbidden}},
		{"an opaque origin (Origin: null)", id, access, "null", want{err: ErrForbidden}},
		{"our host over another scheme", id, access, "http://rooms.example.test", want{err: ErrForbidden}},
		{"our host on another port", id, access, "https://rooms.example.test:8443", want{err: ErrForbidden}},
		{"an access token for another sub", id, s.accessToken(t, "9999", web, webClient, nil, time.Hour), "", want{err: ErrUnauthenticated}},
		{"an access token with azp but no client_id (review C1)", id, s.azpOnlyToken(t, "2918", web, webClient), "",
			want{err: ErrUnauthenticated}},
		{"a roomctl bearer with azp but no client_id", s.azpOnlyToken(t, "2918", []string{cliClient, project}, cliClient), "", "",
			want{err: ErrUnauthenticated}},
		{"no access token", id, "", "", want{err: ErrUnauthenticated}},
		{"the ID token in both headers, a bearer oauth2-proxy let through (review M16)", id, id, "", want{err: ErrUnauthenticated}},
		{"an access token issued to roomctl", id, s.accessToken(t, "2918", []string{cliClient, project}, cliClient, nil, time.Hour), "",
			want{err: ErrUnauthenticated}},
		{"a token for another ZITADEL app", s.humanToken(t, "2918", []string{"other-app", project}, "other-app", nil, time.Hour), access, "",
			want{err: ErrUnauthenticated}},
		{"no bearer", "", access, "", want{err: ErrUnauthenticated}},
	}
	h := s.humans(project, cliClient)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := request(t, c.bearer)
			if c.access != "" {
				r.Header.Set(ForwardedAccessHeader, c.access)
			}
			if c.og != "" {
				r.Header.Set("Origin", c.og)
			}
			p, err := h.Authenticate(r)
			if c.want.err != nil {
				if !errors.Is(err, c.want.err) {
					t.Fatalf("got %+v, %v; want %v", p, err, c.want.err)
				}
				return
			}
			if err != nil || p.Kind != "human" || p.ID != "human:2918" || p.Sub != "2918" || p.ClientID != c.want.client ||
				p.AccessToken != c.want.accessToken || !slices.Equal(p.Groups, []string{"agents-member"}) {
				t.Fatalf("got %#v %v", p, err)
			}
		})
	}
	t.Run("a web session lives until the earlier of its two tokens", func(t *testing.T) {
		r := request(t, id)
		r.Header.Set(ForwardedAccessHeader, access)
		p, err := h.Authenticate(r)
		if err != nil || p.Expiry.After(time.Now().Add(31*time.Minute)) {
			t.Fatalf("expiry %v, %v", p.Expiry, err)
		}
	})
	// A misconfiguration that mounts the web UI's id as roomctl's must not let a
	// bearer pass as a web session, which may steer and decide (ruling P18).
	t.Run("a roomctl client id equal to the web UI's (review M5)", func(t *testing.T) {
		same := s.humans(project, webClient)
		bearer := s.accessToken(t, "2918", web, webClient, []string{"agents-member"}, time.Hour)
		if p, err := same.Authenticate(request(t, bearer)); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("a bearer: got %+v, %v", p, err)
		}
		r := request(t, id)
		r.Header.Set(ForwardedAccessHeader, access)
		if p, err := same.Authenticate(r); err != nil || p.ClientID != webClient {
			t.Fatalf("the web session still signs in: %+v, %v", p, err)
		}
	})
	t.Run("a refusal never echoes a client id or the subject", func(t *testing.T) {
		r := request(t, s.humanToken(t, "2918", []string{"other-app", project}, "other-app", nil, time.Hour))
		_, err := h.Authenticate(r)
		for _, leak := range []string{"other-app", "2918", project} {
			if err == nil || strings.Contains(err.Error(), leak) {
				t.Fatalf("%v", err)
			}
		}
	})
}

func TestGroupNames(t *testing.T) {
	roles := func(names ...string) map[string]json.RawMessage {
		m := map[string]json.RawMessage{}
		for _, n := range names {
			m[n] = json.RawMessage(`{"123":"org.example"}`)
		}
		return m
	}
	cases := []struct {
		name string
		c    Claims
		want []string
	}{
		{"the groups claim wins", Claims{Groups: []string{"g"}, ProjectRoles: roles("r")}, []string{"g"}},
		{"else the project role keys, sorted", Claims{ProjectRoles: roles("b", "a")}, []string{"a", "b"}},
		{"neither is no group", Claims{}, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.c.GroupNames(); !slices.Equal(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

// The ZITADEL claim decodes into ProjectRoles.
func TestProjectRolesClaim(t *testing.T) {
	var c Claims
	if err := json.Unmarshal([]byte(`{"urn:zitadel:iam:org:project:roles":{"agents-admin":{"1":"o"}}}`), &c); err != nil {
		t.Fatal(err)
	}
	if got := c.GroupNames(); !slices.Equal(got, []string{"agents-admin"}) {
		t.Fatalf("got %v", got)
	}
}
