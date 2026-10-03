// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	factoryv1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/fmetrics"
	"github.com/Smana/agent-platform/internal/factory/runs"
	"github.com/Smana/agent-platform/internal/policy"
)

type byToken map[string]authn.Principal

func (b byToken) Authenticate(r *http.Request) (authn.Principal, error) {
	p, ok := b[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if !ok {
		return authn.Principal{}, authn.ErrUnauthenticated
	}
	return p, nil
}

type store struct {
	created  []runs.Spec
	existing []runs.Run
}

func (s *store) Create(_ context.Context, sp runs.Spec) error {
	s.created = append(s.created, sp)
	return nil
}
func (s *store) List(context.Context) ([]runs.Run, error) { return s.existing, nil }

var now = time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

func server(t *testing.T, st *store, enforce bool, objs ...client.Object) http.Handler {
	return serverWith(t, st, enforce, false, objs...)
}

// objs are Rooms, Tasks and ledgers the API reads (room rights, R35's resume checks, R50's
// admission).
func serverWith(t *testing.T, st *store, enforce, stopped bool, objs ...client.Object) http.Handler {
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(objs...).Build()
	return mkServer(t, st, c, enforce, stopped, "7f3cq2xz", 5_000_000).Handler()
}

func mkServer(t *testing.T, st *store, c client.Client, enforce, stopped bool, runID string, humanDaily int64) *Server {
	t.Helper()
	m, err := fmetrics.New(nil, nil, "agent-system", func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		Auth: byToken{
			"alice": {Kind: envelope.ActorHuman, ID: "human:291", Groups: []string{"agents-member"}},
			"admin": {Kind: envelope.ActorHuman, ID: "human:1", Groups: []string{"agents-admin"}},
		},
		Groups: policy.Groups{Admin: "agents-admin", Member: "agents-member"},
		Cfg: &config.Config{API: config.API{Repositories: []string{"Smana/cloud-native-ref"}},
			Budgets: config.Budgets{HumanDaily: humanDaily, FactoryDaily: 25_000_000, EnforcePrincipal: enforce}},
		Runs: st, Rooms: c, Namespace: "agent-system",
		Stopped:  func(context.Context) bool { return stopped },
		NewRunID: func() string { return runID }, Now: func() time.Time { return now },
		Metrics: m,
	}
}

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	_ = factoryv1.AddToScheme(s)
	return s
}

// ledgerCM is one day's R50 ledger with its spent column seeded; the meter (Task 5.3) owns that
// column, so a test is the only one who writes it before then.
func ledgerCM(day string, spent map[string]int64) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: LedgerPrefix + day, Namespace: "agent-system"}}
	if spent != nil {
		b, err := json.Marshal(spent)
		if err != nil {
			panic(err)
		}
		cm.Data = map[string]string{LedgerSpent: string(b)}
	}
	return cm
}

func aliceRoom() *v1alpha1.Room {
	return &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: "3kq7x2ma", Namespace: "agent-system"},
		Spec:   v1alpha1.RoomSpec{Owner: "human:291", Driver: "human:291", DataClass: "public"},
		Status: v1alpha1.RoomStatus{Driver: "human:291"}}
}

// §6.1: the stop object pauses intake, and this API is intake. Nothing is created.
func TestTheStopObjectPausesTheAPI(t *testing.T) {
	st := &store{}
	if w := post(serverWith(t, st, false, true), "alice", base()); w.Code != http.StatusServiceUnavailable ||
		!strings.Contains(w.Body.String(), "kill_switch") || len(st.created) != 0 {
		t.Fatalf("%d %s %d", w.Code, w.Body, len(st.created))
	}
}

