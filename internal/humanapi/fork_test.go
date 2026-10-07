// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/runrequest"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/wire"
)

// Fork is the store's, over the fake's one log: events 1..upTo exist when the
// log holds that many, and the fork's forked_from takes seq upTo+1 in dst.
func (l *actLog) Fork(_ context.Context, src string, upTo int64, r store.NewRoom, d envelope.Draft, fields map[string]any) (envelope.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.forkErr != nil {
		return envelope.Event{}, l.forkErr
	}
	if upTo < 1 || upTo > int64(len(l.drafts)) {
		return envelope.Event{}, store.ErrBadSeq
	}
	f := map[string]any{"room": src, "seq": upTo}
	for k, v := range fields {
		f[k] = v
	}
	d.RoomID, d.Type, d.Payload = r.ID, envelope.StateChanged, envelope.StatePayload("forked_from", f)
	ev := envelope.Event{Seq: upTo + 1, RoomID: r.ID, Actor: d.Actor, Type: d.Type, Payload: d.Payload, Redactions: d.Redactions}
	if l.forked == nil {
		l.forked = map[string]envelope.Event{}
	}
	l.forked[r.ID], l.forkedRoom = ev, r
	return ev, nil
}

// BriefSourcesThrough is BriefSources over the drafts at or before seq.
func (l *actLog) BriefSourcesThrough(ctx context.Context, roomID string, seq int64) ([]envelope.Event, error) {
	evs, err := l.BriefSources(ctx, roomID)
	var out []envelope.Event
	for _, ev := range evs {
		if ev.Seq <= seq {
			out = append(out, ev)
		}
	}
	return out, err
}

// Range serves a fork's forked_from, the one read a forked room's start_run makes.
func (l *actLog) Range(_ context.Context, roomID string, afterSeq int64, _ int) ([]envelope.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ev, ok := l.forked[roomID]; ok && ev.Seq > afterSeq {
		return []envelope.Event{ev}, nil
	}
	return nil, nil
}

// forkFixture is idleFixture with the Room on a fake API server and an agent's
// handoff at seq 1, then a chat at seq 2.
func forkFixture(t *testing.T, funcs *interceptor.Funcs) (*Actor, *actLog, *v1alpha1.Room, *fakeRequester) {
	t.Helper()
	a, log, room := idleFixture()
	room.Spec.Retention = "30d"
	room.Spec.Approvals = v1alpha1.Approvals{Profile: "unattended", Overrides: map[string]string{"forge.pr": "human"}}
	room = rooms(t, a, room, funcs)
	log.mu.Lock()
	log.appendLocked(envelope.Draft{RoomID: room.Name, Actor: envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:aaaaaaaa"},
		Type: envelope.Handoff, Payload: envelope.Must(envelope.HandoffPayload{FromRole: "implementer", ToRole: "reviewer",
			Summary: "done", Commit: "4be1c9d"})})
	log.mu.Unlock()
	if f := act(a, room, "human:col", 50, nil, Action{Kind: "message", Text: "try uv?", Delivery: "none"}); f.Rejected != "" {
		t.Fatal(f)
	}
	req := &fakeRequester{}
	a.Requester = requesterFunc(func(ctx context.Context, r runrequest.Request) (runrequest.Result, error) {
		_, _ = req.Request(ctx, r)
		return runrequest.Manifest{}.Request(ctx, r)
	})
	return a, log, room, req
}

// forkAs sends fork as who, an agents member with no standing of their own in
// the room (a watcher), through roomctl unless web.
func forkAs(a *Actor, room *v1alpha1.Room, who string, web bool, action Action) wire.ServerFrame {
	return forkIn(a, room, who, "agents-member", web, action)
}

// forkIn is forkAs for a member of group.
func forkIn(a *Actor, room *v1alpha1.Room, who, group string, web bool, action Action) wire.ServerFrame {
	action.Kind = "fork"
	raw, _ := json.Marshal(action)
	p := authn.Principal{Kind: envelope.ActorHuman, ID: who, Groups: []string{group}, AccessToken: "tok-" + who}
	return a.Handle(context.Background(), p, web, "s1", room, wire.ClientFrame{Type: "act", ClientSeq: 1, Action: raw})
}

