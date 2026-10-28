// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"context"
	"net/http"
	"regexp"

	"github.com/Smana/maku/internal/wire"
)

// SlackLinks reads the Slack self-serve link records; *store.Store implements it.
type SlackLinks interface {
	SlackLink(ctx context.Context, team, user string) (sub string, found bool, err error)
}

var (
	slackTeamRE = regexp.MustCompile(`^T[A-Z0-9]{2,20}$`)
	slackUserRE = regexp.MustCompile(`^[UW][A-Z0-9]{2,20}$`)
)

// slackLink: GET /v1/links/slack?team=&user=, the link reader's only (SlackLinkReader, the
// factory). The answer is the ZITADEL sub the Slack user linked, never a login: the factory resolves
// that itself. A Slack user with no link is a 404 not_linked.
func (s *Server) slackLink(w http.ResponseWriter, r *http.Request) {
	p, ok := s.systemAuth(w, r)
	if !ok {
		return
	}
	if s.SlackLinks == nil || s.SlackLinkReader == "" || p.ID != s.SlackLinkReader {
		fail(w, http.StatusForbidden, wire.ReasonNotPermitted)
		return
	}
	release, ok := s.admit(w, p.ID)
	if !ok {
		return
	}
	defer release()
	team, user := r.URL.Query().Get("team"), r.URL.Query().Get("user")
	if !slackTeamRE.MatchString(team) || !slackUserRE.MatchString(user) {
		fail(w, http.StatusBadRequest, wire.ReasonBadSlackUser)
		return
	}
	sub, found, err := s.SlackLinks.SlackLink(r.Context(), team, user)
	switch {
	case err != nil:
		s.logFailure(w, err, "read a slack link")
	case !found:
		fail(w, http.StatusNotFound, wire.ReasonNotLinked)
	default:
		reply(w, http.StatusOK, map[string]string{"sub": sub})
	}
}
