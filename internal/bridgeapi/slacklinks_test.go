// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Smana/maku/internal/wire"
)

type linkTable map[string]string

func (l linkTable) SlackLink(_ context.Context, team, user string) (string, bool, error) {
	if team == "T0DOWN" {
		return "", false, errors.New("database down")
	}
	sub, ok := l[team+"/"+user]
	return sub, ok, nil
}

// The factory alone reads a Slack user's link, as a ZITADEL sub; any other system principal is
// not permitted, an unlinked user is a 404, a malformed id a 400, a store failure a 503.
func TestSlackLinkIsTheFactorysToRead(t *testing.T) {
	s, _, _ := newServer(t)
	s.SlackLinks, s.SlackLinkReader = linkTable{"T01ABC/U02DEF": "zsub1"}, factory
	h := s.Routes()
	rec := call(t, h, http.MethodGet, "/v1/links/slack?team=T01ABC&user=U02DEF", "sys:"+factory, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"sub\":\"zsub1\"}\n" {
		t.Fatalf("the factory's read: %d %s", rec.Code, rec.Body)
	}
	for name, c := range map[string]struct {
		path, token string
		code        int
		reason      string
	}{
		"another system principal": {"/v1/links/slack?team=T01ABC&user=U02DEF", "sys:system:runlore", http.StatusForbidden, wire.ReasonNotPermitted},
		"no token":                 {"/v1/links/slack?team=T01ABC&user=U02DEF", "", http.StatusUnauthorized, wire.ReasonUnauthenticated},
		"not linked":               {"/v1/links/slack?team=T01ABC&user=U09NONE", "sys:" + factory, http.StatusNotFound, wire.ReasonNotLinked},
		"a malformed user":         {"/v1/links/slack?team=T01ABC&user=x", "sys:" + factory, http.StatusBadRequest, wire.ReasonBadSlackUser},
		"the store down":           {"/v1/links/slack?team=T0DOWN&user=U02DEF", "sys:" + factory, http.StatusServiceUnavailable, wire.ReasonLogUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			rec := call(t, h, http.MethodGet, c.path, c.token, nil)
			if rec.Code != c.code || reason(t, rec) != c.reason {
				t.Fatalf("%d %s, want %d %s", rec.Code, reason(t, rec), c.code, c.reason)
			}
		})
	}
	s.SlackLinkReader = ""
	if rec := call(t, s.Routes(), http.MethodGet, "/v1/links/slack?team=T01ABC&user=U02DEF", "sys:"+factory, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("off: %d", rec.Code)
	}
}
