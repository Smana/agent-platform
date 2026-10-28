// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
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

// LoopbackOrigins is every origin a local broker answers as itself: publicURL first, then its port
// under each loopback name. kubectl port-forward listens on 127.0.0.1 and [::1] and keeps the Host
// the client sent, so these three names on that port are the listener's own; a DNS-rebound page
// keeps a Host of its own and matches none. A publicURL that is not http://host:port gives itself.
func LoopbackOrigins(publicURL string) []string {
	out := []string{publicURL}
	u, err := url.Parse(publicURL)
	if err != nil || u.Scheme != "http" || u.Port() == "" {
		return out
	}
	for _, h := range []string{"localhost", "127.0.0.1", "[::1]"} {
		if o := "http://" + h + ":" + u.Port(); !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	return out
}

// Local authenticates every request as LocalPrincipal. Reaching the loopback listener is the
// only credential, so two checks stand in for the token: T9's Origin (a page from another
// origin must not act through the user's port-forward) and the Host (one of publicURL's loopback
// names on its port: a DNS-rebound page keeps its own Host, which kubectl port-forward preserves).
type Local struct {
	origins, hosts []string
	groups         []string
	now            func() time.Time
}

// NewLocal accepts requests from publicURL's loopback origins (or with no Origin, as non-browser
// clients send), and only with a Host those origins name.
func NewLocal(publicURL string, groups []string) *Local {
	return NewLocalWithClock(publicURL, groups, time.Now)
}

// NewLocalWithClock is NewLocal with the clock a session's expiry is measured on: the one-hour life
// is what humanapi caps a connection at, so a test pins it rather than racing the wall clock.
func NewLocalWithClock(publicURL string, groups []string, now func() time.Time) *Local {
	if now == nil {
		now = time.Now
	}
	origins := LoopbackOrigins(publicURL)
	hosts := make([]string, 0, len(origins))
	for _, o := range origins {
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			if h := strings.ToLower(u.Host); !slices.Contains(hosts, h) {
				hosts = append(hosts, h)
			}
		}
	}
	return &Local{origins: origins, hosts: hosts, groups: groups, now: now}
}

// Authenticate returns LocalPrincipal, in every group the broker knows.
func (l *Local) Authenticate(r *http.Request) (Principal, error) {
	if !slices.Contains(l.hosts, strings.ToLower(r.Host)) {
		return Principal{}, fmt.Errorf("%w: foreign host", ErrForbidden)
	}
	if o := r.Header.Get("Origin"); o != "" && !slices.Contains(l.origins, o) {
		return Principal{}, fmt.Errorf("%w: foreign origin", ErrForbidden)
	}
	return Principal{Kind: envelope.ActorHuman, ID: LocalPrincipal, Sub: "local", Groups: l.groups,
		ClientID: LocalClientID, Expiry: l.now().Add(localTTL)}, nil
}
