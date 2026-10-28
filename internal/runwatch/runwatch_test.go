// SPDX-License-Identifier: Apache-2.0

package runwatch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllertest"

	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/store"
)

func claim(name, room, phase, revoked string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cloud.ogenki.io/v1alpha1", "kind": "AgentRun",
		"metadata": map[string]any{"name": name, "namespace": "agents"},
		"spec": map[string]any{"role": "reviewer", "principal": "human:312345678901234567", "dataClass": "public",
			"repository": "Smana/cloud-native-ref", "roomRef": room,
			"budget": map[string]any{"maxMinutes": int64(60)}, "egress": map[string]any{"profiles": []any{"pypi"}}},
		"status": map[string]any{"phase": phase, "branch": "agent/" + room, "startedAt": "2026-09-27T10:00:00Z"},
	}}
	if revoked != "" {
		u.SetAnnotations(map[string]string{"agents.ogenki.io/revoked": revoked})
	}
	return u
}

// suspendable is a suspendable run its broker suspended and resumed once.
func suspendable() *unstructured.Unstructured {
	u := claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Suspended", "")
	_ = unstructured.SetNestedField(u.Object, true, "spec", "suspendable")
	u.SetAnnotations(map[string]string{OperatingModeAnnotation: "Suspended",
		RemainingSecondsAnnotation: "900", ResumedAtAnnotation: "2026-09-27T11:00:00Z"})
	u.SetResourceVersion("42")
	return u
}

// transitioned gives the claim conditions, Ready's at transition, and no finishedAt.
func transitioned(u *unstructured.Unstructured, transition string) *unstructured.Unstructured {
	_ = unstructured.SetNestedSlice(u.Object, []any{
		map[string]any{"type": "Synced", "status": "True", "lastTransitionTime": "2026-09-27T10:00:05Z"},
		map[string]any{"type": "Ready", "status": "True", "lastTransitionTime": transition},
	}, "status", "conditions")
	return u
}

