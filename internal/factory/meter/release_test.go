// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/api"
	"github.com/Smana/agent-platform/internal/factory/runs"
	"github.com/Smana/agent-platform/internal/factory/taskid"
)

// reservedIDs reads a day ledger's reservations back.
func reservedIDs(t *testing.T, c client.Client, day string) map[string]bool {
	t.Helper()
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "agent-system", Name: api.LedgerPrefix + day}, &cm); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for k := range cm.Data {
		if id, ok := strings.CutPrefix(k, api.LedgerReserved); ok {
			ids[id] = true
		}
	}
	return ids
}

// switchable is a usage source an outage can break and mend.
type switchable struct {
	source
	down bool
}

func (s *switchable) RunUsage(ctx context.Context) (map[string]Usage, error) {
	if s.down {
		return nil, errors.New("vmsingle down")
	}
	return s.source.RunUsage(ctx)
}

// #54: a failed usage read is counted and stamps nothing, so admission under an enforced cap
// refuses once the stamp is older than BlindTicks periods; the first tick that meters again
// stamps it, reports when, and records what the runs spent past their cap meanwhile.
func TestABlindMeterIsSeenAndItsOvershootRecorded(t *testing.T) {
	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	clock := at
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{
		{ID: "aaaaaaaa", Phase: "Running", MaxTokens: 1000, Principal: "human:291", Created: at}}}
	src := &switchable{source: source{"aaaaaaaa": 100}}
	c := fake.NewClientBuilder().WithScheme(cmScheme()).Build()
	var errs, overshoot int64
	var succeeded []time.Time
	m := &Meter{Runs: st, Source: src, Ledger: c, Namespace: "agent-system", Every: 30 * time.Second,
		Now:          func() time.Time { return clock },
		OnUsageError: func(context.Context) { errs++ },
		OnSucceeded:  func(_ context.Context, at time.Time) { succeeded = append(succeeded, at) },
		OnOvershoot:  func(_ context.Context, n int64) { overshoot += n }}
	if err := m.Tick(t.Context()); err != nil || len(succeeded) != 1 || errs != 0 {
		t.Fatalf("a seeing tick: %v %v %d", err, succeeded, errs)
	}
	admit := func() error {
		_, err := api.Reserve(t.Context(), c, "agent-system", clock, "bbbbbbbb",
			api.Reservation{Principal: "human:291", MaxTokens: 1}, 5_000_000, 0, true)
		if rerr := api.Release(t.Context(), c, "agent-system", clock, "bbbbbbbb"); rerr != nil {
			t.Fatal(rerr)
		}
		return err
	}
	if err := admit(); err != nil {
		t.Fatalf("a seeing meter admits: %v", err)
	}
	src.down = true
	src.source["aaaaaaaa"] = 5000 // the run spends on, unmetered
	for range BlindTicks {
		clock = clock.Add(m.Every)
		if err := m.Tick(t.Context()); err == nil {
			t.Fatal("a blind tick fails")
		}
	}
	if errs != BlindTicks || len(succeeded) != 1 || len(st.patches) != 1 {
		t.Fatalf("counted, nothing written: %d errors, %v, %v", errs, succeeded, st.patches)
	}
	if err := admit(); err != nil {
		t.Fatalf("still within the blind interval: %v", err)
	}
	clock = clock.Add(time.Second)
	if err := admit(); !errors.Is(err, api.ErrMeterBlind) {
		t.Fatalf("past it, an enforced admission is refused: %v", err)
	}
	if _, err := api.Reserve(t.Context(), c, "agent-system", clock, "cccccccc",
		api.Reservation{Principal: "human:291", MaxTokens: 1}, 5_000_000, 0, false); err != nil {
		t.Fatalf("in shadow a blind meter holds nothing: %v", err)
	}
	src.down = false
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p := st.patches["aaaaaaaa"]; p[runs.AnnRevoked] != ReasonBudgetRun || overshoot != 4000 {
		t.Fatalf("revoked on recovery, 4000 past the cap: %v %d", p, overshoot)
	}
	if len(succeeded) != 2 || !succeeded[1].Equal(clock) {
		t.Fatalf("the success is reported: %v", succeeded)
	}
	if err := admit(); err != nil {
		t.Fatalf("seeing again: %v", err)
	}
}

