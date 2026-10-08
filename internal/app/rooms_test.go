// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/config"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/fanout"
	"github.com/Smana/agent-platform/internal/humanapi"
	"github.com/Smana/agent-platform/internal/metrics"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/redact"
	"github.com/Smana/agent-platform/internal/runrequest"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/wire"
)

// fakeActLog is the store under the wired actor: what a chat act calls. Any
// other method panics on the nil embedded interface.
type fakeActLog struct {
	humanapi.ActLog
	mu       sync.Mutex
	appended []envelope.Draft
}

func (*fakeActLog) Room(context.Context, string) (store.RoomState, error) {
	return store.RoomState{ID: "3kq7x2ma", Driver: "system:factory"}, nil
}

func (l *fakeActLog) Append(_ context.Context, d envelope.Draft) (envelope.Event, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appended = append(l.appended, d)
	return envelope.Event{Seq: int64(len(l.appended))}, false, nil
}

func (*fakeActLog) DriverSeen(context.Context, string, string, bool) error { return nil }

func (*fakeActLog) OpenApprovals(context.Context, string) ([]store.Approval, error) { return nil, nil }

// The rest of humanLog: the room list's reads and the lease sweep's.
func (*fakeActLog) Range(context.Context, string, int64, int) ([]envelope.Event, error) {
	return nil, nil
}

func (*fakeActLog) LapsedDrivers(context.Context) ([]store.RoomState, error) { return nil, nil }

func (*fakeActLog) ExpireDriver(context.Context, string, int64, string, envelope.Draft) (envelope.Event, bool, error) {
	return envelope.Event{}, false, nil
}

// serveBroker's whole human side in one call (review 4.4 M2): the server gets
// the actor with the broker's redactor, the leader gets the lease sweep, and a
// missing part or a refused Runnable stops start-up.
func TestHumanSide(t *testing.T) {
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	_, m := newMetrics(t)
	log := slog.New(slog.DiscardHandler)
	cfg := config.Config{Human: config.HumanConfig{Groups: config.GroupsConfig{Admin: "agents-admin", Member: "agents-member"}}}
	humans := authn.NewHumans(authn.NewVerifierWithKeyfunc(humanIssuer, nil), idFile(""), idFile(""), idFile(""), "https://rooms.example.test")
	rooms := fake.NewClientBuilder().Build()
	hub := fanout.New(fakeRoomLog{}, nil, log)
	var added []manager.Runnable
	add := func(r manager.Runnable) error { added = append(added, r); return nil }
	s, err := humanSide(cfg, humans, rooms, "agent-system", &fakeActLog{}, red, hub, fakeRuns{}, add, m, log)
	if err != nil {
		t.Fatal(err)
	}
	if s.Actor == nil || s.Actor.Redactor != red || s.Actor.Requester != (runrequest.Manifest{}) || s.Namespace != "agent-system" {
		t.Fatalf("server %+v, actor %+v", s, s.Actor)
	}
	if l, ok := added[0].(*leaderLoop); len(added) != 1 || !ok || l.every != leaseEvery {
		t.Fatalf("the lease sweep is not on the leader: %v", added)
	}
	if _, err := humanSide(cfg, humans, rooms, "agent-system", &fakeActLog{}, nil, hub, fakeRuns{}, add, m, log); err == nil {
		t.Fatal("no redactor must stop start-up")
	}
	refuse := func(manager.Runnable) error { return errors.New("manager started") }
	if _, err := humanSide(cfg, humans, rooms, "agent-system", &fakeActLog{}, red, hub, fakeRuns{}, refuse, m, log); err == nil {
		t.Fatal("a lease sweep the manager refused must stop start-up")
	}
}

func newMetrics(t *testing.T) (*metrics.Exporter, *metrics.Set) {
	t.Helper()
	exp, err := metrics.NewExporter("test")
	if err != nil {
		t.Fatal(err)
	}
	m, err := metrics.New(exp.Meter())
	if err != nil {
		t.Fatal(err)
	}
	return exp, m
}