func TestFromUnstructured(t *testing.T) {
	r, ok := FromUnstructured(claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Running", ""))
	if !ok || r.ID != "7f3cq2xz" || r.Room != "3kq7x2ma" || r.Role != "reviewer" || r.MaxMinutes != 60 ||
		r.Branch != "agent/3kq7x2ma" || len(r.EgressProfiles) != 1 || !r.Live() ||
		!r.StartedAt.Equal(time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v %v", r, ok)
	}

	for _, c := range []struct {
		name string
		u    func() *unstructured.Unstructured
	}{
		{"a claim not named xplane-run-<id> is not a run", func() *unstructured.Unstructured { return claim("not-a-run", "", "", "") }},
		{"a run id that is not a C2 id is not a run", func() *unstructured.Unstructured { return claim("xplane-run-NOTANID1", "", "", "") }},
		{"a claim outside the agents namespace is not a run", func() *unstructured.Unstructured {
			u := claim("xplane-run-7f3cq2xz", "", "", "")
			u.SetNamespace("default")
			return u
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := FromUnstructured(c.u()); ok {
				t.Fatal("accepted")
			}
		})
	}

	defaults := claim("xplane-run-7f3cq2xz", "", "", "")
	unstructured.RemoveNestedField(defaults.Object, "spec", "budget")
	unstructured.RemoveNestedField(defaults.Object, "status", "branch")
	_ = unstructured.SetNestedField(defaults.Object, "agent/fork", "spec", "branch")
	finished := transitioned(claim("xplane-run-7f3cq2xz", "", "Failed", ""), "2026-09-27T10:20:00Z")
	_ = unstructured.SetNestedField(finished.Object, "2026-09-27T10:05:00Z", "status", "finishedAt")
	for _, c := range []struct {
		name string
		u    *unstructured.Unstructured
		ok   func(Run) bool
	}{
		{"a revoked run is never live", claim("xplane-run-7f3cq2xz", "", "Running", "manual"), func(r Run) bool { return !r.Live() }},
		{"a terminal run is not live", claim("xplane-run-7f3cq2xz", "", "Succeeded", ""), func(r Run) bool { return !r.Live() }},
		{"a claim with no phase yet is Pending and live", defaults, func(r Run) bool { return r.Phase == "Pending" && r.Live() }},
		{"a claim with no budget gets the default deadline", defaults, func(r Run) bool { return r.MaxMinutes == 120 }},
		// v2 suspend: a suspended run keeps its room, so it is neither terminal nor dead.
		{"a suspended run is live", claim("xplane-run-7f3cq2xz", "", "Suspended", ""), func(r Run) bool {
			return r.Live() && !Terminal(r.Phase)
		}},
		{"a run is not suspendable unless its spec says so", defaults, func(r Run) bool {
			return !r.Suspendable && r.OperatingMode == "" && r.RemainingSeconds == "" && r.ResumedAt.IsZero()
		}},
		{"the suspend fields and the resource version are read", suspendable(), func(r Run) bool {
			return r.Suspendable && r.OperatingMode == "Suspended" && r.RemainingSeconds == "900" &&
				r.ResumedAt.Equal(time.Date(2026, 9, 27, 11, 0, 0, 0, time.UTC)) && r.ResourceVersion == "42"
		}},
		{"the branch falls back to spec.branch before status has one", defaults, func(r Run) bool { return r.Branch == "agent/fork" }},
		{
			"with no finishedAt the end is the Ready transition",
			transitioned(claim("xplane-run-7f3cq2xz", "", "Failed", ""), "2026-09-27T10:20:00Z"),
			func(r Run) bool { return r.FinishedAt.Equal(time.Date(2026, 9, 27, 10, 20, 0, 0, time.UTC)) },
		},
		{
			"finishedAt wins over the conditions", finished,
			func(r Run) bool { return r.FinishedAt.Equal(time.Date(2026, 9, 27, 10, 5, 0, 0, time.UTC)) },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if r, ok := FromUnstructured(c.u); !ok || !c.ok(r) {
				t.Fatalf("%+v %v", r, ok)
			}
		})
	}
}

// A provider hiccup that flips Synced long after the pod was lost must not move
// the end time past the deadline: only Ready's transition stands in for finishedAt.
func TestALateSyncedTransitionIsNotTheEnd(t *testing.T) {
	u := claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Failed", "")
	_ = unstructured.SetNestedSlice(u.Object, []any{
		map[string]any{"type": "Ready", "status": "False", "lastTransitionTime": "2026-09-27T10:20:00Z"},
		map[string]any{"type": "Synced", "status": "False", "lastTransitionTime": "2026-09-27T13:00:00Z"},
	}, "status", "conditions")
	r, ok := FromUnstructured(u)
	if !ok || !r.FinishedAt.Equal(time.Date(2026, 9, 27, 10, 20, 0, 0, time.UTC)) {
		t.Fatalf("finished at %s, want Ready's 10:20", r.FinishedAt)
	}
	if got := EndReason(r, "", "", false); got != "pod_lost" {
		t.Fatalf("end reason %s, want pod_lost", got)
	}
}