func post(h http.Handler, tok string, body map[string]any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequestWithContext(context.Background(), "POST", "/v1/runs", bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func base() map[string]any {
	return map[string]any{"role": "implementer", "repository": "Smana/cloud-native-ref", "task": map[string]any{"text": "Fix the link"},
		"dataClass": "public"}
}

// SC-13: the principal is the token's, whatever the body says; the branch is the factory's.
func TestPrincipalAndBranchComeFromTheFactory(t *testing.T) {
	st := &store{}
	body := base()
	body["principal"], body["branch"] = "human:someone-else", "main"
	w := post(server(t, st, false), "alice", body)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"runId":"7f3cq2xz"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	s := st.created[0]
	if s.Principal != "human:291" || s.Branch != "agent/7f3cq2xz" || s.Queue != runs.QueueInteractive ||
		s.MaxTokens != 2_000_000 || s.Model != "agent-default" || s.BaseRef != "main" {
		t.Fatalf("%+v", s)
	}
}

func TestRooms(t *testing.T) {
	room := aliceRoom()
	body := base()
	body["roomRef"] = "3kq7x2ma"
	st := &store{}
	if w := post(server(t, st, false, room), "alice", body); w.Code != http.StatusCreated || st.created[0].Branch != "agent/3kq7x2ma" || st.created[0].RoomRef != "3kq7x2ma" {
		t.Fatalf("a room's runs share its branch (C3): %d %+v", w.Code, st.created)
	}
	body["dataClass"] = "internal"
	if w := post(server(t, &store{}, false, room), "alice", body); w.Code != http.StatusBadRequest {
		t.Fatalf("the room's data class: %d", w.Code)
	}
	body["dataClass"] = "public"
	other := room.DeepCopy()
	other.Spec.Owner, other.Status.Driver = "human:9", "human:9"
	if w := post(server(t, &store{}, false, other), "alice", body); w.Code != http.StatusForbidden {
		t.Fatalf("not hers to act in: %d", w.Code)
	}
	collab := other.DeepCopy()
	collab.Spec.Members = []v1alpha1.Member{{Principal: "human:291", Role: "collaborator"}}
	if w := post(server(t, &store{}, false, collab), "alice", body); w.Code != http.StatusForbidden {
		t.Fatalf("a collaborator who does not drive cannot start a run, as in the room UI (SP2 StartRun): %d", w.Code)
	}
	if w := post(server(t, &store{}, false, other), "admin", body); w.Code != http.StatusCreated {
		t.Fatalf("agents-admin owns every room (SP2 policy): %d", w.Code)
	}
	busy := &store{existing: []runs.Run{{ID: "aaaaaaaa", RoomRef: "3kq7x2ma", Phase: "Running"}}}
	if w := post(server(t, busy, false, room), "alice", body); w.Code != http.StatusConflict {
		t.Fatalf("one Running run per room: %d", w.Code)
	}
}

func TestRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(map[string]any)
		tok  string
		code int
	}{
		"unauthenticated":       {func(map[string]any) {}, "nobody", http.StatusUnauthorized},
		"other repository":      {func(b map[string]any) { b["repository"] = "Smana/other" }, "alice", http.StatusForbidden},
		"above the ceiling":     {func(b map[string]any) { b["maxTokens"] = 6_000_000 }, "alice", http.StatusBadRequest},
		"reviewer without a PR": {func(b map[string]any) { b["role"] = "reviewer" }, "alice", http.StatusBadRequest},
		"two tasks": {func(b map[string]any) {
			b["task"] = map[string]any{"text": "a", "url": "https://github.com/Smana/cloud-native-ref/issues/1"}
		}, "alice", http.StatusBadRequest},
		"unknown role": {func(b map[string]any) { b["role"] = "merger" }, "alice", http.StatusBadRequest},
		// R37 (owner default): internal data and triagers are the admins'.
		"internal, a member":         {func(b map[string]any) { b["dataClass"] = "internal" }, "alice", http.StatusForbidden},
		"triager, a member":          {func(b map[string]any) { b["role"] = "triager" }, "alice", http.StatusForbidden},
		"internal, an admin":         {func(b map[string]any) { b["dataClass"] = "internal" }, "admin", http.StatusCreated},
		"triager, an admin":          {func(b map[string]any) { b["role"] = "triager" }, "admin", http.StatusCreated},
		"a resume branch, malformed": {func(b map[string]any) { b["resumeBranch"] = "main" }, "alice", http.StatusBadRequest},
	} {
		b := base()
		c.edit(b)
		if w := post(server(t, &store{}, false), c.tok, b); w.Code != c.code {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
}

// R35: a stopped human run resumes on its branch; a task's branch, a busy branch or a room the
// caller cannot start a run in stay closed.
func TestResumeBranch(t *testing.T) {
	body := base()
	body["resumeBranch"] = "agent/aaaaaaaa"
	st := &store{}
	if w := post(server(t, st, false), "alice", body); w.Code != http.StatusCreated || st.created[0].Branch != "agent/aaaaaaaa" {
		t.Fatalf("%d %+v", w.Code, st.created)
	}
	task := &factoryv1.Task{ObjectMeta: metav1.ObjectMeta{Name: "aaaaaaaa", Namespace: "agent-system"}}
	if w := post(server(t, &store{}, false, task), "alice", body); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "task_branch") {
		t.Fatalf("a task resumes with /factory retry: %d %s", w.Code, w.Body)
	}
	busy := &store{existing: []runs.Run{{ID: "bbbbbbbb", Branch: "agent/aaaaaaaa", Phase: "Running"}}}
	if w := post(server(t, busy, false), "alice", body); w.Code != http.StatusConflict {
		t.Fatalf("a live run holds it: %d", w.Code)
	}
	room := &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: "aaaaaaaa", Namespace: "agent-system"},
		Spec: v1alpha1.RoomSpec{Owner: "human:9", Driver: "human:9", DataClass: "public"}, Status: v1alpha1.RoomStatus{Driver: "human:9"}}
	if w := post(server(t, &store{}, false, room), "alice", body); w.Code != http.StatusForbidden {
		t.Fatalf("someone else's room branch: %d", w.Code)
	}
}

