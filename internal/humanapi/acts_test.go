// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
	// onRoom moves the room after Room has taken its snapshot, as another
	// replica would between an act's read and its write.
	onRoom     func(*store.RoomState)
	appendErr  error
	queueErr   error
	pendingErr error
	recordErrs int // how many RecordRunRequest calls fail first
	// approvals by id; decisions records each Decide that went through.
	approvals   map[string]store.Approval
	approvalErr error
	decisions   []string
}

// Approval is the store's: one approval, or ErrNoApproval.
func (l *actLog) Approval(_ context.Context, id string) (store.Approval, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.approvalErr != nil {
		return store.Approval{}, l.approvalErr
	}
	a, ok := l.approvals[id]
	if !ok {
		return store.Approval{}, store.ErrNoApproval
	}
	return a, nil
}

// Decide is the store's: the first decision wins, a later one is ErrAlreadyDecided.
func (l *actLog) Decide(_ context.Context, id, decision, by, reason string, d envelope.Draft) (envelope.Event, store.Approval, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.approvals[id]
	switch {
	case !ok:
		return envelope.Event{}, a, store.ErrNoApproval
	case a.State != store.ApprovalPending:
		return envelope.Event{}, a, store.ErrAlreadyDecided
	}
	a.State = decision
	l.approvals[id] = a
	l.decisions = append(l.decisions, by+" "+decision+" "+reason)
	d.Type, d.RunID = envelope.ApprovalDecided, a.RunID
	d.Payload = envelope.Must(envelope.ApprovalDecidedPayload{ApprovalID: id, Decision: decision, Reason: reason})
	return l.appendLocked(d), a, nil
}

func (l *actLog) Room(context.Context, string) (store.RoomState, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	snapshot := l.st
	if l.onRoom != nil {
		l.onRoom(&l.st)
	}
	return snapshot, nil
}

func (l *actLog) appendLocked(d envelope.Draft) envelope.Event {
	l.drafts = append(l.drafts, d)
	return envelope.Event{Seq: int64(len(l.drafts)), RoomID: d.RoomID}
}