// forkedSpec is the Room a successful fork created.
func forkedSpec(t *testing.T, a *Actor, room *v1alpha1.Room, f wire.ServerFrame) v1alpha1.RoomSpec {
	t.Helper()
	var res struct {
		RoomID string `json:"roomId"`
	}
	child := &v1alpha1.Room{}
	if err := json.Unmarshal(f.Result, &res); f.Rejected != "" || err != nil {
		t.Fatalf("%+v: %v", f, err)
	}
	if err := a.Rooms.Get(context.Background(), client.ObjectKey{Namespace: room.Namespace, Name: res.RoomID}, child); err != nil {
		t.Fatal(err)
	}
	return child.Spec
}

// SC-7, offline: a watcher forks from roomctl (ruling P18 allows it) into a room
// they own and drive, with the source's policy, and its first run starts on the
// new room's branch from the commit at the fork point, on their own token.
func TestForkCreatesTheForkersRoomAndItsRun(t *testing.T) {
	a, log, room, req := forkFixture(t, nil)
	f := forkAs(a, room, "human:bob", false, Action{Seq: 2, Note: "try uv, key SECRET", Role: "implementer", EgressProfiles: []string{"pypi"}})
	if f.Rejected != "" || f.Seq != 3 {
		t.Fatalf("%+v", f)
	}
	var res struct {
		RoomID   string          `json:"roomId"`
		Run      json.RawMessage `json:"run"`
		RunError *string         `json:"runError"`
	}
	if err := json.Unmarshal(f.Result, &res); err != nil || !envelope.ValidID(res.RoomID) || res.RoomID == room.Name ||
		res.RunError != nil || len(res.Run) == 0 || string(res.Run) == "null" {
		t.Fatalf("result %s: %v", f.Result, err)
	}
	child := &v1alpha1.Room{}
	if err := a.Rooms.Get(context.Background(), client.ObjectKey{Namespace: room.Namespace, Name: res.RoomID}, child); err != nil {
		t.Fatal(err)
	}
	s := child.Spec
	stricter := v1alpha1.Approvals{Profile: "attended", Overrides: map[string]string{"forge.other": "deny", "mcp.write": "deny"}}
	if s.Owner != "human:bob" || s.Driver != "human:bob" || len(s.Members) != 0 || s.DataClass != room.Spec.DataClass ||
		s.Repository != room.Spec.Repository || s.Retention != "30d" || !equality.Semantic.DeepEqual(s.Approvals, stricter) ||
		child.Annotations[ForkedFrom] != room.Name+"@2" {
		t.Fatalf("%+v %v", s, child.Annotations)
	}
	if r := log.forkedRoom; r.ID != res.RoomID || r.Driver != "human:bob" || r.Retention.Hours() != 30*24 {
		t.Fatalf("row %+v", r)
	}
	ev := log.forked[res.RoomID]
	var p map[string]any
	_ = json.Unmarshal(ev.Payload, &p)
	if p["kind"] != "forked_from" || p["room"] != room.Name || p["seq"] != 2.0 || p["commit"] != "4be1c9d" ||
		p["note"] != "try uv, key [REDACTED:test]" || len(ev.Redactions) != 1 || ev.Actor.ID != "human:bob" {
		t.Fatalf("forked_from %s %v", ev.Payload, ev.Redactions)
	}
	calls := req.calls()
	if len(calls) != 1 {
		t.Fatalf("%d run requests", len(calls))
	}
	r := calls[0]
	if r.RoomRef != res.RoomID || r.Branch != "agent/"+res.RoomID || r.BaseRef != "4be1c9d" || r.Principal != "human:bob" ||
		r.AccessToken != "tok-human:bob" || len(r.EgressProfiles) != 1 || r.EgressProfiles[0] != "pypi" ||
		!strings.Contains(r.TaskText, `"Forked-from: agent/`+room.Name+`@4be1c9d"`) {
		t.Fatalf("%+v", r)
	}
	if d := log.last(); d.RoomID != res.RoomID || !strings.Contains(string(d.Payload), "run_requested") {
		t.Fatalf("the run is recorded in the fork: %s %s", d.RoomID, d.Payload)
	}
}

