// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"testing"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
)

// The group names are config (the Seams rule); these stand in for ZITADEL's.
const adminGroup, memberGroup = "agents-admin", "agents-member"

func human(role Role, approver, driver bool) Subject {
	return Subject{Kind: envelope.ActorHuman, ID: "human:x", Role: role, Approver: approver, Driver: driver, WebUI: true}
}

// SP2 §1's table. "with flag" = the approver flag; the driver column is a human
// who holds the token (their role is collaborator, the least that can hold it).
func TestTheSection1Matrix(t *testing.T) {
	subjects := map[string]Subject{
		"watcher":  human(Watcher, false, false),
		"collab":   human(Collaborator, false, false),
		"approver": human(Collaborator, true, false),
		"driver":   human(Collaborator, false, true),
		"owner":    human(Owner, false, false),
		"agent":    {Kind: envelope.ActorAgent, ID: "agent:7f3cq2xz"},
		"factory":  {Kind: envelope.ActorSystem, ID: "system:factory"},
		"policy":   {Kind: envelope.ActorSystem, ID: "system:policy"},
		// The factory while it holds the driver token, as it does in an unattended room.
		"sysDriver": {Kind: envelope.ActorSystem, ID: "system:factory", Driver: true},
		"stranger":  human(None, false, false),
	}
	rows := []struct {
		a    Action
		want map[string]bool
	}{
		{Read, map[string]bool{"watcher": true, "collab": true, "driver": true, "owner": true, "agent": true, "factory": true, "stranger": false}},
		{Chat, map[string]bool{"watcher": false, "collab": true, "driver": true, "owner": true, "agent": true, "factory": true}},
		{Queue, map[string]bool{"watcher": false, "collab": true, "driver": true, "owner": true, "agent": false, "factory": true}},
		{Steer, map[string]bool{"watcher": false, "collab": false, "driver": true, "owner": false, "agent": false, "factory": false, "sysDriver": true}},
		{Interrupt, map[string]bool{"collab": false, "driver": true, "owner": false, "agent": false, "factory": false, "sysDriver": true}},
		{StartRun, map[string]bool{"collab": false, "driver": true, "owner": true, "agent": false, "factory": true}},
		{Decide, map[string]bool{"collab": false, "approver": true, "driver": false, "owner": true, "agent": false, "factory": false, "policy": true}},
		// A system holder gives, and never requests or seizes the token: it yields to humans.
		{DriverRequest, map[string]bool{"watcher": false, "collab": true, "driver": false, "agent": false, "factory": false, "sysDriver": false}},
		{DriverGive, map[string]bool{"collab": false, "driver": true, "agent": false, "factory": false, "sysDriver": true}},
		{DriverTake, map[string]bool{"collab": false, "driver": false, "owner": true, "agent": false, "factory": false, "sysDriver": false}},
		{Fork, map[string]bool{"watcher": true, "collab": true, "driver": true, "owner": true, "agent": false, "factory": true, "stranger": false}},
		{Invite, map[string]bool{"collab": false, "driver": false, "owner": true, "agent": false, "factory": true}},
		{Close, map[string]bool{"collab": false, "owner": true, "factory": true}},
		{PromoteQueued, map[string]bool{"collab": false, "driver": true, "owner": false, "agent": false, "factory": false, "sysDriver": true}},
		{RemoveQueued, map[string]bool{"watcher": false, "collab": true, "agent": false}},
	}
	for _, r := range rows {
		t.Run(string(r.a), func(t *testing.T) {
			for who, want := range r.want {
				if got := Allowed(subjects[who], r.a); got != want {
					t.Errorf("%s by %s: got %v, want %v", r.a, who, got, want)
				}
			}
		})
	}
}

