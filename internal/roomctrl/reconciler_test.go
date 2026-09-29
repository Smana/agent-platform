// SPDX-License-Identifier: Apache-2.0

package roomctrl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
)

const roomID = "3kq7x2ma"

// deletedAt is every deleting room's deletionTimestamp; the tests' clocks are set against it.
var deletedAt = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// fakeStore keeps rooms in memory. Its errors are wrapped the way the real store
// wraps them, so a sentinel compared with == instead of errors.Is fails a test.
// Every call that writes is also recorded in ops, shared with fakeEnds, to assert order.
type fakeStore struct {
	rooms   map[string]*store.RoomState
	drafts  []envelope.Draft
	keys    map[string]bool
	ensured int
	ops     *[]string
}

func newStore(ops *[]string, rooms ...store.RoomState) *fakeStore {
	m := &fakeStore{rooms: map[string]*store.RoomState{}, keys: map[string]bool{}, ops: ops}
	for _, r := range rooms {
		m.rooms[r.ID] = &r
	}
	return m
}

func (m *fakeStore) record(op string) {
	if m.ops != nil {
		*m.ops = append(*m.ops, op)
	}
}

func (m *fakeStore) EnsureRoom(_ context.Context, r store.NewRoom) (bool, error) {
	m.ensured++
	if _, ok := m.rooms[r.ID]; ok {
		return false, nil
	}
	m.rooms[r.ID] = &store.RoomState{ID: r.ID, Driver: r.Driver, LastEventAt: deletedAt}
	return true, nil
}

func (m *fakeStore) Append(_ context.Context, d envelope.Draft) (envelope.Event, bool, error) {
	st, ok := m.rooms[d.RoomID]
	if !ok {
		return envelope.Event{}, false, fmt.Errorf("store: append to room %s: %w", d.RoomID, store.ErrNoRoom)
	}
	// Keyed before the seal check, as the store does: a replay is a duplicate.
	k := fmt.Sprintf("%s/%d", d.OriginClient, d.OriginSeq)
	if m.keys[k] {
		return envelope.Event{}, true, nil
	}
	if st.Sealed {
		return envelope.Event{}, false, fmt.Errorf("store: append to room %s: %w", d.RoomID, store.ErrSealed)
	}
	m.keys[k] = true
	m.drafts = append(m.drafts, d)
	st.LastSeq++
	return envelope.Event{Seq: st.LastSeq}, false, nil
}

func (m *fakeStore) Room(_ context.Context, id string) (store.RoomState, error) {
	st, ok := m.rooms[id]
	if !ok {
		return store.RoomState{ID: id}, store.ErrNoRoom // Room returns the bare sentinel, as store.Room does
	}
	return *st, nil
}

func (m *fakeStore) CloseRoom(_ context.Context, id, _ string) error {
	st, ok := m.rooms[id]
	if !ok {
		return fmt.Errorf("store: close room %s: %w", id, store.ErrNoRoom)
	}
	m.record("close")
	closed := deletedAt
	st.Sealed, st.ClosedAt = true, &closed
	return nil
}

func (m *fakeStore) PendingApprovals(context.Context, string) (int, error) { return 0, nil }

// fakeEnds records the run ends the reconciler writes before a seal.
type fakeEnds struct{ ops *[]string }

func (f fakeEnds) Observe(_ context.Context, r runwatch.Run) error {
	*f.ops = append(*f.ops, "end:"+r.ID+":"+r.Phase)
	return nil
}

func (f fakeEnds) ObserveDeleted(_ context.Context, r runwatch.Run) error {
	*f.ops = append(*f.ops, "deleted:"+r.ID)
	return nil
}

// mapper knows AgentRun, which the scheme does not, as namespaced: the fake client
// resolves an object's scope through its RESTMapper (review M3).
func mapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(runwatch.GVK(), meta.RESTScopeNamespace)
	m.Add(v1alpha1.GroupVersion.WithKind("Room"), meta.RESTScopeNamespace)
	return m
}

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	return s
}

