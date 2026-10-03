// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/internal/factory/api"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

type store struct {
	mu      sync.Mutex
	runs    []runs.Run
	patches map[string]map[string]string
	fail    map[string]bool
	listErr error
}

func (s *store) List(context.Context) ([]runs.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]runs.Run(nil), s.runs...), s.listErr
}

func (s *store) Annotate(_ context.Context, id string, kv map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail[id] {
		return errors.New("conflict")
	}
	s.patches[id] = kv
	return nil
}

type source map[string]int64

func (s source) RunTokens(context.Context) (map[string]int64, error) { return s, nil }

type broken struct{}

func (broken) RunTokens(context.Context) (map[string]int64, error) {
	return nil, errors.New("vmsingle down")
}

func TestTick(t *testing.T) {
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{
		{ID: "aaaaaaaa", Phase: "Running", MaxTokens: 1000, Tokens: 100},                         // at its cap
		{ID: "bbbbbbbb", Phase: "Running", MaxTokens: 1000, Tokens: 100, Principal: "human:291"}, // a human's run, under cap
		{ID: "cccccccc", Phase: "Succeeded", MaxTokens: 1000, Tokens: 100},                       // finished: usage only
		{ID: "dddddddd", Phase: "Running", MaxTokens: 1000, Tokens: 900},                         // reading dropped (pod restart)
		{ID: "eeeeeeee", Phase: "Running", MaxTokens: 1000, Tokens: 1200, Revoked: "budget-run"}, // already revoked
		{ID: "ffffffff", Phase: "Running", MaxTokens: 0, Tokens: 10},                             // no cap: never revoked
		{ID: "gggggggg", Phase: "Running", MaxTokens: 1000, Tokens: 10},                          // no reading this tick
		{ID: "hhhhhhhh", Phase: "Running", MaxTokens: 1000, Tokens: 10},                          // one under its cap
	}}
	var revoked []string
	m := &Meter{Runs: st, Source: source{"aaaaaaaa": 1000, "bbbbbbbb": 400, "cccccccc": 5000, "dddddddd": 500,
		"eeeeeeee": 1300, "ffffffff": 9_000_000, "hhhhhhhh": 999},
		OnRevoke: func(_ context.Context, r string) { revoked = append(revoked, r) }}
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p := st.patches["aaaaaaaa"]; p[runs.AnnUsage] != "1000" || p[runs.AnnRevoked] != "budget-run" {
		t.Errorf("at cap: %v", p)
	}
	if p := st.patches["bbbbbbbb"]; p[runs.AnnUsage] != "400" || p[runs.AnnRevoked] != "" {
		t.Errorf("a human's run is metered too: %v", p)
	}
	if p := st.patches["cccccccc"]; p[runs.AnnUsage] != "5000" || p[runs.AnnRevoked] != "" {
		t.Errorf("a finished run gets its final usage and no revocation: %v", p)
	}
	if _, ok := st.patches["dddddddd"]; ok {
		t.Error("a lower reading is never written")
	}
	if p := st.patches["eeeeeeee"]; p[runs.AnnRevoked] != "" || p[runs.AnnUsage] != "1300" {
		t.Errorf("revoked once, still metered: %v", p)
	}
	if p := st.patches["ffffffff"]; p[runs.AnnRevoked] != "" || p[runs.AnnUsage] != "9000000" {
		t.Errorf("a run with no cap is metered, never revoked: %v", p)
	}
	if _, ok := st.patches["gggggggg"]; ok {
		t.Error("no reading, nothing written")
	}
	if p := st.patches["hhhhhhhh"]; p[runs.AnnUsage] != "999" || p[runs.AnnRevoked] != "" {
		t.Errorf("under its cap: %v", p)
	}
	if len(revoked) != 1 || revoked[0] != "budget-run" {
		t.Errorf("revocations %v", revoked)
	}
}

