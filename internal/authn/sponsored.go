// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"fmt"
	"slices"

	"github.com/Smana/agent-platform/internal/envelope"
)

// SponsoredClient is one sponsored principal's ZITADEL client, its id read at use (Ruling AS-a).
type SponsoredClient struct {
	Name     string
	ClientID func() string
}

// Sponsored authenticates a sponsored principal (factory interfaces F4, R2): a ZITADEL service
// account's access token, issued to one of Clients through client credentials. The client id is
// the claim ZITADEL sets from the authenticated client; nothing else in the token names the
// principal. Exclude lists the human clients' ids: a principal whose id equals one never matches.
// Every error fails closed: no verifier, an unreadable id, or two principals resolving to the
// same id (the runtime twin of the config's duplicate-file check) admit nobody on that id.
type Sponsored struct {
	V       *Verifier
	Clients []SponsoredClient
	Exclude func() []string
}

// Authenticate maps raw to its sponsored principal, or ErrUnauthenticated.
func (s *Sponsored) Authenticate(ctx context.Context, raw string) (Principal, error) {
	refused := fmt.Errorf("%w: not a sponsored principal", ErrUnauthenticated)
	if s == nil || s.V == nil {
		return Principal{}, refused
	}
	var human []string
	if s.Exclude != nil {
		human = s.Exclude()
	}
	ids := make([]string, len(s.Clients))
	for i, c := range s.Clients {
		if c.ClientID != nil {
			ids[i] = c.ClientID()
		}
	}
	for i, c := range s.Clients {
		id := ids[i]
		if c.Name == "" || id == "" || slices.Contains(human, id) {
			continue
		}
		if slices.Index(ids, id) != i || slices.Contains(ids[i+1:], id) { // shared: ambiguous, so nobody's
			continue
		}
		cl, err := s.V.VerifyAuthorizedParty(ctx, raw, id)
		if err != nil || cl.ExpiresAt == nil || cl.Subject == "" {
			continue
		}
		return Principal{Kind: envelope.ActorSystem, ID: "system:" + c.Name, Sub: cl.Subject, ClientID: id,
			Expiry: cl.ExpiresAt.Time, AccessToken: raw, Sponsored: true}, nil
	}
	return Principal{}, refused
}