// agentRun is a run claim of the room; hold keeps it present after a delete, as a
// claim whose composed resources are still being torn down is.
func agentRun(id, room, phase string, hold bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(runwatch.GVK())
	u.SetName("xplane-run-" + id)
	u.SetNamespace(runwatch.Namespace)
	_ = unstructured.SetNestedField(u.Object, room, "spec", "roomRef")
	if phase != "" {
		_ = unstructured.SetNestedField(u.Object, phase, "status", "phase")
	}
	if hold {
		u.SetFinalizers([]string{"test/hold"})
	}
	return u
}

func request() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "agent-system", Name: roomID}}
}

func newRoom() *v1alpha1.Room {
	return &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: roomID, Namespace: "agent-system"},
		Spec: v1alpha1.RoomSpec{Owner: "human:1", Driver: "human:1", DataClass: "public", Retention: "90d"}}
}

func deletingRoom() *v1alpha1.Room {
	room := newRoom()
	at := metav1.NewTime(deletedAt)
	room.Finalizers, room.DeletionTimestamp = []string{Finalizer}, &at
	return room
}

func clock(t time.Time) func() time.Time { return func() time.Time { return t } }

func build(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(scheme()).WithRESTMapper(mapper()).
		WithObjects(objs...).WithStatusSubresource(&v1alpha1.Room{}).Build()
}

func getRoom(t *testing.T, c client.Client) (v1alpha1.Room, error) {
	t.Helper()
	var got v1alpha1.Room
	err := c.Get(t.Context(), request().NamespacedName, &got)
	return got, err
}