func TestWatcherFiresGoneOnceForTerminalRevokedOrDeleted(t *testing.T) {
	ctx := t.Context()
	w := New()
	var gone, changed []string
	w.OnGone(func(_ context.Context, r Run) { gone = append(gone, r.ID) })
	w.OnChange(func(_ context.Context, r Run) { changed = append(changed, r.ID+":"+r.Phase) })
	w.Upsert(ctx, claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Running", ""))
	w.Upsert(ctx, claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Succeeded", ""))
	w.Upsert(ctx, claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Succeeded", ""))
	w.Upsert(ctx, claim("xplane-run-aaaaaaaa", "3kq7x2ma", "Running", ""))
	w.Remove(ctx, claim("xplane-run-aaaaaaaa", "3kq7x2ma", "Running", ""))
	w.Upsert(ctx, claim("xplane-run-bbbbbbbb", "zzzzzzzz", "Running", "manual"))
	w.Upsert(ctx, claim("not-a-run", "3kq7x2ma", "Running", ""))
	if want := []string{"7f3cq2xz", "aaaaaaaa", "bbbbbbbb"}; !slices.Equal(gone, want) {
		t.Fatalf("gone = %v, want %v", gone, want)
	}
	if len(changed) != 5 {
		t.Fatalf("OnChange fires on every upsert of a run: %v", changed)
	}
	if _, live, _ := w.Live("7f3cq2xz"); live {
		t.Fatal("terminal run reported live")
	}
	if r, ok := w.Get("7f3cq2xz"); !ok || r.Phase != "Succeeded" {
		t.Fatalf("Get keeps a terminal run: %+v %v", r, ok)
	}
	if len(w.InRoom("3kq7x2ma")) != 1 {
		t.Fatal("InRoom keeps terminal runs, drops deleted ones")
	}
	if len(w.All()) != 2 {
		t.Fatalf("All = %v", w.All())
	}
}

// Important 1: cutting a dead run's connections (OnGone) never waits behind the
// log's I/O (OnChange).
func TestGoneFiresBeforeChange(t *testing.T) {
	ctx := t.Context()
	w := New()
	var order []string
	w.OnChange(func(context.Context, Run) { order = append(order, "change") })
	w.OnGone(func(context.Context, Run) { order = append(order, "gone") })
	w.Upsert(ctx, claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Failed", ""))
	if !slices.Equal(order, []string{"gone", "change"}) {
		t.Fatalf("order = %v", order)
	}
}

// OnRemove reports what the watch knew, not the (possibly stale) deleted object.
func TestOnRemoveGetsTheLastKnownState(t *testing.T) {
	ctx := t.Context()
	w := New()
	var removed []Run
	w.OnRemove(func(_ context.Context, r Run) { removed = append(removed, r) })
	w.Upsert(ctx, claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Running", ""))
	w.Remove(ctx, claim("xplane-run-7f3cq2xz", "zzzzzzzz", "Pending", ""))
	if len(removed) != 1 || removed[0].Room != "3kq7x2ma" || removed[0].Phase != "Running" {
		t.Fatalf("removed = %+v", removed)
	}
}

func TestEndReason(t *testing.T) {
	start := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	run := func(phase, revoked string, end time.Time) Run {
		return Run{Phase: phase, Revoked: revoked, StartedAt: start, FinishedAt: end, MaxMinutes: 60}
	}
	early, late := start.Add(20*time.Minute), start.Add(60*time.Minute)
	deleted := run("Revoked", "", early)
	deleted.Deleted = true
	for _, c := range []struct {
		name    string
		r       Run
		status  string
		want    string
		refused bool // the broker refused the run's bridge (F15)
	}{
		{"the agent finished", run("Succeeded", "", early), "finished", "agent_finished", false},
		{"the agent reported an error", run("Failed", "", early), "error", "agent_error", false},
		{"the agent got stuck", run("Failed", "", early), "stuck", "agent_stuck", false},
		{"a still-running agent past its budget hit the deadline", run("Failed", "", late), "running", "deadline", false},
		{"within the toleration of the deadline is the deadline", run("Failed", "", late.Add(-deadlineToleration)), "running", "deadline", false},
		{"a still-running agent before its deadline lost its pod", run("Failed", "", early), "running", "pod_lost", false},
		{"no harness status at all is a lost pod", run("Failed", "", early), "", "pod_lost", false},
		{"a run with no finish time is not past its deadline", run("Failed", "", time.Time{}), "", "pod_lost", false},
		{"a succeeded run without a harness status finished", run("Succeeded", "", early), "", "agent_finished", false},
		{"an exhausted budget names the budget", run("BudgetExhausted", "budget-run", early), "running", "budget-run", false},
		{"an exhausted budget takes its reason from the annotation", run("BudgetExhausted", "budget-principal", early), "running", "budget-principal", false},
		{"an exhausted budget without an annotation is the run's budget", run("BudgetExhausted", "", early), "running", "budget-run", false},
		{"a revoked run was revoked", run("Revoked", "manual", early), "running", "revoked", false},
		{"a deleted claim was deleted", deleted, "running", "deleted", false},
		{"a refused run, admitted later, that succeeded finished", run("Succeeded", "", early), "", "agent_finished", true},
		{"a refused run that failed was refused", run("Failed", "", early), "", "room_busy", true},
		{"a run refused, then admitted, ends on what it did", run("Failed", "", early), "running", "pod_lost", true},
		{"a refused run that the owner revoked was revoked", run("Revoked", "manual", early), "", "revoked", true},
		{"a refused run whose claim was deleted was deleted", deleted, "", "deleted", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := EndReason(c.r, c.status, "", c.refused); got != c.want {
				t.Fatalf("got %s want %s", got, c.want)
			}
		})
	}
}

type fakeStore struct {
	drafts    []envelope.Draft
	status    string
	end       string
	keys      map[string]bool
	appendErr error
	statusErr error
	busy      map[string]bool // origin clients that hold an event
	cursorErr error
}

func (f *fakeStore) Append(_ context.Context, d envelope.Draft) (envelope.Event, bool, error) {
	if f.appendErr != nil {
		return envelope.Event{}, false, f.appendErr
	}
	if err := d.Validate(); err != nil {
		return envelope.Event{}, false, err
	}
	k := fmt.Sprintf("%s/%d", d.OriginClient, d.OriginSeq)
	if f.keys[k] {
		return envelope.Event{}, true, nil
	}
	f.keys[k] = true
	f.drafts = append(f.drafts, d)
	return envelope.Event{Seq: int64(len(f.drafts))}, false, nil
}

func (f *fakeStore) LastHarnessStatus(context.Context, string, string) (string, error) {
	return f.status, f.statusErr
}

func (f *fakeStore) LastHarnessEnd(context.Context, string, string) (string, error) {
	return f.end, nil
}

func (f *fakeStore) Cursor(_ context.Context, _, originClient string) (int64, error) {
	if f.busy[originClient] {
		return 1, f.cursorErr
	}
	return 0, f.cursorErr
}

func payloads(ds []envelope.Draft) []string {
	var out []string
	for _, d := range ds {
		out = append(out, string(d.Type)+":"+string(d.Payload))
	}
	return out
}

func TestObserveAppendsEachStepOnce(t *testing.T) {
	ctx := t.Context()
	fs := &fakeStore{keys: map[string]bool{}, status: "error"}
	e := &Events{Store: fs}
	r, _ := FromUnstructured(claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Running", ""))
	for range 2 { // the second is a replayed informer event, or a new leader
		if err := e.Observe(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	r.Phase, r.FinishedAt = "Failed", r.StartedAt.Add(5*time.Minute)
	if err := e.Observe(ctx, r); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`participant:{"principal":"agent:7f3cq2xz","change":"joined","role":"reviewer"}`,
		`state_changed:{"kind":"run_phase","phase":"Running"}`,
		`state_changed:{"kind":"run_phase","phase":"Failed","reason":"agent_error"}`,
		`participant:{"principal":"agent:7f3cq2xz","change":"left","role":"reviewer"}`,
	}
	if got := payloads(fs.drafts); !slices.Equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	for i, d := range fs.drafts {
		if d.OriginClient != "broker:run:7f3cq2xz" || d.OriginSeq != int64(i+1) || d.Origin != envelope.OriginBroker ||
			d.Actor != (envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"}) || d.RunID != "7f3cq2xz" {
			t.Fatalf("draft %d: %+v", i, d)
		}
	}
}

func TestObserve(t *testing.T) {
	running, _ := FromUnstructured(claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Running", ""))
	pending := running
	pending.Phase = "Pending"
	// Failed with no finishedAt: the end is the claim's last condition transition,
	// whenever the broker observes it (a restart after downtime changes nothing).
	failedAt := func(transition string) Run {
		r, _ := FromUnstructured(transitioned(claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Failed", ""), transition))
		return r
	}
	noRoom, badRoom := running, running
	noRoom.Room, badRoom.Room = "", "NOT-A-C2-ID"
	boom := errors.New("boom")
	for _, c := range []struct {
		name      string
		r         Run
		appendErr error
		statusErr error
		busy      bool // the broker refused the run's bridge (F15)
		cursorErr error
		wantN     int
		wantErr   error
		wantLast  string
	}{
		{name: "a pending run has only joined", r: pending, wantN: 1},
		{name: "a run naming no room appends nothing", r: noRoom},
		{name: "a run naming a malformed room appends nothing", r: badRoom},
		{name: "a run naming a missing room appends nothing", r: running, appendErr: fmt.Errorf("store: %w", store.ErrNoRoom)},
		{name: "a run naming a sealed room appends nothing", r: running, appendErr: fmt.Errorf("store: %w", store.ErrSealed)},
		{name: "a store failure is returned wrapped", r: running, appendErr: boom, wantErr: boom},
		{name: "a harness status failure is returned wrapped", r: failedAt("2026-09-27T10:01:00Z"), statusErr: boom, wantErr: boom, wantN: 2},
		{
			name: "a run that ended past its budget hit the deadline", r: failedAt("2026-09-27T12:00:00Z"), wantN: 4,
			wantLast: `{"kind":"run_phase","phase":"Failed","reason":"deadline"}`,
		},
		{
			name: "a run that ended within its budget lost its pod, however late it is observed", r: failedAt("2026-09-27T10:01:00Z"), wantN: 4,
			wantLast: `{"kind":"run_phase","phase":"Failed","reason":"pod_lost"}`,
		},
		{
			name: "a run the room refused ended room_busy", r: failedAt("2026-09-27T10:01:00Z"), busy: true, wantN: 4,
			wantLast: `{"kind":"run_phase","phase":"Failed","reason":"room_busy"}`,
		},
		{name: "a refusal lookup failure is returned wrapped", r: failedAt("2026-09-27T10:01:00Z"), cursorErr: boom, wantErr: boom, wantN: 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs := &fakeStore{keys: map[string]bool{}, appendErr: c.appendErr, statusErr: c.statusErr,
				busy: map[string]bool{"broker:busy:7f3cq2xz": c.busy}, cursorErr: c.cursorErr}
			e := &Events{Store: fs}
			err := e.Observe(t.Context(), c.r)
			if c.wantErr == nil && err != nil || c.wantErr != nil && !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			if len(fs.drafts) != c.wantN {
				t.Fatalf("drafts = %v", payloads(fs.drafts))
			}
			if c.wantLast != "" && string(fs.drafts[2].Payload) != c.wantLast {
				t.Fatalf("end event = %s", fs.drafts[2].Payload)
			}
		})
	}
}

// Review M15: a claim deleted mid-run still ends in the log, once.
func TestADeletedRunEndsOnce(t *testing.T) {
	ctx := t.Context()
	w := New()
	var removed []string
	w.OnRemove(func(_ context.Context, r Run) { removed = append(removed, r.ID) })
	w.Upsert(ctx, claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Running", ""))
	w.Remove(ctx, claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Running", ""))
	w.Remove(ctx, claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Running", "")) // unknown by now
	if len(removed) != 1 {
		t.Fatalf("removed = %v", removed)
	}
	fs := &fakeStore{keys: map[string]bool{}}
	e := &Events{Store: fs}
	r, _ := FromUnstructured(claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Running", ""))
	for range 2 {
		if err := e.ObserveDeleted(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if len(fs.drafts) != 4 || string(fs.drafts[2].Payload) != `{"kind":"run_phase","phase":"Revoked","reason":"deleted"}` {
		t.Fatalf("drafts = %v", payloads(fs.drafts))
	}

	ended, _ := FromUnstructured(claim("xplane-run-aaaaaaaa", "3kq7x2ma", "Succeeded", ""))
	if err := e.ObserveDeleted(ctx, ended); err != nil || len(fs.drafts) != 4 {
		t.Fatalf("a run that had already ended appends nothing on deletion: %v %v", err, payloads(fs.drafts))
	}
}

func TestRegisterFeedsTheWatcherFromTheInformer(t *testing.T) {
	ctx := t.Context()
	// An informer not synced yet: the watch is not populated until its handler is.
	inf := controllertest.NewFakeInformer()
	informers := &informertest.FakeInformers{InformersByGVK: map[schema.GroupVersionKind]toolscache.SharedIndexInformer{GVK(): inf}}
	w := New()
	synced, err := Register(ctx, informers, w)
	if err != nil {
		t.Fatal(err)
	}
	if synced() {
		t.Fatal("the handler reports synced before the informer's first list")
	}
	inf.Synced()
	if !synced() {
		t.Fatal("the handler never reports synced")
	}
	var gone []string
	w.OnGone(func(_ context.Context, r Run) { gone = append(gone, r.ID) })

	running := claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Running", "")
	inf.Add(running)
	if _, live, _ := w.Live("7f3cq2xz"); !live {
		t.Fatal("an added run is not live")
	}
	inf.Update(running, claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Revoked", "manual"))
	if r, _ := w.Get("7f3cq2xz"); r.Phase != "Revoked" {
		t.Fatalf("an update is not applied: %+v", r)
	}
	inf.Delete(running)
	if _, ok := w.Get("7f3cq2xz"); ok {
		t.Fatal("a deleted run is still watched")
	}
	if !slices.Equal(gone, []string{"7f3cq2xz"}) {
		t.Fatalf("gone = %v", gone)
	}

	if _, err := Register(ctx, &informertest.FakeInformers{Error: errBoom}, New()); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
}

var errBoom = errors.New("boom")

// A missed delete reaches the handler as a tombstone around the last known object.
func TestHandlerUnwrapsATombstone(t *testing.T) {
	ctx := t.Context()
	w := New()
	h := handler(ctx, w)
	h.OnAdd(claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Running", ""), false)
	h.OnAdd("not an object", false)
	h.OnDelete(toolscache.DeletedFinalStateUnknown{Key: "agents/xplane-run-7f3cq2xz",
		Obj: claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Running", "")})
	if len(w.All()) != 0 {
		t.Fatalf("the tombstone's run is still watched: %v", w.All())
	}
}

func TestReadsTheTaskURL(t *testing.T) {
	r, ok := FromUnstructured(&unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "xplane-run-7f3cq2xz", "namespace": "agents"},
		"spec": map[string]any{"role": "reviewer", "repository": "Smana/cloud-native-ref",
			"task": map[string]any{"url": "https://github.com/Smana/cloud-native-ref/pull/12"}},
	}})
	if !ok || r.TaskURL != "https://github.com/Smana/cloud-native-ref/pull/12" || r.Repository != "Smana/cloud-native-ref" {
		t.Fatalf("%+v", r)
	}
}

func TestLiveTellsUnknownFromDead(t *testing.T) {
	w := New()
	synced := false
	w.SetSynced(func() bool { return synced })
	if _, live, known := w.Live("nosuchrn"); live || known {
		t.Fatalf("before sync, an absent run: live %v known %v", live, known)
	}
	w.Upsert(t.Context(), claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Failed", ""))
	if _, live, known := w.Live("7f3cq2xz"); live || !known {
		t.Fatalf("terminal run: live %v known %v", live, known)
	}
	synced = true
	if _, live, known := w.Live("nosuchrn"); live || !known {
		t.Fatalf("after sync, an absent run: live %v known %v", live, known)
	}
}

// rooms_participants{kind="run"}: each run with a roomRef counts once while live,
// whatever the updates and resyncs, and leaves once, whatever ends it.
func TestOnLiveCountsEachLiveRunOnce(t *testing.T) {
	ctx := t.Context()
	w := New()
	var n int64
	w.OnLive(func(_ context.Context, d int64) {
		if n += d; n < 0 {
			t.Fatalf("count went negative: %d", n)
		}
	})
	check := func(step string, want int64) {
		t.Helper()
		if n != want {
			t.Fatalf("%s: count = %d, want %d", step, n, want)
		}
	}
	const a = "xplane-run-7f3cq2xz"
	w.Upsert(ctx, claim(a, "3kq7x2ma", "Pending", ""))
	check("live", 1)
	w.Upsert(ctx, claim(a, "3kq7x2ma", "Running", ""))
	w.Upsert(ctx, claim(a, "3kq7x2ma", "Running", "")) // resync
	check("still live", 1)
	w.Upsert(ctx, claim(a, "3kq7x2ma", "Succeeded", ""))
	check("terminal", 0)
	w.Upsert(ctx, claim(a, "3kq7x2ma", "Succeeded", "")) // resync
	w.Remove(ctx, claim(a, "3kq7x2ma", "Succeeded", ""))
	check("deleted after terminal", 0)

	w.Upsert(ctx, claim("xplane-run-aaaaaaaa", "3kq7x2ma", "Running", ""))
	w.Upsert(ctx, claim("xplane-run-bbbbbbbb", "3kq7x2ma", "Running", ""))
	check("two live", 2)
	w.Remove(ctx, claim("xplane-run-aaaaaaaa", "3kq7x2ma", "Running", ""))
	check("deleted while live", 1)
	w.Upsert(ctx, claim("xplane-run-bbbbbbbb", "3kq7x2ma", "Running", "manual"))
	check("revoked", 0)
	w.Upsert(ctx, claim("xplane-run-cccccccc", "", "Running", ""))
	w.Upsert(ctx, claim("xplane-run-dddddddd", "3kq7x2ma", "Failed", ""))
	check("no room, or born terminal", 0)
}

// Loop policy part 2: a recorded loop end names a Failed run's end before its harness status
// (a stalled conversation is paused); the platform's own ends, and any other phase, keep theirs.
func TestEndReasonReadsTheLoopEndFirst(t *testing.T) {
	start := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	run := func(phase, revoked string) Run {
		return Run{Phase: phase, Revoked: revoked, StartedAt: start, FinishedAt: start.Add(20 * time.Minute), MaxMinutes: 60}
	}
	for _, c := range []struct {
		name        string
		r           Run
		status, end string
		want        string
	}{
		{"a stall ends a paused conversation", run("Failed", ""), "paused", "stalled", "stalled"},
		{"the judge said impossible", run("Failed", ""), "finished", "judge_impossible", "judge_impossible"},
		{"the continuations are spent", run("Failed", ""), "finished", "not_met", "not_met"},
		{"an unknown end is ignored", run("Failed", ""), "finished", "met", "agent_finished"},
		{"a succeeded run ignores an end", run("Succeeded", ""), "finished", "stalled", "agent_finished"},
		{"the budget wins", run("BudgetExhausted", "budget-run"), "paused", "stalled", "budget-run"},
		{"a revocation wins", run("Revoked", "manual"), "paused", "stalled", "revoked"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := EndReason(c.r, c.status, c.end, false); got != c.want {
				t.Fatalf("got %s want %s", got, c.want)
			}
		})
	}
}

func TestFromUnstructuredUnsandboxed(t *testing.T) {
	for _, tc := range []struct {
		conds []any
		want  bool
	}{
		{[]any{map[string]any{"type": "Unsandboxed", "status": "True", "reason": "AllowUnsandboxed"}}, true},
		{[]any{map[string]any{"type": "Unsandboxed", "status": "False"}}, false},
		{[]any{map[string]any{"type": "Ready", "status": "True"}}, false},
		{nil, false},
	} {
		u := claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Running", "")
		if tc.conds != nil {
			if err := unstructured.SetNestedSlice(u.Object, tc.conds, "status", "conditions"); err != nil {
				t.Fatal(err)
			}
		}
		r, ok := FromUnstructured(u)
		if !ok || r.Unsandboxed != tc.want {
			t.Errorf("%v: ok %v unsandboxed %v", tc.conds, ok, r.Unsandboxed)
		}
	}
}

func TestObserveRecordsAnUnsandboxedRun(t *testing.T) {
	for _, tc := range []struct {
		unsandboxed bool
		want        string
	}{
		{true, `state_changed:{"kind":"run_phase","phase":"Running","unsandboxed":true}`},
		{false, `state_changed:{"kind":"run_phase","phase":"Running"}`},
	} {
		fs := &fakeStore{keys: map[string]bool{}}
		r, _ := FromUnstructured(claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Running", ""))
		r.Unsandboxed = tc.unsandboxed
		if err := (&Events{Store: fs}).Observe(t.Context(), r); err != nil {
			t.Fatal(err)
		}
		if got := payloads(fs.drafts); len(got) != 2 || got[1] != tc.want {
			t.Errorf("unsandboxed %v: %v", tc.unsandboxed, got)
		}
	}
}

// spec.model is the claim's, set by the factory or the broker, never the sandbox's: the one source
// of a run's model for the room and the metric. A value outside the envelope's bound is dropped.
func TestTheRunsModelIsTheClaims(t *testing.T) {
	withModel := func(m any) *unstructured.Unstructured {
		u := claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Running", "")
		_ = unstructured.SetNestedField(u.Object, m, "spec", "model")
		return u
	}
	for name, c := range map[string]struct {
		u    *unstructured.Unstructured
		want string
	}{
		"a logical model name":  {withModel("agent-default"), "agent-default"},
		"none set":              {claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Running", ""), ""},
		"one outside the bound": {withModel("agent default <b>"), ""},
	} {
		t.Run(name, func(t *testing.T) {
			r, ok := FromUnstructured(c.u)
			if !ok || r.Model != c.want {
				t.Fatalf("model %q (ok %v), want %q", r.Model, ok, c.want)
			}
			fs := &fakeStore{keys: map[string]bool{}}
			if err := (&Events{Store: fs}).Observe(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			want := `{"kind":"run_phase","phase":"Running"}`
			if c.want != "" {
				want = `{"kind":"run_phase","model":"` + c.want + `","phase":"Running"}`
			}
			if got := string(fs.drafts[1].Payload); got != want {
				t.Fatalf("run_phase %s, want %s", got, want)
			}
		})
	}
}

// An unsandboxed run given a model: the Running step carries both.
func TestTheRunningStepCarriesTheModelAndUnsandboxed(t *testing.T) {
	u := claim("xplane-run-7f3cq2xz", "3kq7x2ma", "Running", "")
	_ = unstructured.SetNestedField(u.Object, "agent-default", "spec", "model")
	_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"type": "Unsandboxed", "status": "True"}}, "status", "conditions")
	r, _ := FromUnstructured(u)
	fs := &fakeStore{keys: map[string]bool{}}
	if err := (&Events{Store: fs}).Observe(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"run_phase","model":"agent-default","phase":"Running","unsandboxed":true}`
	if got := string(fs.drafts[1].Payload); got != want {
		t.Fatalf("run_phase %s, want %s", got, want)
	}
}
