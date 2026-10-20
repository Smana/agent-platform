// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"fmt"
	"net/http"

	"github.com/Smana/agent-platform/internal/envelope"
)

// LocalPrincipal is the one human of a local-mode broker (adoption 3b): whoever reaches the
// loopback listener through kubectl port-forward, which only a cluster user can open.
const LocalPrincipal = "human:local"

// Local authenticates every request as LocalPrincipal. The only check left is T9's: a browser
// page from another origin must not act through the user's port-forward.
type Local struct {
	origin string
	groups []string
}

// NewLocal accepts requests from origin (or with no Origin, as non-browser clients send).
func NewLocal(origin string, groups []string) *Local { return &Local{origin: origin, groups: groups} }

// Authenticate returns LocalPrincipal, in every group the broker knows.
func (l *Local) Authenticate(r *http.Request) (Principal, error) {
	if o := r.Header.Get("Origin"); o != "" && o != l.origin {
		return Principal{}, fmt.Errorf("%w: foreign origin", ErrForbidden)
	}
	return Principal{Kind: envelope.ActorHuman, ID: LocalPrincipal, Sub: "local", Groups: l.groups}, nil
}
