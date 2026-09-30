// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/wire"
)

// The store is the production ActLog.
var _ ActLog = (*store.Store)(nil)

// actLog is an in-memory ActLog with the same fencing rules as the store.
type actLog struct {
	mu      sync.Mutex
	st      store.RoomState
	drafts  []envelope.Draft
	queue   []store.Queued
	reasons []string // ChangeDriver's reasons
	seen    map[string]int
	closed  string
}

func (l *actLog) Room(context.Context, string) (store.RoomState, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.st, nil
}

func (l *actLog) appendLocked(d envelope.Draft) envelope.Event {
	l.drafts = append(l.drafts, d)
	return envelope.Event{Seq: int64(len(l.drafts)), RoomID: d.RoomID}
}

func (l *actLog) Append(_ context.Context, d envelope.Draft) (envelope.Event, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appendLocked(d), false, nil
}

func (l *actLog) AppendAsDriver(_ context.Context, driver string, epoch int64, d envelope.Draft) (envelope.Event, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if driver != l.st.Driver || epoch != l.st.DriverEpoch {
		return envelope.Event{}, false, store.ErrStaleEpoch
	}
	return l.appendLocked(d), false, nil
}

func (l *actLog) Enqueue(_ context.Context, d envelope.Draft, author, text string) (envelope.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ev := l.appendLocked(d)
	l.queue = append(l.queue, store.Queued{Ref: ev.Seq, Author: author, Text: text, State: "queued"})
	return ev, nil
}

func (l *actLog) item(ref int64) *store.Queued {
	for i := range l.queue {
		if l.queue[i].Ref == ref && l.queue[i].State == "queued" {
			return &l.queue[i]
		}
	}
	return nil
}

func (l *actLog) PromoteQueued(_ context.Context, _ string, ref int64, driver string, epoch int64, runID string, d envelope.Draft) (envelope.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if driver != l.st.Driver || epoch != l.st.DriverEpoch {
		return envelope.Event{}, store.ErrStaleEpoch
	}
	q := l.item(ref)
	if q == nil {
		return envelope.Event{}, store.ErrNotQueued
	}
	q.State = "promoted"
	d.Type, d.CausedBy = envelope.Message, &ref
	d.Payload = envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: q.Text, To: []string{"agent:" + runID},
		Delivery: envelope.DeliverySteering})
	return l.appendLocked(d), nil
}

func (l *actLog) RemoveQueued(_ context.Context, _ string, ref int64, d envelope.Draft) (envelope.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	q := l.item(ref)
	if q == nil {
		return envelope.Event{}, store.ErrNotQueued
	}
	if q.Author != d.Actor.ID && l.st.Driver != d.Actor.ID {
		return envelope.Event{}, store.ErrNotAuthor
	}
	q.State = "removed"
	d.Type, d.Payload = envelope.StateChanged, envelope.StatePayload("queued_removed", map[string]any{"ref": ref})
	return l.appendLocked(d), nil
}

func (l *actLog) ChangeDriver(_ context.Context, _ string, expect int64, to, reason string, d envelope.Draft) (envelope.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if expect != l.st.DriverEpoch {
		return envelope.Event{}, store.ErrStaleEpoch
	}
	if to == l.st.Driver {
		return envelope.Event{}, store.ErrInvalidDriver
	}
	l.st.Driver, l.st.DriverEpoch = to, expect+1
	l.reasons = append(l.reasons, reason)
	d.Type = envelope.Driver
	d.Payload = envelope.Must(envelope.DriverPayload{To: to, Epoch: expect + 1, Reason: reason})
	return l.appendLocked(d), nil
}

func (l *actLog) DriverSeen(_ context.Context, _, principal string, acted bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string]int{}
	}
	if acted {
		principal += "+acted"
	}
	l.seen[principal]++
	return nil
}

func (l *actLog) CloseRoom(_ context.Context, _, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = reason
	return nil
}

func (l *actLog) last() envelope.Draft {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.drafts[len(l.drafts)-1]
}

// testRedactor redacts the word SECRET, anywhere in a payload.
type testRedactor struct{}

func (testRedactor) Payload(_ context.Context, raw json.RawMessage) (json.RawMessage, []string, error) {
	if !bytes.Contains(raw, []byte("SECRET")) {
		return raw, nil, nil
	}
	return bytes.ReplaceAll(raw, []byte("SECRET"), []byte("[REDACTED:test]")), []string{"test"}, nil
}