// R12: a data-plane restart zeroes the counter mid-run. The raw reading would give the run a fresh
// budget; the total keeps growing from its high-water mark.
func TestACounterResetNeverResetsTheCap(t *testing.T) {
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{
		{ID: "aaaaaaaa", Phase: "Running", MaxTokens: 1000}}}
	src := source{}
	m := &Meter{Runs: st, Source: src}
	tick := func(raw int64) map[string]string {
		src["aaaaaaaa"] = raw
		st.patches = map[string]map[string]string{}
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		p := st.patches["aaaaaaaa"]
		if u := p[runs.AnnUsage]; u != "" { // the composition projects it (SP1)
			st.runs[0].Tokens, _ = strconv.ParseInt(u, 10, 64)
		}
		return p
	}
	if p := tick(800); p[runs.AnnUsage] != "800" || p[runs.AnnRevoked] != "" {
		t.Fatalf("first reading: %v", p)
	}
	if p := tick(100); len(p) != 0 {
		t.Fatalf("a reset writes nothing and revokes nothing: %v", p)
	}
	if p := tick(150); p[runs.AnnUsage] != "850" {
		t.Fatalf("800 before the reset + 50 after: %v", p)
	}
	if p := tick(350); p[runs.AnnUsage] != "1050" || p[runs.AnnRevoked] != "budget-run" {
		t.Fatalf("800 before the reset + 250 after is past the cap of 1000: %v", p)
	}
	m2 := &Meter{Runs: st, Source: source{"aaaaaaaa": 400}} // a factory failover: no memory
	st.runs[0].Revoked = ""
	st.patches = map[string]map[string]string{}
	if err := m2.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p := st.patches["aaaaaaaa"]; p[runs.AnnUsage] != "" || p[runs.AnnRevoked] != "budget-run" {
		t.Fatalf("a new meter starts from the annotation, never from the lower raw value: %v", p)
	}
}

// R12: until the composition projects usage-tokens, the meter's own last total is the floor. A
// reset in that window neither lowers what is written nor gives the run budget back.
func TestTheMetersMemoryIsTheFloorWhileTheAnnotationLags(t *testing.T) {
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{{ID: "aaaaaaaa", Phase: "Running", MaxTokens: 1000}}}
	src := source{}
	m := &Meter{Runs: st, Source: src}
	for _, c := range []struct {
		raw   int64
		usage string
	}{{800, "800"}, {100, "800"}, {150, "850"}} {
		src["aaaaaaaa"] = c.raw
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := st.patches["aaaaaaaa"][runs.AnnUsage]; got != c.usage {
			t.Fatalf("raw %d: usage %q, want %q (the annotation never projected)", c.raw, got, c.usage)
		}
	}
}

// A run already at its cap is revoked even on a tick with no reading: a lost series is no reprieve.
func TestARunAtItsCapIsRevokedWithoutAReading(t *testing.T) {
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{{ID: "aaaaaaaa", Phase: "Running", MaxTokens: 1000, Tokens: 1000}}}
	if err := (&Meter{Runs: st, Source: source{}}).Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p := st.patches["aaaaaaaa"]; p[runs.AnnRevoked] != "budget-run" || p[runs.AnnUsage] != "" {
		t.Fatalf("%v", p)
	}
}

// A run that is gone is forgotten: its id never comes back (C2), and the memory stays bounded.
func TestTheMeterForgetsRunsThatAreGone(t *testing.T) {
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{{ID: "aaaaaaaa", Phase: "Running"}, {ID: "bbbbbbbb", Phase: "Running"}}}
	m := &Meter{Runs: st, Source: source{"aaaaaaaa": 10, "bbbbbbbb": 10}}
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	st.runs = st.runs[1:]
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.last["aaaaaaaa"]; ok || len(m.last) != 1 {
		t.Fatalf("remembers %v", m.last)
	}
}

func TestTickFailsWithoutWritingOnABadRead(t *testing.T) {
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{{ID: "aaaaaaaa", Phase: "Running", MaxTokens: 1}}}
	if err := (&Meter{Runs: st, Source: broken{}}).Tick(t.Context()); err == nil || len(st.patches) != 0 {
		t.Fatalf("%v %v", err, st.patches)
	}
	st.listErr = errors.New("api down")
	if err := (&Meter{Runs: st, Source: source{"aaaaaaaa": 5}}).Tick(t.Context()); err == nil || len(st.patches) != 0 {
		t.Fatalf("%v %v", err, st.patches)
	}
}

// A failed annotation is reported, is not a revocation, and does not stop the other runs.
func TestAFailedAnnotationIsNotARevocation(t *testing.T) {
	st := &store{patches: map[string]map[string]string{}, fail: map[string]bool{"aaaaaaaa": true}, runs: []runs.Run{
		{ID: "aaaaaaaa", Phase: "Running", MaxTokens: 10}, {ID: "bbbbbbbb", Phase: "Running", MaxTokens: 10}}}
	var revoked int
	m := &Meter{Runs: st, Source: source{"aaaaaaaa": 20, "bbbbbbbb": 20}, OnRevoke: func(context.Context, string) { revoked++ }}
	if err := m.Tick(t.Context()); err == nil {
		t.Fatal("the failure is reported")
	}
	if revoked != 1 || st.patches["bbbbbbbb"][runs.AnnRevoked] != "budget-run" {
		t.Fatalf("%d %v", revoked, st.patches)
	}
}