// With no role, a fork is a room and nothing else. A sealed room forks only for
// its owner or an agents-admin (ruling M3): nothing is written there, but a
// sealed room's log is no longer everyone's to branch.
func TestOnlyAnOwnerForksASealedRoom(t *testing.T) {
	cases := []struct {
		name, who, group string
		want             string
	}{
		{"its owner", "human:own", "agents-member", ""},
		{"an agents-admin", "human:adm", "agents-admin", ""},
		{"a collaborator", "human:col", "agents-member", "sealed"},
		{"a watcher", "human:bob", "agents-member", "sealed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, log, room, req := forkFixture(t, nil)
			log.st.Sealed = true
			f := forkIn(a, room, c.who, c.group, true, Action{Seq: 1})
			if f.Rejected != c.want || len(req.calls()) != 0 {
				t.Fatalf("%+v %s", f, f.Result)
			}
			if c.want != "" {
				if len(log.forked) != 0 {
					t.Fatal("a refused fork creates nothing")
				}
				return
			}
			if string(f.Result) != `{"roomId":"`+log.forkedRoom.ID+`"}` {
				t.Fatalf("%s", f.Result)
			}
		})
	}
}

// A fork keeps its source's approvals only for the source's owners; anyone else
// gets, class by class, the stricter of the source's and a new room's default
// (ruling M2): an unattended source never hands a watcher a weaker gate.
func TestAForksApprovalsAreNeverWeakerThanANewRooms(t *testing.T) {
	stricter := v1alpha1.Approvals{Profile: "attended", Overrides: map[string]string{"forge.other": "deny", "mcp.write": "deny"},
		TTL: "2h", FourEyes: true}
	cases := []struct {
		name, who, group string
		source           bool // keeps the source's approvals as they are
	}{
		{"its owner", "human:own", "agents-member", true},
		{"an agents-admin", "human:adm", "agents-admin", true},
		{"a collaborator", "human:col", "agents-member", false},
		{"a watcher", "human:bob", "agents-member", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, _, room, _ := forkFixture(t, nil)
			room.Spec.Approvals.TTL, room.Spec.Approvals.FourEyes = "2h", true
			want := stricter
			if c.source {
				want = room.Spec.Approvals
			}
			if got := forkedSpec(t, a, room, forkIn(a, room, c.who, c.group, false, Action{Seq: 1})).Approvals; !equality.Semantic.DeepEqual(got, want) {
				t.Fatalf("got %+v want %+v", got, want)
			}
		})
	}
	t.Run("an allow override is dropped", func(t *testing.T) {
		a, _, room, _ := forkFixture(t, nil)
		room.Spec.Approvals = v1alpha1.Approvals{Profile: "attended", Overrides: map[string]string{"forge.pr": "allow"}}
		if got := forkedSpec(t, a, room, forkAs(a, room, "human:bob", false, Action{Seq: 1})).Approvals; !equality.Semantic.DeepEqual(got,
			v1alpha1.Approvals{Profile: "attended"}) {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestForkRefusals(t *testing.T) {
	created := 0
	countCreates := &interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		created++
		return c.Create(ctx, obj, opts...)
	}}
	cases := []struct {
		name   string
		who    string
		action Action
		want   string
	}{
		{"a seq past the end", "human:bob", Action{Seq: 9}, "bad_action"},
		{"seq 0", "human:bob", Action{}, "bad_action"},
		{"a role nobody starts", "human:bob", Action{Seq: 1, Role: "admin"}, "bad_action"},
		{"a PR for an implementer", "human:bob", Action{Seq: 1, Role: "implementer", PRURL: "https://github.com/Smana/cloud-native-ref/pull/1"}, "bad_action"},
		{"a malformed egress profile", "human:bob", Action{Seq: 1, Role: "implementer", EgressProfiles: []string{"Any"}}, "bad_action"},
		{"a note over 1 KiB", "human:bob", Action{Seq: 1, Note: strings.Repeat("n", 1025)}, "bad_action"},
		{"an owner the Room CRD refuses", "human:bob smith", Action{Seq: 1}, "bad_action"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			created = 0
			a, log, room, _ := forkFixture(t, countCreates)
			if f := forkAs(a, room, c.who, false, c.action); f.Rejected != c.want {
				t.Fatalf("%+v", f)
			}
			if created != 0 || len(log.forked) != 0 {
				t.Fatal("a refused fork creates nothing")
			}
		})
	}
	t.Run("nobody outside the room's readers", func(t *testing.T) {
		a, log, room, _ := forkFixture(t, nil)
		raw, _ := json.Marshal(Action{Kind: "fork", Seq: 1})
		f := a.Handle(context.Background(), authn.Principal{Kind: envelope.ActorHuman, ID: "human:out"}, true, "s1", room,
			wire.ClientFrame{Type: "act", ClientSeq: 1, Action: raw})
		if f.Rejected != "not_permitted" || len(log.forked) != 0 {
			t.Fatalf("%+v", f)
		}
	})
	t.Run("a prefix over the store's caps", func(t *testing.T) {
		a, log, room, _ := forkFixture(t, nil)
		log.forkErr = fmt.Errorf("store: fork: %w", store.ErrForkTooLarge)
		if f := forkAs(a, room, "human:bob", false, Action{Seq: 1}); f.Rejected != "too_large" {
			t.Fatalf("%+v", f)
		}
	})
	t.Run("the log is down", func(t *testing.T) {
		a, log, room, _ := forkFixture(t, nil)
		log.forkErr = errors.New("conn reset")
		if f := forkAs(a, room, "human:bob", false, Action{Seq: 1}); f.Rejected != "log_unavailable" {
			t.Fatalf("%+v", f)
		}
	})
}

