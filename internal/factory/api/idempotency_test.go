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

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/internal/factory/runs"
	"github.com/Smana/agent-platform/internal/factory/taskid"
)

// claims behaves like runs.Client for what the API reads: one claim per id, a replayed Create
// is a no-op, and List shows what was created.
type claims struct {
	mu   sync.Mutex
	made map[string]runs.Spec
	at   time.Time
}

func (c *claims) Create(_ context.Context, s runs.Spec) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.made == nil {
		c.made = map[string]runs.Spec{}
	}
	if _, ok := c.made[s.RunID]; !ok {
		c.made[s.RunID] = s
	}
	return nil
}

func (c *claims) List(context.Context) ([]runs.Run, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []runs.Run
	for id, s := range c.made {
		out = append(out, runs.Run{ID: id, Role: s.Role, Principal: s.Principal, Branch: s.Branch,
			RoomRef: s.RoomRef, MaxTokens: s.MaxTokens, RequestHash: s.RequestHash, Created: c.at, Phase: "Running"})
	}
	return out, nil
}

func idemServer(t *testing.T, c *claims, clock func() time.Time) http.Handler {
	t.Helper()
	cl := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(aliceRoom()).Build()
	s := mkServer(t, c, cl, true, false, "", 50_000_000)
	s.NewRunID = taskid.Random // production ids: the key, not a constant, must decide
	if clock != nil {
		s.Now = clock
	}
	return s.Handler()
}

func postKey(h http.Handler, tok, key string, body map[string]any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequestWithContext(context.Background(), "POST", "/v1/runs", bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+tok)
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRetryWithTheSameKeyIsTheSameRun(t *testing.T) {
	c := &claims{at: now}
	h := idemServer(t, c, nil)
	first := postKey(h, "alice", "room:cli:1", base())
	second := postKey(h, "alice", "room:cli:1", base())
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("%d %s / %d %s", first.Code, first.Body, second.Code, second.Body)
	}
	if first.Body.String() != second.Body.String() || len(c.made) != 1 {
		t.Fatalf("first %s second %s claims %d", first.Body, second.Body, len(c.made))
	}
}

func TestRetryTakesNoSecondReservation(t *testing.T) {
	c := &claims{at: now}
	cl := fake.NewClientBuilder().WithScheme(scheme()).Build()
	s := mkServer(t, c, cl, true, false, "", 50_000_000)
	s.NewRunID = taskid.Random
	h := s.Handler()
	postKey(h, "alice", "k1", base())
	postKey(h, "alice", "k1", base())
	used, err := DayUsed(context.Background(), cl, "agent-system", "human:291", now)
	if err != nil || used != 2_000_000 {
		t.Fatalf("used %d err %v: one reservation of the default 2M expected", used, err)
	}
}

func TestADifferentKeyIsAnotherRun(t *testing.T) {
	c := &claims{at: now}
	h := idemServer(t, c, nil)
	a := postKey(h, "alice", "k1", base())
	b := postKey(h, "alice", "k2", base())
	if a.Code != http.StatusCreated || b.Code != http.StatusCreated || a.Body.String() == b.Body.String() || len(c.made) != 2 {
		t.Fatalf("%d %s / %d %s claims %d", a.Code, a.Body, b.Code, b.Body, len(c.made))
	}
}

func TestSameKeyDifferentBodyIsRefused(t *testing.T) {
	c := &claims{at: now}
	h := idemServer(t, c, nil)
	postKey(h, "alice", "k1", base())
	other := base()
	other["task"] = map[string]any{"text": "Something else"}
	w := postKey(h, "alice", "k1", other)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "idempotency_key_reused") || len(c.made) != 1 {
		t.Fatalf("%d %s claims %d", w.Code, w.Body, len(c.made))
	}
}

// A room-bound retry would otherwise meet its own claim as room_busy.
func TestRoomBoundRetryIsNotRoomBusy(t *testing.T) {
	c := &claims{at: now}
	h := idemServer(t, c, nil)
	body := base()
	body["roomRef"] = "3kq7x2ma"
	first := postKey(h, "alice", "k1", body)
	second := postKey(h, "alice", "k1", body)
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated || first.Body.String() != second.Body.String() || len(c.made) != 1 {
		t.Fatalf("%d %s / %d %s", first.Code, first.Body, second.Code, second.Body)
	}
}