// signalling reports each read on reads, so a test can wait for a tick instead of sleeping.
type signalling struct {
	reads chan int64
	next  chan int64
}

func (s signalling) RunTokens(context.Context) (map[string]int64, error) {
	v := <-s.next
	s.reads <- v
	return map[string]int64{"aaaaaaaa": v}, nil
}

func TestStartTicksAtOnceThenEveryPeriodUntilCancelled(t *testing.T) {
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{{ID: "aaaaaaaa", Phase: "Running"}}}
	src := signalling{reads: make(chan int64, 2), next: make(chan int64, 2)}
	ticks := make(chan time.Time)
	stopped := make(chan struct{})
	m := &Meter{Runs: st, Source: src, Every: time.Second,
		Ticker: func(d time.Duration) (<-chan time.Time, func()) {
			if d != time.Second {
				t.Errorf("ticker %s", d)
			}
			return ticks, func() { close(stopped) }
		}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- m.Start(ctx) }()
	wait := func(want int64) {
		t.Helper()
		select {
		case got := <-src.reads:
			if got != want {
				t.Fatalf("read %d, want %d", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no tick read %d", want)
		}
	}
	src.next <- 1
	wait(1) // the first tick needs no ticker value
	src.next <- 2
	ticks <- time.Time{}
	wait(2)
	ticks <- time.Time{} // received only once the second tick is done
	src.next <- 3
	wait(3)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("the ticker is stopped")
	}
	if !m.NeedLeaderElection() {
		t.Fatal("one meter writes: the leader's")
	}
}

func TestStartRefusesNoPeriod(t *testing.T) {
	if err := (&Meter{Runs: &store{}, Source: source{}}).Start(t.Context()); err == nil {
		t.Fatal("a zero period would tick forever or panic")
	}
}

type throttled map[string]bool

func (t throttled) RecentlyThrottled(context.Context) (map[string]bool, error) { return t, nil }

type badThrottle struct{}

func (badThrottle) RecentlyThrottled(context.Context) (map[string]bool, error) {
	return nil, errors.New("victorialogs down")
}

// cmScheme is the fake client's scheme: the meter's ledger work touches ConfigMaps only.
func cmScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}

// ledgerDay is one day's R50 ledger as meter reads it: the spent column and, in reserved,
// whole entry values keyed by run id.
func ledgerDay(t *testing.T, day string, spent map[string]int64, reserved map[string]string) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: api.LedgerPrefix + day, Namespace: "agent-system"}}
	data := map[string]string{}
	if spent != nil {
		b, err := json.Marshal(spent)
		if err != nil {
			t.Fatal(err)
		}
		data[api.LedgerSpent] = string(b)
	}
	for id, v := range reserved {
		data[api.LedgerReserved+id] = v
	}
	if len(data) > 0 {
		cm.Data = data
	}
	return cm
}

// spentOf reads a day ledger back; found is false once the ledger never landed.
func spentOf(t *testing.T, c client.Client, day string) (map[string]int64, bool) {
	t.Helper()
	var cm corev1.ConfigMap
	err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "agent-system", Name: api.LedgerPrefix + day}, &cm)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := cm.Data[api.LedgerSpent]
	if !ok || raw == "" {
		return nil, true
	}
	var m map[string]int64
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("spent column %q: %v", raw, err)
	}
	return m, true
}