// Until 4.4 the broker built no actor, so production acked every act
// not_permitted. The wired actor appends through the broker's redactor, with
// the config's groups, and counts its rejections.
func TestHumanActorIsWired(t *testing.T) {
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	exp, m := newMetrics(t)
	roomLog := &fakeActLog{}
	h := config.HumanConfig{Groups: config.GroupsConfig{Admin: "agents-admin", Member: "agents-member"}}
	rooms := fake.NewClientBuilder().Build()
	actor, err := humanActor(h, roomLog, red, fakeRuns{}, rooms, runrequest.Manifest{}, m)
	if err != nil {
		t.Fatal(err)
	}
	if actor.Groups != (policy.Groups{Admin: "agents-admin", Member: "agents-member"}) || actor.Rooms != rooms ||
		actor.Requester != (runrequest.Manifest{}) {
		t.Fatalf("%+v", actor)
	}
	room := &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: "3kq7x2ma", Namespace: "agent-system"},
		Spec: v1alpha1.RoomSpec{Owner: "human:own", Driver: "system:factory", DataClass: "public"}}
	token := "ghp_" + rand.Text()[:26] + "Ab12CdE34f"
	chat := func(who string, seq int64) wire.ServerFrame {
		raw, _ := json.Marshal(humanapi.Action{Kind: "message", Text: "key " + token, Delivery: "none"})
		p := authn.Principal{Kind: envelope.ActorHuman, ID: who, Groups: []string{"agents-member"}}
		return actor.Handle(t.Context(), p, true, "s1", room, wire.ClientFrame{Type: "act", ClientSeq: seq, Action: raw})
	}
	if f := chat("human:own", 1); f.Rejected != "" || f.Seq != 1 {
		t.Fatalf("the owner's chat: %+v", f)
	}
	if len(roomLog.appended) != 1 {
		t.Fatalf("%d appends", len(roomLog.appended))
	}
	if d := roomLog.appended[0]; strings.Contains(string(d.Payload), token) || !strings.Contains(string(d.Payload), "[REDACTED:github-pat]") ||
		d.Actor.ID != "human:own" {
		t.Fatalf("the stored draft: %s %+v", d.Payload, d.Actor)
	}
	if f := chat("human:watcher", 1); f.Rejected != "not_permitted" {
		t.Fatalf("a watcher's chat: %+v", f)
	}
	if body := scrape(t, exp.Handler()); !strings.Contains(body, `rooms_rejected_actions_total{reason="not_permitted"} 1`) {
		t.Fatalf("the rejection is not counted:\n%s", body)
	}
}

// Review 4.4 M2: a part the broker failed to pass stops start-up, rather than
// answering every act log_unavailable or not_permitted.
func TestHumanActorRefusesMissingParts(t *testing.T) {
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	_, m := newMetrics(t)
	h := config.HumanConfig{}
	rooms := fake.NewClientBuilder().Build()
	for name, build := range map[string]func() (*humanapi.Actor, error){
		"log": func() (*humanapi.Actor, error) {
			return humanActor(h, nil, red, fakeRuns{}, rooms, runrequest.Manifest{}, m)
		},
		"redactor": func() (*humanapi.Actor, error) {
			return humanActor(h, &fakeActLog{}, nil, fakeRuns{}, rooms, runrequest.Manifest{}, m)
		},
		"runs": func() (*humanapi.Actor, error) {
			return humanActor(h, &fakeActLog{}, red, nil, rooms, runrequest.Manifest{}, m)
		},
		"rooms": func() (*humanapi.Actor, error) {
			return humanActor(h, &fakeActLog{}, red, fakeRuns{}, nil, runrequest.Manifest{}, m)
		},
		"requester": func() (*humanapi.Actor, error) { return humanActor(h, &fakeActLog{}, red, fakeRuns{}, rooms, nil, m) },
		"metrics": func() (*humanapi.Actor, error) {
			return humanActor(h, &fakeActLog{}, red, fakeRuns{}, rooms, runrequest.Manifest{}, nil)
		},
	} {
		if a, err := build(); a != nil || err == nil || !strings.Contains(err.Error(), name+" is required") {
			t.Errorf("no %s: %v, %v", name, a, err)
		}
	}
}

func TestRunRequester(t *testing.T) {
	if _, ok := runRequester("").(runrequest.Manifest); !ok {
		t.Fatal("before SP3 the claim is rendered")
	}
	f, ok := runRequester("https://factory.example").(runrequest.Factory)
	if !ok || f.URL != "https://factory.example" || f.HC == nil {
		t.Fatalf("%+v", f)
	}
}

// fakeDrivers answers every driver move with dup and err.
type fakeDrivers struct {
	dup bool
	err error
}

func (f fakeDrivers) ChangeDriver(context.Context, string, int64, string, string, envelope.Draft) (envelope.Event, bool, error) {
	return envelope.Event{}, f.dup, f.err
}

func (f fakeDrivers) ExpireDriver(context.Context, string, int64, string, envelope.Draft) (envelope.Event, bool, error) {
	return envelope.Event{}, f.dup, f.err
}

