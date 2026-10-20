// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
)

// LocalPrincipal is the one human of a local-mode broker (adoption 3b): whoever reaches the
// loopback listener through kubectl port-forward, which only a cluster user can open.
const LocalPrincipal = "human:local"

// LocalClientID is the local user's rooms client. Every client of the loopback listener is the
// one human, so the web UI/CLI split (P18) does not apply: the broker's WebClient is this id too.
const LocalClientID = "local"

// localTTL is how long a local session lives: humanapi caps a connection at its principal's
// Expiry, and the UI reconnects on close code 4001.
const localTTL = time.Hour

// Local authenticates every request as LocalPrincipal. Reaching the loopback listener is the
// only credential, so two checks stand in for the token: T9's Origin (a page from another
// origin must not act through the user's port-forward) and the Host (a DNS-rebound page sends
// no Origin on a same-origin GET but keeps its own Host, which kubectl port-forward preserves).
type Local struct {
	origin, host string
	groups       []string
	now          func() time.Time
}

// NewLocal accepts requests from origin (or with no Origin, as non-browser clients send), and
// only with the Host that origin names.
func NewLocal(origin string, groups []string) *Local {
	var host string
	if u, err := url.Parse(origin); err == nil {
		host = u.Host
	}
	return &Local{origin: origin, host: host, groups: groups, now: time.Now}
}

// Authenticate returns LocalPrincipal, in every group the broker knows.
func (l *Local) Authenticate(r *http.Request) (Principal, error) {
	if r.Host != l.host {
		return Principal{}, fmt.Errorf("%w: foreign host", ErrForbidden)
	}
	if o := r.Header.Get("Origin"); o != "" && o != l.origin {
		return Principal{}, fmt.Errorf("%w: foreign origin", ErrForbidden)
	}
	return Principal{Kind: envelope.ActorHuman, ID: LocalPrincipal, Sub: "local", Groups: l.groups,
		ClientID: LocalClientID, Expiry: l.now().Add(localTTL)}, nil
}
