// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/internal/factory/api"
	"github.com/Smana/agent-platform/internal/factory/runs"
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

func (s *switchable) RunTokens(ctx context.Context) (map[string]int64, error) {
	if s.down {
		return nil, errors.New("vmsingle down")
	}
	return s.source.RunTokens(ctx)
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
			api.Reservation{Principal: "human:291", MaxTokens: 1}, 5_000_000, true)
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
		api.Reservation{Principal: "human:291", MaxTokens: 1}, 5_000_000, false); err != nil {
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
