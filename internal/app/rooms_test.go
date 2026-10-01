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

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/config"
	"github.com/Smana/agent-platform/internal/envelope"
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
	actor := humanActor(h, roomLog, red, fakeRuns{}, rooms, runrequest.Manifest{}, m)
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

func TestRunRequester(t *testing.T) {
	if _, ok := runRequester("").(runrequest.Manifest); !ok {
		t.Fatal("before SP3 the claim is rendered")
	}
	f, ok := runRequester("https://factory.example").(runrequest.Factory)
	if !ok || f.URL != "https://factory.example" || f.HC == nil {
		t.Fatalf("%+v", f)
	}
}

// fakeDrivers answers every driver move with err.
type fakeDrivers struct{ err error }

func (f fakeDrivers) ChangeDriver(context.Context, string, int64, string, string, envelope.Draft) (envelope.Event, error) {
	return envelope.Event{}, f.err
}

func (f fakeDrivers) ExpireDriver(context.Context, string, int64, string, envelope.Draft) (envelope.Event, error) {
	return envelope.Event{}, f.err
}

// Every token move counts, a human's or the sweeper's; a refused one does not.
func TestMeteredDriverChanges(t *testing.T) {
	exp, m := newMetrics(t)
	l := &meteredLog{drivers: fakeDrivers{}, m: m, now: time.Now}
	_, _ = l.ChangeDriver(t.Context(), "3kq7x2ma", 0, "human:a", "given", envelope.Draft{})
	_, _ = l.ExpireDriver(t.Context(), "3kq7x2ma", 1, "system:factory", envelope.Draft{})
	l.drivers = fakeDrivers{err: store.ErrStaleEpoch}
	_, _ = l.ChangeDriver(t.Context(), "3kq7x2ma", 0, "human:a", "given", envelope.Draft{})
	_, _ = l.ExpireDriver(t.Context(), "3kq7x2ma", 1, "system:factory", envelope.Draft{})
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

func (f *fakeLeaseLog) ExpireDriver(_ context.Context, roomID string, expect int64, to string, d envelope.Draft) (envelope.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expired = append(f.expired, fmt.Sprintf("%s %d %s: %s %s %s:%d", roomID, expect, to, d.Actor.ID, d.Origin, d.OriginClient, d.OriginSeq))
	return envelope.Event{}, f.errs[roomID]
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
