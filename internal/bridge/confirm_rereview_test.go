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
		{waiting, []string{"send", "respond false", "run"}},
		{"paused", []string{"send", "respond false", "run"}},
		{"running", []string{"send"}}, // the live loop takes the message (re-review 2 M8)
		{"idle", []string{"send", "run"}},
		{"finished", []string{"send", "run"}},
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
	t.Run("an unreadable status skips the run", func(t *testing.T) {
		c, f, _, _ := setup(t, "attended")
		f.statusCode = http.StatusServiceUnavailable
		steer := &Steering{Harness: c.Harness, RunID: runID, Push: c.Push, Gate: c.Gate}
		if err := steer.Deliver(t.Context(), wire.Deliver{Ref: 5, Text: "hold on"}); err != nil {
			t.Fatalf("the message is in; replaying it would repeat it: %v", err)
		}
		if w := f.written(); !slices.Equal(w, []string{"send"}) {
			t.Fatalf("writes %v", w)
		}
	})
	t.Run("a refused send is replayed", func(t *testing.T) {
		c, f, _, _ := setup(t, "attended")
		if err := c.Gate(t.Context(), func(bool) error { return http.ErrHandlerTimeout }); err == nil {
			t.Fatal("nothing is in: the delivery must be replayed")
		}
		if w := f.written(); len(w) != 0 {
			t.Fatalf("writes %v", w)
		}
	})
}

// Ruling SAW: the step parks on a confirmation right after the gate read the
// status as running. A message sent before the read turns that park into a
// rejection, so nothing is accepted, and the message still reaches the run.
func TestTheGateClosesTheParkRace(t *testing.T) {
	c, f, _, _ := setup(t, "attended")
	f.status, f.parkAfterRead = "running", true
	steer := &Steering{Harness: c.Harness, RunID: runID, Push: c.Push, Gate: c.Gate}
	if err := steer.Deliver(t.Context(), wire.Deliver{Ref: 5, Text: "hold on"}); err != nil {
		t.Fatal(err)
	}
	w := f.written()
	if slices.Contains(w, "implicit accept") || !slices.Equal(w, []string{"send", "rejected by the message"}) {
		t.Fatalf("writes %v: a step parked after the read must never be accepted", w)
	}
	if sent, _, _ := f.snapshot(); !slices.Equal(sent, []string{"hold on"}) {
		t.Fatalf("the message reaches the conversation: %v", sent)
	}
}

// Without a confirmation loop (phase 4), steering runs the message itself.
func TestUngatedSteeringRunsTheMessage(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: "idle"}
	steer := &Steering{Harness: NewHarness(f.start(t, conv).URL, conv), RunID: runID, Push: func(wire.Item) {}}
	if err := steer.Deliver(t.Context(), wire.Deliver{Ref: 1, Text: "go"}); err != nil {
		t.Fatal(err)
	}
	if w := f.written(); !slices.Equal(w, []string{"send run"}) {
		t.Fatalf("writes %v", w)
	}
}

// Re-review 2 I1, probe P3: the gate's /run is taken but its answer is lost,
// and the agent parks within that window. The loop's status, read before the
// gate wrote, must not drive a retry over the waiting step.
func TestAGateRunLostOnTheWayBackIsNeverRetriedOverAPark(t *testing.T) {
	c, f, _, _ := setup(t, "attended")
	f.status, f.runTakenCode = "idle", http.StatusGatewayTimeout
	steer := &Steering{Harness: c.Harness, RunID: runID, Push: c.Push, Gate: c.Gate}
	if err := steer.Deliver(t.Context(), wire.Deliver{Ref: 5, Text: "go on"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.status, f.runTakenCode = waiting, 0 // the agent ran, then parked
	f.mu.Unlock()
	c.OnStatus(t.Context(), "idle") // read before the gate wrote
	c.OnStatus(t.Context(), waiting)
	if w := f.written(); slices.Contains(w, "implicit accept") || !slices.Equal(w, []string{"send", "run"}) {
		t.Fatalf("writes %v", w)
	}
}

// Re-review 2 M9: a status the gate could not read leaves the run to the
// resume retry, from the next fresh idle status.
func TestAnUnreadStatusLeavesTheRunToTheRetry(t *testing.T) {
	c, f, _, _ := setup(t, "attended")
	f.status, f.statusCode = "idle", http.StatusServiceUnavailable
	steer := &Steering{Harness: c.Harness, RunID: runID, Push: c.Push, Gate: c.Gate}
	if err := steer.Deliver(t.Context(), wire.Deliver{Ref: 5, Text: "go on"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.statusCode = 0
	f.mu.Unlock()
	c.OnStatus(t.Context(), "idle") // read before the gate wrote: skipped
	if w := f.written(); !slices.Equal(w, []string{"send"}) {
		t.Fatalf("writes %v", w)
	}
	c.OnStatus(t.Context(), "idle")
	if w := f.written(); !slices.Equal(w, []string{"send", "run"}) {
		t.Fatalf("the message is run from a fresh idle status: %v", w)
	}
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
	if w := f.written(); !slices.Equal(w, []string{"respond false", "run", "send", "run"}) {
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
		if err := c.Gate(t.Context(), func(bool) error { return nil }); err != nil {
			t.Fatal(err)
		}
		c.OnStatus(t.Context(), "idle")
		if w := f.written(); !slices.Equal(w, []string{"respond false", "run", "run"}) {
			t.Fatalf("only the gate's own run: %v", w)
		}
	})
	t.Run("a failed send keeps it", func(t *testing.T) {
		c, f := retrying(t)
		_ = c.Gate(t.Context(), func(bool) error { return http.ErrHandlerTimeout })
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
	if err := c.Gate(t.Context(), func(bool) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, k := range kinds(*items) {
		if k == "decision_applied 77 "+runID {
			t.Fatalf("acknowledged a frame that named no approval: %v", kinds(*items))
		}
	}
}
