// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/Smana/agent-platform/internal/envelope"
)

// ForwardedAccessHeader carries the JWT access token oauth2-proxy forwards beside
// the ID token. Not named ...TokenHeader: gosec G101 reads that as a credential.
const ForwardedAccessHeader = "X-Forwarded-Access-Token"

// Humans re-validates what oauth2-proxy forwards (§3, S5): the ID token in
// Authorization and the JWT access token in X-Forwarded-Access-Token, same sub.
// The broker never trusts a header oauth2-proxy merely asserts. roomctl sends its
// own access token as a bearer, which oauth2-proxy passes through (phase 6).
type Humans struct {
	v *Verifier
	// Read at use from mounted files: ZITADEL mints new ids on every gcp-0 build
	// (Ruling AS-a), and a rotation must need no restart.
	projectID func() string
	web       func() string
	roomctl   func() string // "" until phase 6
	origin    string
}

// NewHumans authenticates the humans of one ZITADEL project. The id functions
// are called on every request; an unreadable id returns "", which refuses.
func NewHumans(v *Verifier, projectID, webClientID, roomctlClientID func() string, origin string) *Humans {
	return &Humans{v: v, projectID: projectID, web: webClientID, roomctl: roomctlClientID, origin: origin}
}

// VerifyHuman checks a ZITADEL ID or access token (Ruling AS). Its aud holds the
// project id and an allowlisted rooms client, and azp names that client:
// ZITADEL lists every app of the project in aud, so only azp says which app the
// token was issued to. Machine tokens stay on Verify's one-audience rule (AF).
// The accepted client is the returned claims' AuthorizedParty.
func (h *Humans) VerifyHuman(ctx context.Context, raw string) (*Claims, error) {
	c, err := h.v.parse(ctx, raw, h.projectID())
	if err != nil {
		return nil, err
	}
	clients := slices.DeleteFunc([]string{h.web(), h.roomctl()}, func(s string) bool { return s == "" })
	if c.AuthorizedParty == "" || !slices.Contains(clients, c.AuthorizedParty) || !slices.Contains(c.Audience, c.AuthorizedParty) {
		return nil, fmt.Errorf("%w: not issued to a rooms client", ErrWrongAudience)
	}
	if c.Subject == "" {
		return nil, fmt.Errorf("%w: no subject", ErrUnauthenticated)
	}
	return c, nil
}

// Authenticate maps a viewer's request to a human principal. A foreign Origin is
// ErrForbidden; every other refusal wraps ErrUnauthenticated. ClientID says which
// client the session came through, so callers can tell the web UI from roomctl
// (ruling P18).
func (h *Humans) Authenticate(r *http.Request) (Principal, error) {
	// T9: a browser always sends Origin on a WebSocket upgrade; another site's page must not ride the cookie.
	if o := r.Header.Get("Origin"); o != "" && o != h.origin {
		return Principal{}, fmt.Errorf("%w: foreign origin", ErrForbidden)
	}
	raw, err := Bearer(r)
	if err != nil {
		return Principal{}, err
	}
	id, err := h.VerifyHuman(r.Context(), raw)
	if err != nil {
		return Principal{}, err
	}
	if cli := h.roomctl(); cli != "" && id.AuthorizedParty == cli {
		return Principal{Kind: envelope.ActorHuman, ID: "human:" + id.Subject, Sub: id.Subject, Groups: id.GroupNames(),
			ClientID: cli, Expiry: id.ExpiresAt.Time, AccessToken: raw}, nil
	}
	// A web session carries two different tokens. A bearer oauth2-proxy lets
	// through (skip-jwt-bearer-tokens, phase 6) arrives as both at once, and must
	// never pass as a web session, which may steer and decide (ruling P18, review M16).
	access := r.Header.Get(ForwardedAccessHeader)
	if access == "" || access == raw {
		return Principal{}, fmt.Errorf("%w: a web session needs a distinct access token", ErrUnauthenticated)
	}
	ac, err := h.VerifyHuman(r.Context(), access)
	if err != nil || ac.AuthorizedParty != id.AuthorizedParty || ac.Subject != id.Subject {
		return Principal{}, fmt.Errorf("%w: the access token does not match the ID token", ErrUnauthenticated)
	}
	exp := id.ExpiresAt.Time
	if ac.ExpiresAt.Before(exp) {
		exp = ac.ExpiresAt.Time
	}
	return Principal{Kind: envelope.ActorHuman, ID: "human:" + id.Subject, Sub: id.Subject, Groups: id.GroupNames(),
		ClientID: id.AuthorizedParty, Expiry: exp, AccessToken: access}, nil
}