// Every token move counts, a human's or the sweeper's; a refused one does not,
// nor a replay (review 4.4 M5: two overlapping leaders build the same lease key).
func TestMeteredDriverChanges(t *testing.T) {
	exp, m := newMetrics(t)
	l := &meteredLog{drivers: fakeDrivers{}, m: m, now: time.Now}
	both := func() {
		_, _, _ = l.ChangeDriver(t.Context(), "3kq7x2ma", 0, "human:a", "given", envelope.Draft{})
		_, _, _ = l.ExpireDriver(t.Context(), "3kq7x2ma", 1, "system:factory", envelope.Draft{})
	}
	both()
	l.drivers = fakeDrivers{err: store.ErrStaleEpoch}
	both()
	l.drivers = fakeDrivers{dup: true}
	both()
	if body := scrape(t, exp.Handler()); !strings.Contains(body, "rooms_driver_changes_total 2\n") {
		t.Fatalf("want 2 changes:\n%s", body)
	}
}

// fakeLeaseLog lists lapsed and records each expiry; errs answers it per room.
type fakeLeaseLog struct {
	mu      sync.Mutex
	lapsed  []store.RoomState
	listErr error
	errs    map[string]error
	expired []string // room expect to: key
}

func (f *fakeLeaseLog) LapsedDrivers(context.Context) ([]store.RoomState, error) {
	return f.lapsed, f.listErr
}

func (f *fakeLeaseLog) ExpireDriver(_ context.Context, roomID string, expect int64, to string, d envelope.Draft) (envelope.Event, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expired = append(f.expired, fmt.Sprintf("%s %d %s: %s %s %s:%d", roomID, expect, to, d.Actor.ID, d.Origin, d.OriginClient, d.OriginSeq))
	return envelope.Event{}, false, f.errs[roomID]
}

func (f *fakeLeaseLog) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.expired...)
}

// §2: a lapsed human's token goes back to the room's system holder, keyed on its
// epoch, while this replica leads (ruling SY).
func TestSweepLeases(t *testing.T) {
	lapsed := []store.RoomState{{ID: "aaaaaaaa", DriverEpoch: 3, FallbackDriver: "system:factory"},
		{ID: "bbbbbbbb", DriverEpoch: 7, FallbackDriver: "system:reviewer"}}
	both := []string{
		"aaaaaaaa 3 system:factory: system:room-broker broker broker:lease:aaaaaaaa:4",
		"bbbbbbbb 7 system:reviewer: system:room-broker broker broker:lease:bbbbbbbb:8",
	}
	quiet := slog.New(slog.DiscardHandler)
	for _, c := range []struct {
		name    string
		l       *fakeLeaseLog
		leading func() bool
		want    []string
	}{
		{"every lapsed holder's lease expires", &fakeLeaseLog{lapsed: lapsed}, func() bool { return true }, both},
		{"one refusal does not stop the sweep", &fakeLeaseLog{lapsed: lapsed, errs: map[string]error{"aaaaaaaa": store.ErrNotLapsed}},
			func() bool { return true }, both},
		{"nor does one failure", &fakeLeaseLog{lapsed: lapsed, errs: map[string]error{"aaaaaaaa": errors.New("conn reset")}},
			func() bool { return true }, both},
		{"an unreadable list expires nothing", &fakeLeaseLog{lapsed: lapsed, listErr: errors.New("down")}, func() bool { return true }, nil},
		{"a replica that stops leading stops", &fakeLeaseLog{lapsed: lapsed}, onlyOnce(), both[:1]},
	} {
		t.Run(c.name, func(t *testing.T) {
			sweepLeases(t.Context(), c.l, c.leading, quiet)
			if got := c.l.got(); strings.Join(got, "\n") != strings.Join(c.want, "\n") {
				t.Fatalf("expired\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(c.want, "\n"))
			}
		})
	}
}

// onlyOnce leads for one check, then no longer.
func onlyOnce() func() bool {
	n := 0
	return func() bool { n++; return n == 1 }
}

// The sweep runs on the leader only, every 30 s, marked leading while it runs.
func TestLeaseLoop(t *testing.T) {
	l := &fakeLeaseLog{lapsed: []store.RoomState{{ID: "aaaaaaaa", DriverEpoch: 1, FallbackDriver: "system:factory"}}}
	tick := make(chan time.Time)
	loop := leaseLoop(l, slog.New(slog.DiscardHandler), func(d time.Duration) (<-chan time.Time, func()) {
		if d != 30*time.Second {
			t.Errorf("period %s", d)
		}
		return tick, func() {}
	})
	if !loop.NeedLeaderElection() {
		t.Fatal("only the leader sweeps")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- loop.Start(ctx) }()
	tick <- time.Now()
	tick <- time.Now() // the first tick's sweep has finished once the loop takes this one
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := l.got(); len(got) < 1 || got[0] != "aaaaaaaa 1 system:factory: system:room-broker broker broker:lease:aaaaaaaa:2" {
		t.Fatalf("%v", got)
	}
}
