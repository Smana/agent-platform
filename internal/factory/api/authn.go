// SPDX-License-Identifier: Apache-2.0

// Package api is the run-request API (§4): once SP3 ships, the only way to create an AgentRun.
// The principal comes from the caller's token, never from the body.
package api

import (
	"fmt"
	"net/http"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/policy"
)

// AudienceSystem is what a system caller's projected token carries (R23).
const AudienceSystem = "agent-factory"

// Authenticator maps a run-request to its caller: a human through one of the
// two rooms clients (the broker forwards rooms-proxy's token, roomctl sends its
// own), or an allowlisted system caller on the cluster's issuer.
type Authenticator struct {
	Humans      *authn.Verifier   // ZITADEL, offline against its JWKS
	Groups      policy.Groups     // the two agents groups; a human must be in one (R23)
	ClientIDs   func() []string   // rooms-proxy (the broker forwards it) and roomctl (the CLI)
	Systems     *authn.Verifier   // the cluster's issuer; nil when no system caller is allowed
	SystemAllow map[string]string // ServiceAccount sub → system:<component>; empty ships: none exists yet
}

// Authenticate maps r to its caller's principal, from the bearer token alone.
// A valid token for a rejected caller is not mistaken for an invalid one: an
// unadmitted human and an unlisted system caller are ErrForbidden, every other
// refusal ErrUnauthenticated.
func (a *Authenticator) Authenticate(r *http.Request) (authn.Principal, error) {
	raw, err := authn.Bearer(r)
	if err != nil {
		return authn.Principal{}, err
	}
	for _, id := range a.ClientIDs() {
		if id == "" {
			continue
		}
		// A refusal here is only a client mismatch until every id failed; it must
		// not mask the system branch, so it is skipped, not returned.
		c, err := a.Humans.VerifyAuthorizedParty(r.Context(), raw, id)
		if err != nil {
			continue
		}
		p := authn.Principal{Kind: envelope.ActorHuman, ID: "human:" + c.Subject, Sub: c.Subject, Groups: c.GroupNames(),
			ClientID: id, Expiry: c.ExpiresAt.Time, AccessToken: raw}
		if !a.Groups.Admitted(p) {
			// No claims in the message: it reaches logs (authn package rule).
			return authn.Principal{}, fmt.Errorf("%w: not in an agents group", authn.ErrForbidden)
		}
		return p, nil
	}
	if a.Systems != nil {
		if c, err := a.Systems.Verify(r.Context(), raw, AudienceSystem); err == nil {
			id, ok := a.SystemAllow[c.Subject]
			if !ok {
				return authn.Principal{}, fmt.Errorf("%w: the subject is not an allowlisted system caller", authn.ErrForbidden)
			}
			return authn.Principal{Kind: envelope.ActorSystem, ID: id, Sub: c.Subject, Expiry: c.ExpiresAt.Time}, nil
		}
	}
	return authn.Principal{}, fmt.Errorf("%w: no accepted issuer or client", authn.ErrUnauthenticated)
}