func TestTheKeyIsPerPrincipal(t *testing.T) {
	c := &claims{at: now}
	h := idemServer(t, c, nil)
	a := postKey(h, "alice", "k1", base())
	b := postKey(h, "admin", "k1", base())
	if a.Code != http.StatusCreated || b.Code != http.StatusCreated || len(c.made) != 2 {
		t.Fatalf("%d %d claims %d", a.Code, b.Code, len(c.made))
	}
}

func TestConcurrentDuplicatesMakeOneClaim(t *testing.T) {
	c := &claims{at: now}
	h := idemServer(t, c, nil)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	bodies := make([]string, 8)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := postKey(h, "alice", "k1", base())
			codes[i], bodies[i] = w.Code, w.Body.String()
		}()
	}
	wg.Wait()
	for i := range codes {
		if codes[i] == http.StatusCreated && bodies[i] != bodies[0] && codes[0] == http.StatusCreated {
			t.Fatalf("diverging runs: %s vs %s", bodies[0], bodies[i])
		}
	}
	if len(c.made) != 1 {
		t.Fatalf("claims %d, codes %v", len(c.made), codes)
	}
}

// The key lives a day: a retry within 24h finds the run even across midnight; after that the key
// is free again and makes a new run.
func TestKeyLifetimeIsTheLedgersDay(t *testing.T) {
	at := time.Date(2026, 9, 27, 23, 59, 0, 0, time.UTC)
	cur := at
	c := &claims{at: at}
	h := idemServer(t, c, func() time.Time { return cur })
	first := postKey(h, "alice", "k1", base())
	cur = at.Add(2 * time.Minute) // past midnight, inside the day
	again := postKey(h, "alice", "k1", base())
	if first.Body.String() != again.Body.String() || len(c.made) != 1 {
		t.Fatalf("%s vs %s claims %d", first.Body, again.Body, len(c.made))
	}
	cur = at.Add(25 * time.Hour)
	late := postKey(h, "alice", "k1", base())
	if late.Code != http.StatusCreated || late.Body.String() == first.Body.String() || len(c.made) != 2 {
		t.Fatalf("%d %s claims %d", late.Code, late.Body, len(c.made))
	}
}

// Run ids are public and 40 bits: a key ground to another principal's id must not replay their run.
func TestAnotherPrincipalsRunIsNeverReplayed(t *testing.T) {
	c := &claims{at: now}
	victim := keyedID(now, "human:291", "k1")
	_ = c.Create(context.Background(), runs.Spec{RunID: victim, Principal: "human:1", Branch: "agent/" + victim,
		RequestHash: requestHash(RunRequest{})})
	w := postKey(idemServer(t, c, nil), "alice", "k1", base())
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "idempotency_key_collision") ||
		strings.Contains(w.Body.String(), victim) || len(c.made) != 1 {
		t.Fatalf("%d %s claims %d", w.Code, w.Body, len(c.made))
	}
}

func TestRetryWithReorderedOrEmptyEgressReplays(t *testing.T) {
	c := &claims{at: now}
	h := idemServer(t, c, nil)
	a := base()
	a["egressProfiles"] = []string{"pypi", "npm"}
	b := base()
	b["egressProfiles"] = []string{"npm", "pypi"}
	first, second := postKey(h, "alice", "k1", a), postKey(h, "alice", "k1", b)
	if second.Code != http.StatusCreated || first.Body.String() != second.Body.String() || len(c.made) != 1 {
		t.Fatalf("%d %s claims %d", second.Code, second.Body, len(c.made))
	}
	e := base()
	e["egressProfiles"] = []string{}
	postKey(h, "alice", "k2", base())
	if w := postKey(h, "alice", "k2", e); w.Code != http.StatusCreated || len(c.made) != 2 {
		t.Fatalf("%d %s claims %d", w.Code, w.Body, len(c.made))
	}
}

func TestNoKeyStillMintsAFreshRun(t *testing.T) {
	c := &claims{at: now}
	h := idemServer(t, c, nil)
	postKey(h, "alice", "", base())
	postKey(h, "alice", "", base())
	if len(c.made) != 2 {
		t.Fatalf("claims %d", len(c.made))
	}
}