func fixture(driver string) (*Actor, *actLog, *v1alpha1.Room) {
	room := &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: "3kq7x2ma", Namespace: "agent-system"},
		Spec: v1alpha1.RoomSpec{Owner: "human:own", Driver: "system:factory", DataClass: "public", Repository: "Smana/cloud-native-ref",
			Members: []v1alpha1.Member{{Principal: "human:col", Role: "collaborator"}, {Principal: "human:two", Role: "collaborator"}}}}
	w := runwatch.New()
	w.Upsert(context.Background(), &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "xplane-run-7f3cq2xz", "namespace": "agents"},
		"spec":     map[string]any{"roomRef": "3kq7x2ma", "role": "implementer"},
		"status":   map[string]any{"phase": "Running"}}})
	log := &actLog{st: store.RoomState{ID: "3kq7x2ma", Driver: driver, DriverEpoch: 7, FallbackDriver: "system:factory"}}
	return &Actor{Log: log, Runs: w, Groups: groups, Redactor: testRedactor{}}, log, room
}

func act(a *Actor, room *v1alpha1.Room, who string, seq int64, epoch *int64, action Action) wire.ServerFrame {
	raw, _ := json.Marshal(action)
	p := authn.Principal{Kind: envelope.ActorHuman, ID: who, Groups: []string{"agents-member"}}
	return a.Handle(context.Background(), p, true, "s1", room, wire.ClientFrame{Type: "act", ClientSeq: seq, Action: raw, DriverEpoch: epoch})
}

// SC-3: only the driver steers; after driver_give it is accepted; driver{epoch n+1}.
func TestOnlyTheDriverSteers(t *testing.T) {
	a, log, room := fixture("human:own")
	e7 := int64(7)
	steer := Action{Kind: "message", Text: "use the v2 API", Delivery: "steering"}
	if f := act(a, room, "human:col", 1, &e7, steer); f.Rejected != "not_permitted" {
		t.Fatalf("collaborator steering: %+v", f)
	}
	if f := act(a, room, "human:own", 1, &e7, Action{Kind: "driver_give", To: "human:col"}); f.Rejected != "" {
		t.Fatalf("give: %+v", f)
	}
	if log.st.Driver != "human:col" || log.st.DriverEpoch != 8 {
		t.Fatalf("driver{epoch n+1}: %+v", log.st)
	}
	if f := act(a, room, "human:col", 2, &e7, steer); f.Rejected != "stale_epoch" {
		t.Fatalf("an old epoch is fenced: %+v", f)
	}
	e8 := int64(8)
	if f := act(a, room, "human:col", 3, &e8, steer); f.Rejected != "" || f.Seq == 0 {
		t.Fatalf("the new driver steers: %+v", f)
	}
	var p envelope.MessagePayload
	_ = json.Unmarshal(log.drafts[len(log.drafts)-1].Payload, &p)
	if p.Delivery != envelope.DeliverySteering || len(p.To) != 1 || p.To[0] != "agent:7f3cq2xz" {
		t.Fatalf("steering is addressed to the running run: %+v", p)
	}
}

func TestASystemHolderYieldsAtOnce(t *testing.T) {
	a, log, room := fixture("system:factory")
	if f := act(a, room, "human:col", 1, nil, Action{Kind: "driver_request"}); f.Rejected != "" || log.st.Driver != "human:col" {
		t.Fatalf("%+v %+v", f, log.st)
	}
}

// A human holder does not yield: the request is recorded for them to answer.
func TestAHumanHolderIsAsked(t *testing.T) {
	a, log, room := fixture("human:own")
	if f := act(a, room, "human:col", 1, nil, Action{Kind: "driver_request"}); f.Rejected != "" || log.st.Driver != "human:own" {
		t.Fatalf("%+v %+v", f, log.st)
	}
	if d := log.last(); d.Type != envelope.StateChanged || string(d.Payload) != `{"by":"human:col","holder":"human:own","kind":"driver_request"}` {
		t.Fatalf("%s %s", d.Type, d.Payload)
	}
}

func TestQueueAndPromote(t *testing.T) {
	a, log, room := fixture("human:own")
	if f := act(a, room, "human:col", 1, nil, Action{Kind: "message", Text: "address L42", Delivery: "queued"}); f.Rejected != "" {
		t.Fatalf("queue: %+v", f)
	}
	e7 := int64(7)
	if f := act(a, room, "human:col", 2, &e7, Action{Kind: "promote_queued", Ref: 1}); f.Rejected != "not_permitted" {
		t.Fatalf("only the driver promotes: %+v", f)
	}
	if f := act(a, room, "human:own", 1, &e7, Action{Kind: "promote_queued", Ref: 1}); f.Rejected != "" || log.queue[0].State != "promoted" {
		t.Fatalf("promote: %+v %+v", f, log.queue)
	}
}