// A fork whose Room cannot be created leaves no open log behind: a row with no
// Room is never closed, so retention would never purge it.
func TestAForkWhoseRoomFailsIsClosed(t *testing.T) {
	refuse := &interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
		return apierrors.NewServiceUnavailable("etcd")
	}}
	a, log, room, req := forkFixture(t, refuse)
	if f := forkAs(a, room, "human:bob", false, Action{Seq: 1, Role: "implementer"}); f.Rejected != "log_unavailable" {
		t.Fatalf("%+v", f)
	}
	if log.closed == "" || len(req.calls()) != 0 {
		t.Fatalf("closed %q, %d run requests", log.closed, len(req.calls()))
	}
	conflict := &interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
		return apierrors.NewAlreadyExists(schema.GroupResource{Group: "agents.ogenki.io", Resource: "rooms"}, "x")
	}}
	a, _, room, _ = forkFixture(t, conflict)
	if f := forkAs(a, room, "human:bob", false, Action{Seq: 1}); f.Rejected != "conflict" {
		t.Fatalf("%+v", f)
	}
}

// The fork is made even when its run is not: the ack says why, beside the room.
func TestAForksRunErrorRidesTheResult(t *testing.T) {
	a, _, room, _ := forkFixture(t, nil)
	a.Requester = requesterFunc(func(context.Context, runrequest.Request) (runrequest.Result, error) {
		return runrequest.Result{}, runrequest.ErrBudget
	})
	f := forkAs(a, room, "human:bob", false, Action{Seq: 1, Role: "implementer"})
	var res map[string]any
	if err := json.Unmarshal(f.Result, &res); f.Rejected != "" || err != nil || res["run"] != nil || res["runError"] != "over_budget" {
		t.Fatalf("%+v %s", f, f.Result)
	}
}

// Each fork copies a prefix in one transaction: forks have their own budget per
// principal, 3 at once then one a minute, apart from the 10 actions a second.
func TestForksAreRateLimitedPerPrincipal(t *testing.T) {
	a, log, room, _ := forkFixture(t, nil)
	for i := range 3 {
		if f := forkAs(a, room, "human:bob", false, Action{Seq: 1}); f.Rejected != "" {
			t.Fatalf("fork %d: %+v", i+1, f)
		}
	}
	if f := forkAs(a, room, "human:bob", false, Action{Seq: 1}); f.Rejected != "rate_limited" || len(log.forked) != 3 {
		t.Fatalf("a 4th fork within the minute: %+v, %d forks", f, len(log.forked))
	}
	if f := forkAs(a, room, "human:alice", true, Action{Seq: 1}); f.Rejected != "" {
		t.Fatalf("another principal's fork: %+v", f)
	}
	if f := act(a, room, "human:col", 51, nil, Action{Kind: "message", Text: "still here", Delivery: "none"}); f.Rejected != "" {
		t.Fatalf("other actions keep their own limit: %+v", f)
	}
}