// R50: the day's ledger is the budget's record, not a sum over live runs. Today's 5.1 M spent
// refuses an enforced cap and only counts in shadow; yesterday's larger ledger shows the name
// carries the date boundary.
func TestDailyBudget(t *testing.T) {
	today := ledgerCM("20260927", map[string]int64{"human:291": 5_100_000})
	yesterday := ledgerCM("20260926", map[string]int64{"human:291": 9_000_000})
	st := &store{}
	if w := post(server(t, st, false, today, yesterday), "alice", base()); w.Code != http.StatusCreated {
		t.Fatalf("in shadow the cap is counted, not enforced (R3): %d", w.Code)
	}
	if w := post(server(t, &store{}, true, today, yesterday), "alice", base()); w.Code != http.StatusTooManyRequests ||
		!strings.Contains(w.Body.String(), "over_budget") {
		t.Fatalf("over today's 5 M: %d", w.Code)
	}
}

// staleOnce serves its client's first ConfigMap read from a frozen snapshot: one replica's
// informer state before the other's reservation landed — R50's race, made deterministic.
type staleOnce struct {
	client.Client
	snap *corev1.ConfigMap
	used bool
}

func (c *staleOnce) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if cm, ok := obj.(*corev1.ConfigMap); ok && !c.used && key.Name == c.snap.Name {
		c.used = true
		c.snap.DeepCopyInto(cm)
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// R50: the list → check → create sequence is not atomic across replicas. Two servers share one
// apiserver and each first reads the ledger frozen at a pre-race version: the loser's Update
// conflicts, its re-read sees the winner's reservation, and exactly one slot is taken.
func TestTwoAdmissionsOneSlot(t *testing.T) {
	for name, c := range map[string]struct {
		room   bool
		daily  int64
		code   int
		reason string
	}{
		"one budget slot": {daily: 2_000_000, code: http.StatusTooManyRequests, reason: "over_budget"},
		"one room slot":   {room: true, daily: 25_000_000, code: http.StatusConflict, reason: "room_busy"},
	} {
		t.Run(name, func(t *testing.T) {
			var objs []client.Object
			ledger := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: LedgerPrefix + "20260927", Namespace: "agent-system"}}
			objs = append(objs, ledger)
			body := base()
			if c.room {
				objs = append(objs, aliceRoom())
				body["roomRef"] = "3kq7x2ma"
			}
			api := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(objs...).Build()
			var seed corev1.ConfigMap
			if err := api.Get(t.Context(), client.ObjectKey{Namespace: "agent-system", Name: ledger.Name}, &seed); err != nil {
				t.Fatal(err)
			}
			stA, stB := &store{}, &store{}
			sA := mkServer(t, stA, &staleOnce{Client: api, snap: seed.DeepCopy()}, true, false, "7f3cq2xz", c.daily)
			sB := mkServer(t, stB, &staleOnce{Client: api, snap: seed.DeepCopy()}, true, false, "bbbbbbbb", c.daily)
			hA, hB := sA.Handler(), sB.Handler()
			var wa, wb *httptest.ResponseRecorder
			var wg sync.WaitGroup
			start := make(chan struct{})
			wg.Add(2)
			go func() { defer wg.Done(); <-start; wa = post(hA, "alice", body) }()
			go func() { defer wg.Done(); <-start; wb = post(hB, "alice", body) }()
			close(start)
			wg.Wait()
			if (wa.Code == http.StatusCreated) == (wb.Code == http.StatusCreated) {
				t.Fatalf("exactly one 201: %d %s | %d %s", wa.Code, wa.Body, wb.Code, wb.Body)
			}
			loser := wb
			if wb.Code == http.StatusCreated {
				loser = wa
			}
			if loser.Code != c.code || !strings.Contains(loser.Body.String(), c.reason) {
				t.Fatalf("the loser: %d %s", loser.Code, loser.Body)
			}
			if len(stA.created)+len(stB.created) != 1 {
				t.Fatalf("one run created: %d + %d", len(stA.created), len(stB.created))
			}
		})
	}
}

// R50: deleting a run refunds nothing — the day's spend lives in the ledger, not in the sum of
// live runs. The ledger says the day is spent while the live list is empty.
func TestDeleteDoesNotRefund(t *testing.T) {
	l := ledgerCM("20260927", map[string]int64{"human:291": 5_000_000})
	st := &store{}
	if w := post(server(t, st, true, l), "alice", base()); w.Code != http.StatusTooManyRequests ||
		!strings.Contains(w.Body.String(), "over_budget") || len(st.created) != 0 {
		t.Fatalf("%d %s %d", w.Code, w.Body, len(st.created))
	}
}