// Ruling P18.
func TestCLISessionsNeverSteer(t *testing.T) {
	a, _, room := fixture("human:own")
	raw, _ := json.Marshal(Action{Kind: "interrupt"})
	e7 := int64(7)
	f := a.Handle(context.Background(), authn.Principal{Kind: envelope.ActorHuman, ID: "human:own", Groups: []string{"agents-member"}},
		false, "s1", room, wire.ClientFrame{Type: "act", ClientSeq: 1, Action: raw, DriverEpoch: &e7})
	if f.Rejected != "not_permitted" {
		t.Fatalf("%+v", f)
	}
}

// §4: every human text is redacted before it is stored, the queue row included.
func TestActsRedactBeforeAppend(t *testing.T) {
	a, log, room := fixture("human:own")
	if f := act(a, room, "human:col", 1, nil, Action{Kind: "message", Text: "token SECRET", Delivery: "none"}); f.Rejected != "" {
		t.Fatalf("%+v", f)
	}
	if d := log.last(); !strings.Contains(string(d.Payload), "[REDACTED:test]") || strings.Contains(string(d.Payload), "SECRET") ||
		len(d.Redactions) != 1 {
		t.Fatalf("chat %s %v", d.Payload, d.Redactions)
	}
	if f := act(a, room, "human:col", 2, nil, Action{Kind: "message", Text: "later SECRET", Delivery: "queued"}); f.Rejected != "" {
		t.Fatalf("%+v", f)
	}
	if q := log.queue[0]; q.Text != "later [REDACTED:test]" || q.Author != "human:col" {
		t.Fatalf("queue row %+v", q)
	}
	a.Redactor = nil
	if f := act(a, room, "human:col", 3, nil, Action{Kind: "message", Text: "x", Delivery: "none"}); f.Rejected != "log_unavailable" {
		t.Fatalf("nothing is stored unredacted: %+v", f)
	}
}

func TestSteeringNeedsARunningRun(t *testing.T) {
	a, _, room := fixture("human:own")
	a.Runs = runwatch.New()
	e7 := int64(7)
	for _, action := range []Action{{Kind: "message", Text: "now", Delivery: "steering"}, {Kind: "interrupt"},
		{Kind: "promote_queued", Ref: 1}} {
		if f := act(a, room, "human:own", 1, &e7, action); f.Rejected != "no_running_run" {
			t.Fatalf("%s: %+v", action.Kind, f)
		}
	}
}

func TestInterruptNamesTheRunningRun(t *testing.T) {
	a, log, room := fixture("human:own")
	e7 := int64(7)
	if f := act(a, room, "human:own", 1, &e7, Action{Kind: "interrupt"}); f.Rejected != "" {
		t.Fatalf("%+v", f)
	}
	if d := log.last(); string(d.Payload) != `{"kind":"interrupt","runId":"7f3cq2xz"}` {
		t.Fatalf("%s", d.Payload)
	}
	if log.seen["human:own+acted"] != 1 {
		t.Fatalf("the driver's action is its lease: %v", log.seen)
	}
}

// The token goes to a collaborator or better, or back to the room's system holder.
func TestDriverGiveTargets(t *testing.T) {
	e7 := int64(7)
	for _, c := range []struct{ to, want string }{
		{"human:stranger", "bad_action"}, {"system:elsewhere", "bad_action"}, {"agent:7f3cq2xz", "bad_action"},
		{"human:own", "bad_action"}, // the holder already
		{"system:factory", ""}, {"human:two", ""},
	} {
		a, _, room := fixture("human:own")
		if f := act(a, room, "human:own", 1, &e7, Action{Kind: "driver_give", To: c.to}); f.Rejected != c.want {
			t.Fatalf("give to %s: %+v", c.to, f)
		}
	}
}

func TestDriverTakeNeedsAReason(t *testing.T) {
	a, log, room := fixture("human:col")
	for _, why := range []string{"", "   ", strings.Repeat("x", maxReason+1)} {
		if f := act(a, room, "human:own", 1, nil, Action{Kind: "driver_take", Reason: why}); f.Rejected != "bad_action" {
			t.Fatalf("reason %q: %+v", why, f)
		}
	}
	if f := act(a, room, "human:col", 1, nil, Action{Kind: "driver_take", Reason: "mine"}); f.Rejected != "not_permitted" {
		t.Fatalf("a collaborator takes: %+v", f)
	}
	if f := act(a, room, "human:own", 2, nil, Action{Kind: "driver_take", Reason: "stuck on SECRET"}); f.Rejected != "" {
		t.Fatalf("%+v", f)
	}
	if log.st.Driver != "human:own" || log.reasons[0] != "taken: stuck on [REDACTED:test]" || len(log.last().Redactions) != 1 {
		t.Fatalf("%+v %v %v", log.st, log.reasons, log.last().Redactions)
	}
}