// #55: occupancy is not usage. A blind meter still releases a run that ended.
func TestABlindMeterStillReleases(t *testing.T) {
	day := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	entry := `{"principal":"human:291","room":"3kq7x2ma","maxTokens":2000000}`
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{
		{ID: "dddddddd", Phase: "Succeeded", Principal: "human:291", Created: day}}}
	c := fake.NewClientBuilder().WithScheme(cmScheme()).WithObjects(
		ledgerDay(t, "20260927", nil, map[string]string{"dddddddd": entry})).Build()
	m := &Meter{Runs: st, Source: broken{}, Ledger: c, Namespace: "agent-system", Now: func() time.Time { return day }}
	if err := m.Tick(t.Context()); err == nil {
		t.Fatal("the usage read failed")
	}
	if ids := reservedIDs(t, c, "20260927"); ids["dddddddd"] {
		t.Fatalf("released: %v", ids)
	}
}

// #55: a claim deleted while live keeps its phase while its finalizers run, and never reaches
// the terminal one: Deleting releases it.
func TestADeletingClaimLosesItsReservation(t *testing.T) {
	day := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	entry := `{"principal":"system:factory","maxTokens":1500000}`
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{
		{ID: "aaaaaaaa", Phase: "Running", Principal: "system:factory", Created: day, Deleting: true},
		{ID: "bbbbbbbb", Phase: "Running", Principal: "system:factory", Created: day}}}
	c := fake.NewClientBuilder().WithScheme(cmScheme()).WithObjects(
		ledgerDay(t, "20260927", nil, map[string]string{"aaaaaaaa": entry, "bbbbbbbb": entry})).Build()
	m := &Meter{Runs: st, Source: source{}, Ledger: c, Namespace: "agent-system", Now: func() time.Time { return day }}
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ids := reservedIDs(t, c, "20260927"); ids["aaaaaaaa"] || !ids["bbbbbbbb"] {
		t.Fatalf("the deleting run's released, the live one's kept: %v", ids)
	}
}

// #55: a reservation no claim holds — its claim deleted and gone before any tick saw it end, or
// never created (Queued reserved, then the task stopped) — is released once it has stayed
// unclaimed for orphanGrace, the time an admission may take between its reservation and its
// claim. A claim that appears meanwhile keeps it; a new leader starts the grace again.
func TestAnUnclaimedReservationIsReleasedAfterTheGrace(t *testing.T) {
	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	clock := at
	entry := `{"principal":"system:factory","maxTokens":1500000}`
	st := &store{patches: map[string]map[string]string{}}
	c := fake.NewClientBuilder().WithScheme(cmScheme()).WithObjects(
		ledgerDay(t, "20260927", nil, map[string]string{"aaaaaaaa": entry, "bbbbbbbb": entry})).Build()
	newMeter := func() *Meter {
		return &Meter{Runs: st, Source: source{}, Ledger: c, Namespace: "agent-system", Now: func() time.Time { return clock }}
	}
	tick := func(m *Meter) map[string]bool {
		t.Helper()
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		return reservedIDs(t, c, "20260927")
	}
	m := newMeter()
	if ids := tick(m); !ids["aaaaaaaa"] || !ids["bbbbbbbb"] {
		t.Fatalf("first sight keeps both: %v", ids)
	}
	clock = at.Add(orphanGrace / 2)
	st.runs = []runs.Run{{ID: "bbbbbbbb", Phase: "Pending", Principal: "system:factory", Created: clock}} // its claim lands
	if ids := tick(m); !ids["aaaaaaaa"] || !ids["bbbbbbbb"] {
		t.Fatalf("within the grace: %v", ids)
	}
	m = newMeter() // a failover: the new leader has seen nothing yet
	clock = at.Add(orphanGrace)
	if ids := tick(m); !ids["aaaaaaaa"] {
		t.Fatalf("a new leader starts the grace again: %v", ids)
	}
	clock = clock.Add(orphanGrace)
	if ids := tick(m); ids["aaaaaaaa"] || !ids["bbbbbbbb"] {
		t.Fatalf("the unclaimed one is released, the claimed one kept: %v", ids)
	}
	if ids := tick(m); ids["aaaaaaaa"] || !ids["bbbbbbbb"] {
		t.Fatalf("a second sweep changes nothing: %v", ids)
	}
}

