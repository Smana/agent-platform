// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/ghidentity"
)

// brokerLinks is the broker's link records as the factory reads them: team/user -> sub.
type brokerLinks map[string]string

func (b brokerLinks) SlackLink(_ context.Context, team, user string) (string, bool, error) {
	if team == "T0DOWN" {
		return "", false, errors.New("broker down")
	}
	sub, ok := b[team+"/"+user]
	return sub, ok, nil
}

// zitadelLogins is each sub's current GitHub login; "gone" is a deactivated user, "flaky" fails.
type zitadelLogins map[string]string

func (z zitadelLogins) Login(_ context.Context, sub, _ string) (string, error) {
	switch sub {
	case "gone":
		return "", ghidentity.ErrInactive
	case "flaky":
		return "", errors.New("zitadel down")
	}
	return z[sub], nil
}

// selfServe is the request for U07NEW, linked on the broker only.
var selfServe = strings.Replace(draft, "U02DEF", "U07NEW", 1)

// A Slack user the config does not list files through the broker's link record, resolved to the
// sub's current GitHub login, and only for a principal that opted in; the maintainer check is
// unchanged, and the config wins over a link.
func TestCreateTaskSelfServeLink(t *testing.T) {
	links := brokerLinks{"T01ABC/U07NEW": "zsub-smana", "T01ABC/U08OUT": "zsub-out", "T01ABC/U09NOGH": "zsub-nogh",
		"T01ABC/U05GONE": "gone", "T01ABC/U06FLAKY": "flaky", "T01ABC/U02DEF": "zsub-out"}
	logins := zitadelLogins{"zsub-smana": "Smana", "zsub-out": "outsider"}
	optIn := func(c *config.Config) { c.API.Sponsored[0].OnBehalfOf.SlackLinks = true }
	for name, c := range map[string]struct {
		body   string
		edit   func(*config.Config)
		status int
		reason string
	}{
		"not opted in":           {selfServe, nil, 403, "not_linked"},
		"no link":                {strings.Replace(draft, "U02DEF", "U01NONE", 1), optIn, 403, "not_linked"},
		"linked, not maintainer": {strings.Replace(draft, "U02DEF", "U08OUT", 1), optIn, 403, "not_a_maintainer"},
		"no GitHub link left":    {strings.Replace(draft, "U02DEF", "U09NOGH", 1), optIn, 403, "not_linked"},
		"a deactivated account":  {strings.Replace(draft, "U02DEF", "U05GONE", 1), optIn, 403, "not_linked"},
		"ZITADEL down":           {strings.Replace(draft, "U02DEF", "U06FLAKY", 1), optIn, 503, "link_unverified"},
		"the broker down":        {strings.Replace(selfServe, "T01ABC", "T0DOWN", 1), optIn, 503, "link_unverified"},
	} {
		t.Run(name, func(t *testing.T) {
			s, f := newTaskServer(t, slack, 0)
			s.SlackLinks, s.Logins = links, logins
			if c.edit != nil {
				c.edit(s.Cfg)
			}
			rec := postTask(s, c.body, "k")
			if rec.Code != c.status || !strings.Contains(rec.Body.String(), `"`+c.reason+`"`) {
				t.Fatalf("%d %s, want %d %s", rec.Code, rec.Body, c.status, c.reason)
			}
			if len(f.got) != 0 || len(tasks(t, s)) != 0 {
				t.Fatal("a refusal filed or created something")
			}
		})
	}
	t.Run("linked maintainer", func(t *testing.T) {
		s, f := newTaskServer(t, slack, 0)
		s.SlackLinks, s.Logins = links, logins
		optIn(s.Cfg)
		rec := postTask(s, selfServe, "k")
		var out struct{ Sponsor string }
		if rec.Code != 201 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Sponsor != "smana" {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if got := tasks(t, s)[0].Spec.Source; got.Sponsor != "github:smana" || got.OnBehalfOf != "slack:T01ABC/U07NEW" || len(f.got) != 1 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("config wins", func(t *testing.T) { // U02DEF is smana in the config, outsider on the broker
		s, _ := newTaskServer(t, slack, 0)
		s.SlackLinks, s.Logins = links, logins
		optIn(s.Cfg)
		if rec := postTask(s, draft, "k"); rec.Code != 201 || !strings.Contains(rec.Body.String(), `"sponsor":"smana"`) {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})
}