// R50/R07: the day's spend comes from the ledger the meter maintains, never from a re-sum of
// live runs. The ledger already holds the day's earlier spend (what the run annotations record);
// this tick adds only the increase, and budget-principal fires when the day is spent.
func TestGateway429sAndThePrincipalsDay(t *testing.T) {
	day := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{
		{ID: "aaaaaaaa", Phase: "Running", MaxTokens: 5_000_000, Tokens: 100, Principal: "system:factory", Created: day}, // 429 below B1
		{ID: "bbbbbbbb", Phase: "Running", MaxTokens: 5_000_000, Tokens: 100, Principal: "system:factory", Created: day}, // 429 at B1
		{ID: "cccccccc", Phase: "Running", MaxTokens: 5_000_000, Tokens: 100, Principal: "human:291", Created: day},      // her day is spent
		{ID: "dddddddd", Phase: "Succeeded", MaxTokens: 5_000_000, Tokens: 4_900_000, Principal: "human:291", Created: day},
	}}
	c := fake.NewClientBuilder().WithScheme(cmScheme()).WithObjects(
		ledgerDay(t, "20260927", map[string]int64{"system:factory": 200, "human:291": 4_900_100}, nil)).Build()
	remaining := map[string]int64{}
	m := &Meter{Runs: st, Source: source{"aaaaaaaa": 200, "bbbbbbbb": 5_000_000, "cccccccc": 200_000, "dddddddd": 4_900_000},
		Throttle: throttled{"aaaaaaaa": true, "bbbbbbbb": true}, B1Ceiling: 5_000_000,
		Budgets: config.Budgets{EnforcePrincipal: true, FactoryDaily: 25_000_000, HumanDaily: 5_000_000},
		Ledger:  c, Namespace: "agent-system",
		Now: func() time.Time { return day }, Remaining: func(p string, n, _ int64) { remaining[p] = n }}
	if err := m.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"aaaaaaaa": "budget-fleet", "bbbbbbbb": "budget-run", "cccccccc": "budget-principal"} {
		if got := st.patches[id][runs.AnnRevoked]; got != want {
			t.Errorf("%s: %q, want %q", id, got, want)
		}
	}
	if remaining["human:291"] != 0 || remaining["system:factory"] != 25_000_000-5_000_200 {
		t.Fatalf("%v", remaining)
	}
	// The ledger now holds the full day: the seeded 5_000_100 plus the tick's increase.
	spent, _ := spentOf(t, c, "20260927")
	if spent["human:291"] != 5_100_000 || spent["system:factory"] != 5_000_200 {
		t.Fatalf("ledger %v", spent)
	}
}

// A lost 429 lookup costs only the fleet mapping: a run at its own cap is still revoked.
func TestAFailedThrottleLookupKeepsTheOtherCauses(t *testing.T) {
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{
		{ID: "aaaaaaaa", Phase: "Running", MaxTokens: 10, Tokens: 0},
		{ID: "bbbbbbbb", Phase: "Running", MaxTokens: 1000, Tokens: 0},
	}}
	m := &Meter{Runs: st, Source: source{"aaaaaaaa": 20, "bbbbbbbb": 20}, Throttle: badThrottle{}}
	if err := m.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.patches["aaaaaaaa"][runs.AnnRevoked] != "budget-run" {
		t.Errorf("own cap stands without the lookup: %v", st.patches["aaaaaaaa"])
	}
	if st.patches["bbbbbbbb"][runs.AnnRevoked] != "" {
		t.Errorf("budget-fleet needs the lookup: %v", st.patches["bbbbbbbb"])
	}
}

// R07: the day is when the increase is observed, not when the run began. A run straddling
// midnight is billed to each day's ledger for only the spend of that day, and its principal's
// budget-principal revocation reads the day it is in.
func TestMidnightCrossingSplitsSpend(t *testing.T) {
	start := time.Date(2026, 9, 27, 23, 0, 0, 0, time.UTC)
	now := start
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{
		{ID: "aaaaaaaa", Phase: "Running", Principal: "human:291", Created: start}}}
	src := source{"aaaaaaaa": 100}
	c := fake.NewClientBuilder().WithScheme(cmScheme()).Build()
	m := &Meter{Runs: st, Source: src, Ledger: c, Namespace: "agent-system",
		Budgets: config.Budgets{EnforcePrincipal: true, HumanDaily: 120},
		Now:     func() time.Time { return now }}
	if err := m.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if spent, _ := spentOf(t, c, "20260927"); spent["human:291"] != 100 {
		t.Fatalf("day one: %v", spent)
	}
	if st.patches["aaaaaaaa"][runs.AnnRevoked] != "" {
		t.Fatalf("100 under a cap of 120 is no revocation: %v", st.patches["aaaaaaaa"])
	}
	now = start.Add(2 * time.Hour) // 2026-09-28: the same run, a new ledger
	src["aaaaaaaa"] = 250
	if err := m.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if spent, _ := spentOf(t, c, "20260927"); spent["human:291"] != 100 {
		t.Fatalf("yesterday's ledger never moves: %v", spent)
	}
	// 150, not 250: the day counts the increase observed in it, not the run's lifetime total.
	if spent, _ := spentOf(t, c, "20260928"); spent["human:291"] != 150 {
		t.Fatalf("today: %v", spent)
	}
	if st.patches["aaaaaaaa"][runs.AnnRevoked] != "budget-principal" {
		t.Fatalf("her new day is spent at 150 over 120: %v", st.patches["aaaaaaaa"])
	}
}