func TestCreateEnsuresRowOpenEventAndFinalizer(t *testing.T) {
	c := build(newRoom())
	ms := newStore(nil)
	var observed string
	r := &Reconciler{Client: c, Store: ms, Runs: runwatch.New(), Now: clock(deletedAt),
		Observe: func(_ string, st v1alpha1.RoomStatus, _ time.Time) { observed = st.Phase }}
	res, err := r.Reconcile(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != resync {
		t.Fatalf("requeue after %s, want the %s resync", res.RequeueAfter, resync)
	}
	got, err := getRoom(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) != 1 || got.Finalizers[0] != Finalizer {
		t.Fatalf("finalizers = %v", got.Finalizers)
	}
	if len(ms.drafts) != 1 || ms.drafts[0].OriginClient != "broker:room" || ms.drafts[0].OriginSeq != 1 {
		t.Fatalf("seq 1 must be the broker's room_phase Open: %+v", ms.drafts)
	}
	if got.Status.Phase != "Open" || got.Status.LastSeq != 1 || got.Status.Driver != "human:1" ||
		got.Status.ObservedGeneration != got.Generation {
		t.Fatalf("status = %+v, generation %d", got.Status, got.Generation)
	}
	if observed != "Open" {
		t.Fatalf("Observe saw phase %q, want Open", observed)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	t.Run("a second reconcile writes nothing", func(t *testing.T) {
		c := build(newRoom())
		ms := newStore(nil)
		r := &Reconciler{Client: c, Store: ms, Runs: runwatch.New(), Now: clock(deletedAt)}
		for range 2 {
			if _, err := r.Reconcile(t.Context(), request()); err != nil {
				t.Fatal(err)
			}
		}
		if got, _ := getRoom(t, c); len(ms.drafts) != 1 || ms.ensured != 1 || got.Status.LastSeq != 1 {
			t.Fatalf("%d drafts, %d ensures, lastSeq %d; want one each", len(ms.drafts), ms.ensured, got.Status.LastSeq)
		}
	})
	t.Run("a lost status write reopens nothing", func(t *testing.T) {
		c := build(newRoom())
		ms := newStore(nil)
		r := &Reconciler{Client: c, Store: ms, Runs: runwatch.New(), Now: clock(deletedAt)}
		if _, err := r.Reconcile(t.Context(), request()); err != nil {
			t.Fatal(err)
		}
		// A new leader whose status write was lost sees lastSeq 0 and retries the open.
		room, _ := getRoom(t, c)
		room.Status = v1alpha1.RoomStatus{}
		if err := c.Status().Update(t.Context(), &room); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Reconcile(t.Context(), request()); err != nil {
			t.Fatal(err)
		}
		if got, _ := getRoom(t, c); len(ms.drafts) != 1 || got.Status.LastSeq != 1 {
			t.Fatalf("%d drafts, lastSeq %d; the open is keyed and lands once", len(ms.drafts), got.Status.LastSeq)
		}
	})
}

// Retention deletes a sealed room's row; the Room CR can outlive it. The next
// resync must neither re-insert the row nor write a fresh seq 1.
func TestPurgedLogIsNeverResurrected(t *testing.T) {
	room := newRoom()
	room.Finalizers = []string{Finalizer}
	room.Status = v1alpha1.RoomStatus{Phase: "Closed", LastSeq: 42, Driver: "human:1"}
	c := build(room)
	ms := newStore(nil) // no row: retention purged it
	r := &Reconciler{Client: c, Store: ms, Runs: runwatch.New(), Now: clock(deletedAt)}
	if _, err := r.Reconcile(t.Context(), request()); err != nil {
		t.Fatalf("a purged log is not a reconcile error: %v", err)
	}
	if ms.ensured != 0 || len(ms.drafts) != 0 || len(ms.rooms) != 0 {
		t.Fatalf("%d ensures, %d drafts: the purged log came back", ms.ensured, len(ms.drafts))
	}
	if got, _ := getRoom(t, c); got.Status.Phase != "Closed" || got.Status.LastSeq != 42 {
		t.Fatalf("status = %+v, want the last known Closed", got.Status)
	}
}

func TestStatusProjection(t *testing.T) {
	running := agentRun("7f3cq2xz", roomID, "Running", false)
	for _, tc := range []struct {
		name  string
		now   time.Time
		runs  []*unstructured.Unstructured
		gen   int64
		phase string
	}{
		{"a running run makes the room Active", deletedAt.Add(time.Minute), []*unstructured.Unstructured{running}, 1, "Active"},
		{"the injected clock decides a stall", deletedAt.Add(31 * time.Minute), []*unstructured.Unstructured{running}, 1, "AwaitingHuman"},
		{"only finished runs leave it Idle", deletedAt, []*unstructured.Unstructured{agentRun("7f3cq2xz", roomID, "Succeeded", false)}, 1, "Idle"},
		{"observedGeneration follows the spec's generation", deletedAt, nil, 4, "Open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			room := newRoom()
			room.Generation = tc.gen
			c := build(room)
			w := runwatch.New()
			for _, u := range tc.runs {
				w.Upsert(t.Context(), u)
			}
			r := &Reconciler{Client: c, Store: newStore(nil), Runs: w, Now: clock(tc.now)}
			if _, err := r.Reconcile(t.Context(), request()); err != nil {
				t.Fatal(err)
			}
			got, _ := getRoom(t, c)
			if got.Status.Phase != tc.phase || got.Status.ObservedGeneration != tc.gen {
				t.Fatalf("status = %+v, generation %d; want phase %s", got.Status, got.Generation, tc.phase)
			}
		})
	}
}

// A room sealed by its limit still reconciles: its Open event is history, not an error.
func TestSealedRoomStillReconciles(t *testing.T) {
	c := build(newRoom())
	ms := newStore(nil, store.RoomState{ID: roomID, Sealed: true, LastSeq: 100})
	r := &Reconciler{Client: c, Store: ms, Runs: runwatch.New(), Now: clock(deletedAt)}
	if _, err := r.Reconcile(t.Context(), request()); err != nil {
		t.Fatalf("a sealed room is not a reconcile error: %v", err)
	}
	if got, _ := getRoom(t, c); got.Status.Phase != "Closed" {
		t.Fatalf("status = %+v", got.Status)
	}
}

func TestMissingRoomIsNotAnError(t *testing.T) {
	r := &Reconciler{Client: build(), Store: newStore(nil), Runs: runwatch.New()}
	if _, err := r.Reconcile(t.Context(), request()); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidRetentionIsTerminal(t *testing.T) {
	room := newRoom()
	room.Spec.Retention = "0d"
	r := &Reconciler{Client: build(room), Store: newStore(nil), Runs: runwatch.New()}
	_, err := r.Reconcile(t.Context(), request())
	if !errors.Is(err, reconcile.TerminalError(nil)) {
		t.Fatalf("err = %v, want a terminal error", err)
	}
}

func TestConflictRequeuesWithoutError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(newRoom()).WithStatusSubresource(&v1alpha1.Room{}).
		WithInterceptorFuncs(interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			return apierrors.NewConflict(schema.GroupResource{Group: "agents.ogenki.io", Resource: "rooms"}, roomID, nil)
		}}).Build()
	r := &Reconciler{Client: c, Store: newStore(nil), Runs: runwatch.New()}
	res, err := r.Reconcile(t.Context(), request())
	if err != nil || res.RequeueAfter <= 0 {
		t.Fatalf("result %+v, err %v; want a quiet requeue", res, err)
	}
}