func (l *actLog) Append(_ context.Context, d envelope.Draft) (envelope.Event, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.appendErr != nil {
		return envelope.Event{}, false, l.appendErr
	}
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

func (l *actLog) ChangeDriver(_ context.Context, _ string, expect int64, to, reason string, d envelope.Draft) (envelope.Event, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if expect != l.st.DriverEpoch {
		return envelope.Event{}, false, store.ErrStaleEpoch
	}
	if to == l.st.Driver {
		return envelope.Event{}, false, store.ErrInvalidDriver
	}
	l.st.Driver, l.st.DriverEpoch = to, expect+1
	l.reasons = append(l.reasons, reason)
	d.Type = envelope.Driver
	d.Payload = envelope.Must(envelope.DriverPayload{To: to, Epoch: expect + 1, Reason: reason})
	return l.appendLocked(d), false, nil
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

// event is draft i as stored, seq i+1.
func event(i int, d envelope.Draft) envelope.Event {
	return envelope.Event{Seq: int64(i + 1), RoomID: d.RoomID, Actor: d.Actor, Type: d.Type, Payload: d.Payload}
}

// BriefSources is the store's: the latest agent handoff and the latest agent
// review_verdict, in seq order.
func (l *actLog) BriefSources(context.Context, string) ([]envelope.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var handoff, verdict *envelope.Event
	for i, d := range l.drafts {
		if d.Actor.Kind != envelope.ActorAgent {
			continue
		}
		ev := event(i, d)
		var m envelope.MessagePayload
		switch {
		case d.Type == envelope.Handoff:
			handoff = &ev
		case d.Type == envelope.Message && json.Unmarshal(d.Payload, &m) == nil && m.Kind == envelope.KindReviewVerdict:
			verdict = &ev
		}
	}
	var out []envelope.Event
	for _, ev := range []*envelope.Event{handoff, verdict} {
		if ev != nil {
			out = append(out, *ev)
		}
	}
	slices.SortFunc(out, func(a, b envelope.Event) int { return int(a.Seq - b.Seq) })
	return out, nil
}

// PendingRuns is the store's, without the TTL: run_requested records whose run
// no later participant event names.
func (l *actLog) PendingRuns(context.Context, string, time.Duration) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for i, d := range l.drafts {
		var r struct{ Kind, RunID string }
		if d.Type != envelope.StateChanged || json.Unmarshal(d.Payload, &r) != nil || r.Kind != "run_requested" {
			continue
		}
		joined := false
		for _, later := range l.drafts[i+1:] {
			var p envelope.ParticipantPayload
			joined = joined || (later.Type == envelope.Participant && json.Unmarshal(later.Payload, &p) == nil && p.Principal == "agent:"+r.RunID)
		}
		if !joined {
			out = append(out, r.RunID)
		}
	}
	return out, l.pendingErr
}

func (l *actLog) Queue(context.Context, string) ([]store.Queued, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []store.Queued
	for _, q := range l.queue {
		if q.State == "queued" {
			out = append(out, q)
		}
	}
	return out, l.queueErr
}

func (l *actLog) Stored(_ context.Context, d envelope.Draft) (envelope.Event, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, s := range l.drafts {
		if s.OriginClient == d.OriginClient && s.OriginSeq == d.OriginSeq {
			return envelope.Event{Seq: int64(i + 1), RoomID: s.RoomID, Type: s.Type, Payload: s.Payload}, true, nil
		}
	}
	return envelope.Event{}, false, nil
}

func (l *actLog) RecordRunRequest(_ context.Context, d envelope.Draft, runID string, refs []int64, fields map[string]any) (envelope.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.recordErrs > 0 {
		l.recordErrs--
		return envelope.Event{}, errors.New("conn reset")
	}
	consumed := []int64{}
	for _, ref := range refs {
		if q := l.item(ref); q != nil {
			q.State = "consumed"
			consumed = append(consumed, ref)
		}
	}
	f := map[string]any{"runId": runID, "consumed": consumed}
	for k, v := range fields {
		f[k] = v
	}
	d.Type, d.Payload = envelope.StateChanged, envelope.StatePayload("run_requested", f)
	return l.appendLocked(d), nil
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
	a.OnReject = func(_ context.Context, r string) { got = append(got, r) }
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

// Review 4.2 I1, §2: the token moved on another replica between the act's read and
// its write. The store's fence, not the snapshot, decides.
func TestTheStoreFencesARacedToken(t *testing.T) {
	a, log, room := fixture("human:own")
	log.onRoom = func(st *store.RoomState) { st.Driver, st.DriverEpoch = "human:col", 8 }
	e7 := int64(7)
	for i, action := range []Action{{Kind: "message", Text: "x", Delivery: "steering"}, {Kind: "interrupt"}} {
		log.st.Driver, log.st.DriverEpoch = "human:own", 7
		if f := act(a, room, "human:own", int64(i+1), &e7, action); f.Rejected != "stale_epoch" {
			t.Fatalf("%s after the token moved: %+v", action.Kind, f)
		}
	}
	if len(log.drafts) != 0 {
		t.Fatalf("a fenced action was appended: %+v", log.drafts)
	}
}

// §2: a give carries the epoch it was decided on.
func TestDriverGiveIsFenced(t *testing.T) {
	a, log, room := fixture("human:own")
	e6 := int64(6)
	for _, epoch := range []*int64{&e6, nil} {
		if f := act(a, room, "human:own", 1, epoch, Action{Kind: "driver_give", To: "human:col"}); f.Rejected != "stale_epoch" {
			t.Fatalf("epoch %v: %+v", epoch, f)
		}
	}
	if log.st.Driver != "human:own" || log.st.DriverEpoch != 7 {
		t.Fatalf("%+v", log.st)
	}
}

// Review 4.2 M4: the store's ErrKeyConflict, a clientSeq reused for another action.
func TestAReusedClientSeqIsABadAction(t *testing.T) {
	a, log, room := fixture("human:own")
	log.appendErr = fmt.Errorf("store: append to room 3kq7x2ma: %w", store.ErrKeyConflict)
	if f := act(a, room, "human:col", 1, nil, Action{Kind: "message", Text: "x", Delivery: "none"}); f.Rejected != "bad_action" {
		t.Fatalf("%+v", f)
	}
}

// rooms serves room from a fake API server and returns it as read back, with its
// resourceVersion, as the WebSocket reads it before each act.
func rooms(t *testing.T, a *Actor, room *v1alpha1.Room, funcs *interceptor.Funcs) *v1alpha1.Room {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(s).WithObjects(room)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	a.Rooms = b.Build()
	read := &v1alpha1.Room{}
	if err := a.Rooms.Get(context.Background(), client.ObjectKeyFromObject(room), read); err != nil {
		t.Fatal(err)
	}
	return read
}

func members(t *testing.T, a *Actor, room *v1alpha1.Room) []v1alpha1.Member {
	t.Helper()
	fresh := &v1alpha1.Room{}
	if err := a.Rooms.Get(context.Background(), client.ObjectKeyFromObject(room), fresh); err != nil {
		t.Fatal(err)
	}
	return fresh.Spec.Members
}

// Review 4.2 M2: nothing is written to a sealed room, the Room CR included.
func TestASealedRoomTakesNoAction(t *testing.T) {
	a, log, room := fixture("human:own")
	read := rooms(t, a, room, nil)
	log.st.Sealed = true
	if f := act(a, read, "human:own", 1, nil, Action{Kind: "invite", Principal: "human:new", MemberRole: "watcher"}); f.Rejected != "sealed" {
		t.Fatalf("invite: %+v", f)
	}
	if m := members(t, a, room); len(m) != 2 {
		t.Fatalf("the Room changed: %+v", m)
	}
	if f := act(a, read, "human:col", 1, nil, Action{Kind: "message", Text: "x", Delivery: "none"}); f.Rejected != "sealed" || len(log.drafts) != 0 {
		t.Fatalf("chat: %+v", f)
	}
}

// Review 4.2 M3: only a conflict asks for a retry.
func TestInviteUpdateErrors(t *testing.T) {
	gk := schema.GroupKind{Group: "agents.ogenki.io", Kind: "Room"}
	for _, c := range []struct {
		err  error
		want string
	}{
		{apierrors.NewConflict(schema.GroupResource{Group: gk.Group, Resource: "rooms"}, "3kq7x2ma", errors.New("changed")), "conflict"},
		{apierrors.NewInvalid(gk, "3kq7x2ma", field.ErrorList{field.TooMany(field.NewPath("spec", "members"), 21, 20)}), "bad_action"},
		{apierrors.NewForbidden(schema.GroupResource{Group: gk.Group, Resource: "rooms"}, "3kq7x2ma", errors.New("rbac")), "log_unavailable"},
	} {
		a, _, room := fixture("human:own")
		read := rooms(t, a, room, &interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			return c.err
		}})
		if f := act(a, read, "human:own", 1, nil, Action{Kind: "invite", Principal: "human:new", MemberRole: "watcher"}); f.Rejected != c.want {
			t.Fatalf("%v: %+v", c.err, f)
		}
	}
}