// R50: the meter is the only dropper of a reservation once its run is terminal; a live run's
// reservation stays.
func TestTerminalRunLosesItsReservation(t *testing.T) {
	day := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	entry := `{"principal":"human:291","maxTokens":2000000}`
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{
		{ID: "aaaaaaaa", Phase: "Running", Principal: "human:291", Created: day},
		{ID: "dddddddd", Phase: "Succeeded", Tokens: 1000, Principal: "human:291", Created: day},
	}}
	c := fake.NewClientBuilder().WithScheme(cmScheme()).WithObjects(
		ledgerDay(t, "20260927", nil, map[string]string{"aaaaaaaa": entry, "dddddddd": entry})).Build()
	m := &Meter{Runs: st, Source: source{"aaaaaaaa": 10}, Ledger: c, Namespace: "agent-system",
		Now: func() time.Time { return day }}
	if err := m.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "agent-system", Name: api.LedgerPrefix + "20260927"}, &cm); err != nil {
		t.Fatal(err)
	}
	if _, ok := cm.Data[api.LedgerReserved+"dddddddd"]; ok {
		t.Error("the terminal run's reservation is dropped")
	}
	if _, ok := cm.Data[api.LedgerReserved+"aaaaaaaa"]; !ok {
		t.Error("a live run keeps its reservation")
	}
}

// R50: a day's ledger outlives its budget but not long: a month plus a week of margin.
func TestLedgersOlderThan35DaysAreDeleted(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{
		{ID: "aaaaaaaa", Phase: "Running", Principal: "human:291", Created: now}}}
	c := fake.NewClientBuilder().WithScheme(cmScheme()).WithObjects(
		ledgerDay(t, "20260822", map[string]int64{"human:291": 1}, nil), // 36 days: gone
		ledgerDay(t, "20260823", map[string]int64{"human:291": 1}, nil), // 35 days: kept
		ledgerDay(t, "20260927", nil, nil),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: api.LedgerPrefix + "20251301", Namespace: "agent-system"}}, // not a day
	).Build()
	m := &Meter{Runs: st, Source: source{}, Ledger: c, Namespace: "agent-system",
		Now: func() time.Time { return now }}
	if err := m.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		day  string
		want bool
	}{{"20260822", false}, {"20260823", true}, {"20260927", true}, {"20251301", true}} {
		var cm corev1.ConfigMap
		err := c.Get(context.Background(),
			types.NamespacedName{Namespace: "agent-system", Name: api.LedgerPrefix + tc.day}, &cm)
		if got := err == nil; got != tc.want {
			t.Errorf("ledger %s exists = %v, want %v", tc.day, got, tc.want)
		}
	}
}

// R13: agent-router's 429s come back keyed by run id, read from the verified identity header.
func TestRecentlyThrottled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/select/logsql/query" || r.URL.Query().Get("query") == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("query") == "boom" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprintln(w, `{"log.x_ar_agent":"system:serviceaccount:agents:xplane-run-7f3cq2xz","hits":"3"}`)
		_, _ = fmt.Fprintln(w, `{"log.x_ar_agent":"system:serviceaccount:agents:xplane-run-notarun!","hits":"1"}`)
		_, _ = fmt.Fprintln(w, `{"hits":"9"}`)
		_, _ = fmt.Fprintln(w, `not json`)
	}))
	defer srv.Close()
	ctx := context.Background()
	got, err := VL{URL: srv.URL, Query: "q", HC: srv.Client()}.RecentlyThrottled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got["7f3cq2xz"] {
		t.Fatalf("%v", got)
	}
	if _, err := (VL{URL: srv.URL, Query: "boom", HC: srv.Client()}).RecentlyThrottled(ctx); err == nil {
		t.Error("a failed query is an error, not an empty map")
	}
	if _, err := (VL{URL: srv.URL, Query: "q"}).RecentlyThrottled(ctx); err == nil {
		t.Error("the audited httpx client is required, never a hand-rolled one")
	}
}
