// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"net/http"
	"slices"
	"testing"

	"github.com/Smana/agent-platform/internal/wire"
)

// Re-review C1, probe P1: the bridge's view lags the harness, so steering asks
// the harness. A step the loop has not read yet is rejected all the same.
func TestSteeringAsksTheHarnessNotTheBridgesView(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   []string
	}{
		{waiting, []string{"respond false", "send"}},
		{"paused", []string{"respond false", "send"}},
		{"running", []string{"send"}}, // the live step turns a message into a rejection itself
		{"idle", []string{"send"}},
		{"finished", []string{"send"}},
	} {
		t.Run(tc.status, func(t *testing.T) {
			c, f, _, _ := setup(t, "attended")
			f.status = tc.status
			steer := &Steering{Harness: c.Harness, RunID: runID, Push: c.Push, Gate: c.Gate}
			// Nothing observed: the loop has not read the harness's step yet.
			if err := steer.Deliver(t.Context(), wire.Deliver{Ref: 5, Text: "hold on"}); err != nil {
				t.Fatal(err)
			}
			if w := f.written(); !slices.Equal(w, tc.want) {
				t.Fatalf("writes %v, want %v", w, tc.want)
			}
		})
	}
	t.Run("an unreadable status sends nothing", func(t *testing.T) {
		c, f, _, _ := setup(t, "attended")
		f.statusCode = http.StatusServiceUnavailable
		steer := &Steering{Harness: c.Harness, RunID: runID, Push: c.Push, Gate: c.Gate}
		if err := steer.Deliver(t.Context(), wire.Deliver{Ref: 5, Text: "hold on"}); err == nil {
			t.Fatal("the stream must replay the delivery")
		}
		if w := f.written(); len(w) != 0 {
			t.Fatalf("writes %v", w)
		}
	})
}

// Re-review I1, probe P2: a failed resume is never retried over a step that
// steering resumed and that now waits.
func TestAResumeRetryNeverConfirmsANewStep(t *testing.T) {
	c, f, _, _ := setup(t, "unattended")
	steer := &Steering{Harness: c.Harness, RunID: runID, Push: c.Push, Gate: c.Gate}
	f.runCode = http.StatusInternalServerError
	c.Observe(action("c1", "gh pr merge 1"))
	c.OnStatus(t.Context(), waiting) // reject, then /run fails
	if err := steer.Deliver(t.Context(), wire.Deliver{Ref: 5, Text: "try again"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.status, f.runCode = waiting, 0 // the agent's next step waits
	f.mu.Unlock()
	c.OnStatus(t.Context(), waiting)
	if w := f.written(); !slices.Equal(w, []string{"respond false", "run", "send"}) {
		t.Fatalf("writes %v: a /run now would confirm the new step", w)
	}
}

func TestTheResumeRetryWaitsForAnIdleStatus(t *testing.T) {
	retrying := func(t *testing.T) (*Confirmer, *fakeAgentServer) {
		c, f, _, _ := setup(t, "unattended")
		f.runCode = http.StatusInternalServerError
		c.Observe(action("c1", "gh pr merge 1"))
		c.OnStatus(t.Context(), waiting)
		f.mu.Lock()
		f.runCode = 0
		f.mu.Unlock()
		return c, f
	}
	t.Run("idle retries", func(t *testing.T) {
		c, f := retrying(t)
		c.OnStatus(t.Context(), "idle")
		if w := f.written(); !slices.Equal(w, []string{"respond false", "run", "run"}) {
			t.Fatalf("writes %v", w)
		}
	})
	for _, s := range []string{waiting, "running", "paused", "finished"} {
		t.Run(s+" gives up the retry for good", func(t *testing.T) {
			c, f := retrying(t)
			c.OnStatus(t.Context(), s)
			c.OnStatus(t.Context(), "idle")
			if w := f.written(); slices.Contains(w[2:], "run") {
				t.Fatalf("writes %v", w)
			}
		})
	}
	t.Run("a stale status neither retries nor gives up", func(t *testing.T) {
		c, f, _, _ := setup(t, "attended")
		c.Observe(action("c1", "gh pr create --fill"))
		c.OnStatus(t.Context(), waiting)
		f.mu.Lock()
		f.runCode = http.StatusInternalServerError
		f.mu.Unlock()
		_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c1", Reason: "no", Ref: 2}) // reject, /run fails
		f.mu.Lock()
		f.runCode = 0
		f.mu.Unlock()
		c.OnStatus(t.Context(), waiting) // read before the rejection
		if w := f.written(); !slices.Equal(w, []string{"respond false", "run"}) {
			t.Fatalf("writes %v", w)
		}
		c.OnStatus(t.Context(), "idle")
		if w := f.written(); !slices.Equal(w, []string{"respond false", "run", "run"}) {
			t.Fatalf("the retry survives the stale status: %v", w)
		}
	})
	t.Run("a steering send ends the retry", func(t *testing.T) {
		c, f := retrying(t)
		if err := c.Gate(t.Context(), func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		c.OnStatus(t.Context(), "idle")
		if w := f.written(); slices.Contains(w[2:], "run") {
			t.Fatalf("writes %v", w)
		}
	})
	t.Run("a failed send keeps it", func(t *testing.T) {
		c, f := retrying(t)
		_ = c.Gate(t.Context(), func() error { return http.ErrHandlerTimeout })
		c.OnStatus(t.Context(), "idle")
		if w := f.written(); !slices.Equal(w, []string{"respond false", "run", "run"}) {
			t.Fatalf("writes %v", w)
		}
	})
}

// Re-review nit: a decision naming no approval is never held, so it can never
// be acknowledged against an action not asked yet.
func TestADecisionWithoutAnApprovalIDIsDropped(t *testing.T) {
	c, _, _, items := setup(t, "attended")
	_ = c.Decision(t.Context(), wire.Decision{Allow: true, Ref: 77})
	if len(c.decided) != 0 {
		t.Fatalf("held %v", c.decided)
	}
	c.Observe(action("c1", "ls"))
	if err := c.Gate(t.Context(), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, k := range kinds(*items) {
		if k == "decision_applied 77 "+runID {
			t.Fatalf("acknowledged a frame that named no approval: %v", kinds(*items))
		}
	}
}