// Review 4.2 M5: the CRD's 20 members; a member's role still changes at the cap.
func TestInviteCapsMembers(t *testing.T) {
	a, _, room := fixture("human:own")
	for i := range 18 {
		room.Spec.Members = append(room.Spec.Members, v1alpha1.Member{Principal: fmt.Sprintf("human:m%02d", i), Role: "watcher"})
	}
	read := rooms(t, a, room, nil)
	if f := act(a, read, "human:own", 1, nil, Action{Kind: "invite", Principal: "human:new", MemberRole: "watcher"}); f.Rejected != "bad_action" {
		t.Fatalf("a 21st member: %+v", f)
	}
	if f := act(a, read, "human:own", 2, nil, Action{Kind: "invite", Principal: "human:m00", MemberRole: "collaborator"}); f.Rejected != "" {
		t.Fatalf("a role change at the cap: %+v", f)
	}
	if m := members(t, a, room); len(m) != 20 || m[19] != (v1alpha1.Member{Principal: "human:m00", Role: "collaborator"}) {
		t.Fatalf("%+v", m)
	}
}

// Review 4.2 M6: the token stays with a collaborator or better.
func TestInviteNeverDemotesTheDriver(t *testing.T) {
	a, _, room := fixture("human:col")
	read := rooms(t, a, room, nil)
	if f := act(a, read, "human:own", 1, nil, Action{Kind: "invite", Principal: "human:col", MemberRole: "watcher"}); f.Rejected != "bad_action" {
		t.Fatalf("the holder demoted: %+v", f)
	}
	if f := act(a, read, "human:own", 2, nil, Action{Kind: "invite", Principal: "human:two", MemberRole: "watcher"}); f.Rejected != "" {
		t.Fatalf("another member demoted: %+v", f)
	}
}

// pendingApproval is one of the room's approvals, prompted by human:own.
func pendingApproval(id, roomName string) store.Approval {
	return store.Approval{ID: id, RoomID: roomName, RunID: "7f3cq2xz", CallID: "c1", Class: "forge.pr", State: store.ApprovalPending,
		Action: []byte(`{}`), Prompters: []string{"human:own"}, RequestedAt: time.Now().Add(-time.Minute)}
}