// Ruling AH, review I1: the runs are listed from the API, not the watcher, and a
// live one holds the seal until the timeout.
func TestDeleteWaitsForLiveRunsTheWatcherDoesNotKnow(t *testing.T) {
	run := agentRun("7f3cq2xz", roomID, "Running", true)
	c := build(deletingRoom(), run)
	var ops []string
	ms := newStore(&ops, store.RoomState{ID: roomID})
	r := &Reconciler{Client: c, Store: ms, Runs: runwatch.New(), Ends: fakeEnds{&ops}, Now: clock(deletedAt.Add(10 * time.Second))}
	res, err := r.Reconcile(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter <= 0 || len(ops) != 0 {
		t.Fatalf("result %+v, ops %v: a live run must hold the seal", res, ops)
	}
	var left unstructured.Unstructured
	left.SetGroupVersionKind(runwatch.GVK())
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(run), &left); err != nil || left.GetDeletionTimestamp() == nil {
		t.Fatalf("the live run was not deleted: %v", err)
	}
	if got, err := getRoom(t, c); err != nil || !slices.Contains(got.Finalizers, Finalizer) {
		t.Fatalf("the room was released early: %v", err)
	}
}

func TestDeleteSealsAfterTheTimeoutAndSaysWhy(t *testing.T) {
	c := build(deletingRoom(), agentRun("7f3cq2xz", roomID, "Running", true))
	var ops []string
	var logs bytes.Buffer
	r := &Reconciler{Client: c, Store: newStore(&ops, store.RoomState{ID: roomID}), Runs: runwatch.New(),
		Ends: fakeEnds{&ops}, Now: clock(deletedAt.Add(sealTimeout + time.Second)),
		Log: slog.New(slog.NewTextHandler(&logs, nil))}
	if _, err := r.Reconcile(t.Context(), request()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"deleted:7f3cq2xz", "close"}; !slices.Equal(ops, want) {
		t.Fatalf("ops = %v, want %v", ops, want)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "7f3cq2xz") {
		t.Fatalf("no warning naming the live run: %s", logs.String())
	}
	if _, err := getRoom(t, c); !apierrors.IsNotFound(err) {
		t.Fatal("the finalizer was not released")
	}
}

// P15: why each run ended lands before the seal, for runs the API still lists and
// for runs only the watcher still remembers.
func TestDeleteRecordsRunEndsBeforeTheSeal(t *testing.T) {
	ended := agentRun("7f3cq2xz", roomID, "Failed", true)
	gone := agentRun("aaaabbbb", roomID, "Running", false)
	other := agentRun("ccccdddd", "zzzzzzzz", "Running", true)
	c := build(deletingRoom(), ended, other)
	w := runwatch.New()
	w.Upsert(t.Context(), gone)
	var ops []string
	r := &Reconciler{Client: c, Store: newStore(&ops, store.RoomState{ID: roomID}), Runs: w, Ends: fakeEnds{&ops},
		Now: clock(deletedAt.Add(time.Second))}
	if _, err := r.Reconcile(t.Context(), request()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"end:7f3cq2xz:Failed", "deleted:aaaabbbb", "close"}; !slices.Equal(ops, want) {
		t.Fatalf("ops = %v, want %v", ops, want)
	}
	var kept unstructured.Unstructured
	kept.SetGroupVersionKind(runwatch.GVK())
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(other), &kept); err != nil || kept.GetDeletionTimestamp() != nil {
		t.Fatalf("another room's run was touched: %v", err)
	}
}

