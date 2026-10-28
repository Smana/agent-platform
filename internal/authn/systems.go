// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"fmt"
	"net/http"

	"github.com/Smana/maku/internal/envelope"
)

// Systems authenticates system:* callers of the :8443 API (SP3's factory). The
// allowlist maps a ServiceAccount subject to its canonical principal.
type Systems struct {
	v     RunVerifier // nil: no system issuer is configured, and nothing authenticates
	allow map[string]string
}

// NewSystems allowlists subjects: allow maps a token's sub to a system:<component> id. A nil v is
// the intentional never-authenticating path, so a broker without a system issuer still answers 401
// on the system routes rather than 404.
func NewSystems(v RunVerifier, allow map[string]string) *Systems { return &Systems{v: v, allow: allow} }

// Authenticate maps a system caller's token to its principal. A valid token for
// an unlisted subject is ErrForbidden, not ErrUnauthenticated.
func (s *Systems) Authenticate(req *http.Request) (Principal, error) {
	if s.v == nil {
		return Principal{}, fmt.Errorf("%w: no system issuer is configured", ErrUnauthenticated)
	}
	raw, err := Bearer(req)
	if err != nil {
		return Principal{}, err
	}
	c, err := s.v.Verify(req.Context(), raw, AudienceSystem)
	if err != nil {
		return Principal{}, err
	}
	id, ok := s.allow[c.Subject]
	if !ok {
		return Principal{}, fmt.Errorf("%w: the subject is not an allowlisted system principal", ErrForbidden)
	}
	return Principal{Kind: envelope.ActorSystem, ID: id, Sub: c.Subject, Expiry: c.ExpiresAt.Time}, nil
}