// OD-16: with four-eyes on, whoever prompted the run cannot decide its approvals;
// another approver can.
func TestFourEyes(t *testing.T) {
	a, log, room := fixture("system:factory")
	room.Spec.Approvals.FourEyes = true
	room.Spec.Members = append(room.Spec.Members, v1alpha1.Member{Principal: "human:apr", Role: "collaborator", Approver: true})
	log.approvals = map[string]store.Approval{"ap1": pendingApproval("ap1", room.Name)}
	var waited []time.Duration
	a.OnDecided = func(d time.Duration) { waited = append(waited, d) }
	decide := Action{Kind: "decide", ApprovalID: "ap1", Decision: "approved"}
	if f := act(a, room, "human:own", 1, nil, decide); f.Rejected != "four_eyes" {
		t.Fatalf("the prompter decides: %+v", f)
	}
	if f := act(a, room, "human:apr", 1, nil, decide); f.Rejected != "" || f.Seq == 0 {
		t.Fatalf("another approver: %+v", f)
	}
	if len(waited) != 1 || waited[0] < time.Minute {
		t.Fatalf("decision waits %v", waited)
	}
	// Four-eyes off, the owner may decide what they prompted.
	room.Spec.Approvals.FourEyes = false
	log.approvals["ap2"] = pendingApproval("ap2", room.Name)
	if f := act(a, room, "human:own", 2, nil, Action{Kind: "decide", ApprovalID: "ap2", Decision: "denied"}); f.Rejected != "" {
		t.Fatalf("four-eyes off: %+v", f)
	}
}

// §6: the first valid decision wins; only approvers and owners decide, from the
// web UI, on this room's approvals, with approved or denied and a short reason,
// redacted.
func TestDecide(t *testing.T) {
	a, log, room := fixture("system:factory")
	room.Spec.Members = append(room.Spec.Members, v1alpha1.Member{Principal: "human:apr", Role: "collaborator", Approver: true})
	log.approvals = map[string]store.Approval{"ap1": pendingApproval("ap1", room.Name), "far": pendingApproval("far", "abcdefgh")}
	d := func(id, decision, reason string) Action {
		return Action{Kind: "decide", ApprovalID: id, Decision: decision, Reason: reason}
	}
	if f := act(a, room, "human:col", 1, nil, d("ap1", "approved", "")); f.Rejected != "not_permitted" {
		t.Fatalf("a collaborator without the flag: %+v", f)
	}
	for i, bad := range []Action{d("ap1", "expired", ""), d("ap1", "", ""), d("ap1", "Approved", ""), d("", "approved", ""),
		d("nope", "approved", ""), d("far", "approved", ""), d("ap1", "denied", strings.Repeat("x", 1025))} {
		if f := act(a, room, "human:apr", int64(10+i), nil, bad); f.Rejected != "bad_action" {
			t.Fatalf("%+v: %+v", bad, f)
		}
	}
	if f := act(a, room, "human:apr", 2, nil, d("ap1", "denied", "  leaks a SECRET  ")); f.Rejected != "" {
		t.Fatalf("deny: %+v", f)
	}
	last := log.last()
	if last.Type != envelope.ApprovalDecided || !slices.Equal(last.Redactions, []string{"test"}) ||
		!slices.Equal(log.decisions, []string{"human:apr denied leaks a [REDACTED:test]"}) {
		t.Fatalf("decision %+v %v", last, log.decisions)
	}
	if f := act(a, room, "human:own", 3, nil, d("ap1", "approved", "")); f.Rejected != "already_decided" {
		t.Fatalf("a second decision: %+v", f)
	}
	p := authn.Principal{Kind: envelope.ActorHuman, ID: "human:own", Groups: []string{"agents-member"}}
	raw, _ := json.Marshal(d("ap1", "approved", ""))
	if f := a.Handle(context.Background(), p, false, "s1", room, wire.ClientFrame{Type: "act", ClientSeq: 4, Action: raw}); f.Rejected != "not_permitted" {
		t.Fatalf("a CLI token decides: %+v", f)
	}
	log.approvalErr = errors.New("down")
	if f := act(a, room, "human:apr", 5, nil, d("ap1", "approved", "")); f.Rejected != "log_unavailable" {
		t.Fatalf("an unreadable approval: %+v", f)
	}
}