func TestDeleteDeletesRunsThenSeals(t *testing.T) {
	run := agentRun("7f3cq2xz", roomID, "", false)
	c := build(deletingRoom(), run)
	var ops []string
	ms := newStore(&ops, store.RoomState{ID: roomID})
	r := &Reconciler{Client: c, Store: ms, Runs: runwatch.New(), Ends: fakeEnds{&ops}, Now: clock(deletedAt.Add(time.Second))}
	res, err := r.Reconcile(t.Context(), request())
	if err != nil || res.RequeueAfter <= 0 {
		t.Fatalf("result %+v, err %v: the Pending run was live when listed", res, err)
	}
	var left unstructured.Unstructured
	left.SetGroupVersionKind(runwatch.GVK())
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(run), &left); !apierrors.IsNotFound(err) {
		t.Fatal("the room's run was not deleted")
	}
	if _, err := r.Reconcile(t.Context(), request()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ops, []string{"close"}) {
		t.Fatalf("ops = %v, want the seal once the run is gone", ops)
	}
	if _, err := getRoom(t, c); !apierrors.IsNotFound(err) {
		t.Fatal("the finalizer was not released")
	}
}

// A room whose row never existed, or was purged, still releases its CR.
func TestDeleteWithoutALogRowReleases(t *testing.T) {
	c := build(deletingRoom())
	var ops []string
	r := &Reconciler{Client: c, Store: newStore(&ops), Runs: runwatch.New(), Ends: fakeEnds{&ops}, Now: clock(deletedAt)}
	if _, err := r.Reconcile(t.Context(), request()); err != nil {
		t.Fatal(err)
	}
	if _, err := getRoom(t, c); !apierrors.IsNotFound(err) {
		t.Fatal("the finalizer was not released")
	}
}

func TestPhase(t *testing.T) {
	now := deletedAt
	closed := now
	for _, c := range []struct {
		name    string
		st      store.RoomState
		runs    []runwatch.Run
		pending int
		want    string
	}{
		{"a room with only its open event is Open", store.RoomState{LastSeq: 1, LastEventAt: now}, nil, 0, "Open"},
		{"a running run makes it Active", store.RoomState{LastSeq: 9, LastEventAt: now}, []runwatch.Run{{Phase: "Running"}}, 0, "Active"},
		{"only finished runs make it Idle", store.RoomState{LastSeq: 9, LastEventAt: now}, []runwatch.Run{{Phase: "Succeeded"}}, 0, "Idle"},
		{"a pending approval awaits a human", store.RoomState{LastSeq: 9, LastEventAt: now}, []runwatch.Run{{Phase: "Running"}}, 1, "AwaitingHuman"},
		{"a running room silent past the stall awaits a human", store.RoomState{LastSeq: 9, LastEventAt: now.Add(-31 * time.Minute)}, []runwatch.Run{{Phase: "Running"}}, 0, "AwaitingHuman"},
		{"a sealed room is Closed", store.RoomState{LastSeq: 9, Sealed: true, ClosedAt: &closed}, nil, 0, "Closed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Phase(c.st, c.runs, c.pending, now); got != c.want {
				t.Errorf("got %s want %s", got, c.want)
			}
		})
	}
}

func TestParseRetention(t *testing.T) {
	for _, c := range []struct {
		name, in string
		want     time.Duration
		ok       bool
	}{
		{"days are read", "90d", 90 * 24 * time.Hour, true},
		{"empty defaults to OD-17's 90d", "", 90 * 24 * time.Hour, true},
		{"the CRD's largest value fits", "9999d", 9999 * 24 * time.Hour, true},
		{"zero days is refused", "0d", 0, false},
		{"a unit other than days is refused", "90h", 0, false},
		{"a sign is refused", "+5d", 0, false},
		{"beyond the CRD's bound is refused", "10000d", 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseRetention(c.in)
			if (err == nil) != c.ok || got != c.want {
				t.Fatalf("ParseRetention(%q) = %s, %v", c.in, got, err)
			}
		})
	}
}

func TestStatusClampsPendingApprovals(t *testing.T) {
	if got := clampInt32(1 << 40); got != 1<<31-1 {
		t.Fatalf("clampInt32 = %d", got)
	}
}
