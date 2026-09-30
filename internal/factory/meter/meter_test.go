// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

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