// §2: the author or the driver removes a queued message.
func TestRemoveQueued(t *testing.T) {
	a, _, room := fixture("human:own")
	if f := act(a, room, "human:col", 1, nil, Action{Kind: "message", Text: "maybe", Delivery: "queued"}); f.Rejected != "" {
		t.Fatalf("%+v", f)
	}
	if f := act(a, room, "human:two", 1, nil, Action{Kind: "remove_queued", Ref: 1}); f.Rejected != "not_permitted" {
		t.Fatalf("another collaborator: %+v", f)
	}
	if f := act(a, room, "human:col", 2, nil, Action{Kind: "remove_queued", Ref: 1}); f.Rejected != "" {
		t.Fatalf("the author: %+v", f)
	}
	if f := act(a, room, "human:own", 1, nil, Action{Kind: "remove_queued", Ref: 1}); f.Rejected != "not_queued" {
		t.Fatalf("already removed: %+v", f)
	}
}

func TestMalformedActs(t *testing.T) {
	a, _, room := fixture("human:own")
	var got []string
	a.OnReject = func(r string) { got = append(got, r) }
	for _, c := range []struct {
		name   string
		seq    int64
		action Action
	}{
		{"no clientSeq", 0, Action{Kind: "message", Text: "x", Delivery: "none"}},
		{"an unknown kind", 1, Action{Kind: "teleport"}},
		{"an unknown delivery", 2, Action{Kind: "message", Text: "x", Delivery: "shouted"}},
		{"an empty message", 3, Action{Kind: "message", Text: " ", Delivery: "none"}},
		{"an oversize message", 4, Action{Kind: "message", Text: strings.Repeat("x", envelope.MaxHumanMessage+1), Delivery: "none"}},
	} {
		if f := act(a, room, "human:col", c.seq, nil, c.action); f.Rejected != "bad_action" {
			t.Fatalf("%s: %+v", c.name, f)
		}
	}
	if len(got) != 5 || got[0] != "bad_action" {
		t.Fatalf("rejections counted: %v", got)
	}
}

func TestActsAreRateLimited(t *testing.T) {
	a, _, room := fixture("human:own")
	limited := 0
	for i := range int64(25) {
		if f := act(a, room, "human:col", i+1, nil, Action{Kind: "message", Text: "x", Delivery: "none"}); f.Rejected == "rate_limited" {
			limited++
		}
	}
	if limited == 0 || limited > 5 {
		t.Fatalf("burst 20: %d of 25 limited", limited)
	}
}

func TestInviteAndClose(t *testing.T) {
	a, log, room := fixture("human:own")
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	a.Rooms = fake.NewClientBuilder().WithScheme(s).WithObjects(room).Build()
	invite := Action{Kind: "invite", Principal: "human:new", MemberRole: "collaborator", Approver: true}
	if f := act(a, room, "human:col", 1, nil, invite); f.Rejected != "not_permitted" {
		t.Fatalf("only the owner invites: %+v", f)
	}
	for _, bad := range []Action{{Kind: "invite", Principal: "system:x", MemberRole: "watcher"},
		{Kind: "invite", Principal: "human:new", MemberRole: "admin"}} {
		if f := act(a, room, "human:own", 1, nil, bad); f.Rejected != "bad_action" {
			t.Fatalf("%+v: %+v", bad, f)
		}
	}
	read := &v1alpha1.Room{} // as the WebSocket reads it before each act
	if err := a.Rooms.Get(context.Background(), client.ObjectKeyFromObject(room), read); err != nil {
		t.Fatal(err)
	}
	if f := act(a, read, "human:own", 2, nil, invite); f.Rejected != "" {
		t.Fatalf("%+v", f)
	}
	fresh := &v1alpha1.Room{}
	if err := a.Rooms.Get(context.Background(), client.ObjectKeyFromObject(room), fresh); err != nil {
		t.Fatal(err)
	}
	if m := fresh.Spec.Members; len(m) != 3 || m[2] != (v1alpha1.Member{Principal: "human:new", Role: "collaborator", Approver: true}) {
		t.Fatalf("members %+v", m)
	}
	if d := log.last(); d.Type != envelope.Participant {
		t.Fatalf("%+v", d)
	}
	if f := act(a, read, "human:own", 3, nil, invite); f.Rejected != "conflict" {
		t.Fatalf("an invite on a stale Room: %+v", f)
	}
	if f := act(a, room, "human:own", 4, nil, Action{Kind: "close"}); f.Rejected != "" || log.closed != "closed by human:own" {
		t.Fatalf("%+v %q", f, log.closed)
	}
}