// Ruling P18: a CLI token never steers, interrupts, takes the driver or decides.
// Each subject would be allowed the refused action from the web UI, so only the
// WebUI gate refuses it.
func TestCLITokensNeverSteerOrDecide(t *testing.T) {
	cli := func(s Subject) Subject { s.WebUI = false; return s }
	ownerDriver := cli(human(Owner, true, true))     // the ui-only actions but DriverRequest
	collab := cli(human(Collaborator, false, false)) // DriverRequest, which needs a non-holder
	cases := []struct {
		who  string
		s    Subject
		a    Action
		want bool
	}{
		{"owner driver", ownerDriver, Steer, false}, {"owner driver", ownerDriver, Interrupt, false},
		{"owner driver", ownerDriver, Decide, false}, {"owner driver", ownerDriver, DriverGive, false},
		{"owner driver", ownerDriver, DriverTake, false}, {"owner driver", ownerDriver, PromoteQueued, false},
		{"collaborator", collab, DriverRequest, false},
		{"owner driver", ownerDriver, Read, true}, {"owner driver", ownerDriver, Chat, true},
		{"owner driver", ownerDriver, Queue, true}, {"owner driver", ownerDriver, Fork, true},
	}
	for _, c := range cases {
		t.Run(c.who+"/"+string(c.a), func(t *testing.T) {
			if web := c.s; !c.want {
				web.WebUI = true
				if !Allowed(web, c.a) {
					t.Fatalf("%s by %s is refused from the web UI too: the case does not isolate the gate", c.a, c.who)
				}
			}
			if got := Allowed(c.s, c.a); got != c.want {
				t.Errorf("%s by %s from a CLI token: got %v, want %v", c.a, c.who, got, c.want)
			}
		})
	}
}

func TestParseRole(t *testing.T) {
	cases := map[string]Role{"watcher": Watcher, "collaborator": Collaborator, "owner": Owner, "": None, "admin": None}
	for in, want := range cases {
		if got := ParseRole(in); got != want {
			t.Errorf("ParseRole(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestResolve(t *testing.T) {
	room := &v1alpha1.Room{Spec: v1alpha1.RoomSpec{Owner: "human:own",
		Members: []v1alpha1.Member{{Principal: "human:col", Role: "collaborator", Approver: true}}}}
	p := func(id string, groups ...string) authn.Principal {
		return authn.Principal{Kind: envelope.ActorHuman, ID: id, Groups: groups}
	}
	cases := []struct {
		name         string
		p            authn.Principal
		driver       string
		wantRole     Role
		wantApprover bool
		wantDriver   bool
	}{
		{"an agents-member watches everywhere", p("human:dev", memberGroup), "system:factory", Watcher, false, false},
		{"a member entry grants its role and flag", p("human:col", memberGroup), "human:col", Collaborator, true, true},
		{"spec.owner is owner", p("human:own", memberGroup), "system:factory", Owner, false, false},
		{"agents-admin is owner and approver everywhere", p("human:boss", adminGroup), "system:factory", Owner, true, false},
		{"a human in neither group and not in the spec has no role", p("human:stranger", "backend"), "system:factory", None, false, false},
		{"a non-human keeps only its driver standing",
			authn.Principal{Kind: envelope.ActorSystem, ID: "system:factory", Groups: []string{adminGroup}},
			"system:factory", None, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Groups{Admin: adminGroup, Member: memberGroup}.Resolve(room, c.p, c.driver, true)
			if s.Role != c.wantRole || s.Approver != c.wantApprover || s.Driver != c.wantDriver || !s.WebUI {
				t.Errorf("got %+v, want role %v approver %v driver %v", s, c.wantRole, c.wantApprover, c.wantDriver)
			}
		})
	}
}

func TestAdmitted(t *testing.T) {
	p := func(groups ...string) authn.Principal {
		return authn.Principal{Kind: envelope.ActorHuman, ID: "human:x", Groups: groups}
	}
	g := Groups{Admin: adminGroup, Member: memberGroup}
	cases := []struct {
		name   string
		groups Groups
		p      authn.Principal
		want   bool
	}{
		{"the member group is admitted", g, p(memberGroup), true},
		{"the admin group is admitted", g, p("backend", adminGroup), true},
		{"any other group is refused", g, p("backend"), false},
		{"no group is refused", g, p(), false},
		{"an unset group name never matches an empty claim", Groups{}, p(""), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.groups.Admitted(c.p); got != c.want {
				t.Errorf("Admitted(%v) = %v, want %v", c.p.Groups, got, c.want)
			}
		})
	}
}