func taskScheme() *runtime.Scheme {
	s := cmScheme()
	_ = v1alpha1.AddToScheme(s)
	return s
}

func taskIn(name, phase string, runs int) *v1alpha1.Task {
	tk := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agent-system"}}
	tk.Status.Phase = phase
	for i := range runs {
		tk.Status.Runs = append(tk.Status.Runs, v1alpha1.RunRecord{ID: taskid.Run(name, i)})
	}
	return tk
}

// #87 review: Queued reserves its next run, then may wait minutes without a claim (the room log,
// the broker's permission, a forge error), and its idempotent re-reserve refreshes nothing. The
// meter never releases a live task's next run: the task's end does. Another unclaimed entry —
// an ended task's — still goes after the grace.
func TestAQueuedTasksNextRunKeepsItsReservation(t *testing.T) {
	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	clock := at
	queued, ended := taskid.Run("3buqdlot", 1), taskid.Run("4buqdlot", 0)
	entry := `{"principal":"system:factory","maxTokens":1500000}`
	c := fake.NewClientBuilder().WithScheme(taskScheme()).WithObjects(
		ledgerDay(t, "20260927", nil, map[string]string{queued: entry, ended: entry}),
		taskIn("3buqdlot", v1alpha1.PhaseQueued, 1), taskIn("4buqdlot", v1alpha1.PhaseStopped, 0)).Build()
	st := &store{patches: map[string]map[string]string{}}
	m := &Meter{Runs: st, Source: source{}, Ledger: c, Tasks: c, Namespace: "agent-system",
		Now: func() time.Time { return clock }}
	for d := time.Duration(0); d <= 10*time.Minute; d += time.Minute { // minutes of waiting_room_log
		clock = at.Add(d)
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if ids := reservedIDs(t, c, "20260927"); !ids[queued] || ids[ended] {
		t.Fatalf("the queued task's run kept, the ended task's released: %v", ids)
	}
	st.runs = []runs.Run{{ID: queued, Phase: "Pending", Principal: runs.PrincipalFactory, Created: clock}} // startRun
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ids := reservedIDs(t, c, "20260927"); !ids[queued] {
		t.Fatalf("the run starts reserved: %v", ids)
	}
}

// A task list the meter cannot read releases nothing unclaimed that tick.
func TestAnUnreadTaskListReleasesNoUnclaimedReservation(t *testing.T) {
	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	clock := at
	c := fake.NewClientBuilder().WithScheme(cmScheme()).WithObjects( // no Task kind: every list fails
		ledgerDay(t, "20260927", nil, map[string]string{"aaaaaaaa": `{"principal":"system:factory","maxTokens":1}`})).Build()
	m := &Meter{Runs: &store{patches: map[string]map[string]string{}}, Source: source{}, Ledger: c, Tasks: c,
		Namespace: "agent-system", Now: func() time.Time { return clock }}
	for _, d := range []time.Duration{0, orphanGrace} {
		clock = at.Add(d)
		if err := m.Tick(t.Context()); err == nil {
			t.Fatal("the task list error is reported")
		}
	}
	if ids := reservedIDs(t, c, "20260927"); !ids["aaaaaaaa"] {
		t.Fatalf("kept: %v", ids)
	}
}

// #87 review: the success gauge says what admission reads. A heartbeat that could not be stamped
// is no success, so the gauge cannot look healthy beside a 503 meter_blind.
func TestAnUnstampedTickIsNoSuccess(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(cmScheme()).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			if o.GetName() == api.MeterHeartbeat {
				return errors.New("apiserver down")
			}
			return cl.Create(ctx, o, opts...)
		}}).Build()
	var succeeded int
	m := &Meter{Runs: &store{patches: map[string]map[string]string{}}, Source: source{}, Ledger: c, Namespace: "agent-system",
		Every: 30 * time.Second, OnSucceeded: func(context.Context, time.Time) { succeeded++ }}
	if err := m.Tick(t.Context()); err == nil || succeeded != 0 {
		t.Fatalf("an unstamped tick fails and reports no success: %v %d", err, succeeded)
	}
	m.Ledger = fake.NewClientBuilder().WithScheme(cmScheme()).Build()
	if err := m.Tick(t.Context()); err != nil || succeeded != 1 {
		t.Fatalf("stamped: %v %d", err, succeeded)
	}
}
