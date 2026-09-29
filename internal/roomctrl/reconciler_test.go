// SPDX-License-Identifier: Apache-2.0

package roomctrl

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
)

// fakeStore keeps rooms in memory. sealed makes every Append refuse, as a room
// sealed by its limit does.
type fakeStore struct {
	rooms  map[string]*store.RoomState
	drafts []envelope.Draft
	closed []string
	sealed bool
}

func (m *fakeStore) EnsureRoom(_ context.Context, r store.NewRoom) (bool, error) {
	if _, ok := m.rooms[r.ID]; ok {
		return false, nil
	}
	m.rooms[r.ID] = &store.RoomState{ID: r.ID, Driver: r.Driver}
	return true, nil
}

func (m *fakeStore) Append(_ context.Context, d envelope.Draft) (envelope.Event, bool, error) {
	if m.sealed {
		return envelope.Event{}, false, store.ErrSealed
	}
	m.drafts = append(m.drafts, d)
	m.rooms[d.RoomID].LastSeq++
	return envelope.Event{Seq: m.rooms[d.RoomID].LastSeq}, false, nil
}

func (m *fakeStore) Room(_ context.Context, id string) (store.RoomState, error) {
	return *m.rooms[id], nil
}

func (m *fakeStore) CloseRoom(_ context.Context, id, _ string) error {
	st, ok := m.rooms[id]
	if !ok {
		return store.ErrNoRoom
	}
	m.closed = append(m.closed, id)
	closed := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	st.Sealed, st.ClosedAt = true, &closed
	return nil
}

func (m *fakeStore) PendingApprovals(context.Context, string) (int, error) { return 0, nil }

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

func agentRun(name, room string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(runwatch.GVK())
	u.SetName(name)
	u.SetNamespace(runwatch.Namespace)
	_ = unstructured.SetNestedField(u.Object, room, "spec", "roomRef")
	return u
}

func request() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "agent-system", Name: "3kq7x2ma"}}
}

func newRoom() *v1alpha1.Room {
	return &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: "3kq7x2ma", Namespace: "agent-system"},
		Spec: v1alpha1.RoomSpec{Owner: "human:1", Driver: "human:1", DataClass: "public", Retention: "90d"}}
}

func TestCreateEnsuresRowOpenEventAndFinalizer(t *testing.T) {
	room := newRoom()
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(room).WithStatusSubresource(room).Build()
	ms := &fakeStore{rooms: map[string]*store.RoomState{}}
	var observed string
	r := &Reconciler{Client: c, Store: ms, Runs: runwatch.New(),
		Observe: func(_ string, st v1alpha1.RoomStatus, _ time.Time) { observed = st.Phase }}
	res, err := r.Reconcile(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != resync {
		t.Fatalf("requeue after %s, want the %s resync", res.RequeueAfter, resync)
	}
	var got v1alpha1.Room
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(room), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) != 1 || got.Finalizers[0] != Finalizer {
		t.Fatalf("finalizers = %v", got.Finalizers)
	}
	if len(ms.drafts) != 1 || ms.drafts[0].OriginClient != "broker:room" || ms.drafts[0].OriginSeq != 1 {
		t.Fatalf("seq 1 must be the broker's room_phase Open: %+v", ms.drafts)
	}
	if got.Status.Phase != "Open" || got.Status.LastSeq != 1 || got.Status.Driver != "human:1" {
		t.Fatalf("status = %+v", got.Status)
	}
	if observed != "Open" {
		t.Fatalf("Observe saw phase %q, want Open", observed)
	}
}

// A room sealed by its limit still reconciles: its Open event is history, not an error.
func TestSealedRoomStillReconciles(t *testing.T) {
	room := newRoom()
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(room).WithStatusSubresource(room).Build()
	ms := &fakeStore{rooms: map[string]*store.RoomState{}, sealed: true}
	r := &Reconciler{Client: c, Store: ms, Runs: runwatch.New()}
	if _, err := r.Reconcile(t.Context(), request()); err != nil {
		t.Fatalf("a sealed room is not a reconcile error: %v", err)
	}
}

func TestMissingRoomIsNotAnError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme()).Build()
	r := &Reconciler{Client: c, Store: &fakeStore{rooms: map[string]*store.RoomState{}}, Runs: runwatch.New()}
	if _, err := r.Reconcile(t.Context(), request()); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteDeletesRunsAndSeals(t *testing.T) {
	now := metav1.Now()
	room := newRoom()
	room.Finalizers, room.DeletionTimestamp = []string{Finalizer}, &now
	run := agentRun("xplane-run-7f3cq2xz", "3kq7x2ma")
	c := fake.NewClientBuilder().WithScheme(scheme()).WithRESTMapper(mapper()).WithObjects(room, run).Build()
	w := runwatch.New()
	w.Upsert(t.Context(), run)
	ms := &fakeStore{rooms: map[string]*store.RoomState{"3kq7x2ma": {ID: "3kq7x2ma"}}}
	r := &Reconciler{Client: c, Store: ms, Runs: w}
	if _, err := r.Reconcile(t.Context(), request()); err != nil {
		t.Fatal(err)
	}
	if len(ms.closed) != 1 {
		t.Fatal("the log was not sealed")
	}
	left := agentRun("xplane-run-7f3cq2xz", "")
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(run), left); !apierrors.IsNotFound(err) {
		t.Fatal("the room's run was not deleted")
	}
	var gone v1alpha1.Room
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(room), &gone); !apierrors.IsNotFound(err) {
		t.Fatal("the finalizer was not released")
	}
}

// A room whose row never existed still releases its CR.
func TestDeleteWithoutALogRowReleases(t *testing.T) {
	now := metav1.Now()
	room := newRoom()
	room.Finalizers, room.DeletionTimestamp = []string{Finalizer}, &now
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(room).Build()
	r := &Reconciler{Client: c, Store: &fakeStore{rooms: map[string]*store.RoomState{}}, Runs: runwatch.New()}
	if _, err := r.Reconcile(t.Context(), request()); err != nil {
		t.Fatal(err)
	}
	var gone v1alpha1.Room
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(room), &gone); !apierrors.IsNotFound(err) {
		t.Fatal("the finalizer was not released")
	}
}

func TestPhase(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
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
