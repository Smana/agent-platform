// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/wire"
)

// Review C1: a message sent with run:true confirms every pending action, so
// steering rejects a waiting step first, and sends only once that is taken.
func TestSteeringNeverConfirmsAWaitingStep(t *testing.T) {
	t.Run("a waiting step is rejected before the message", func(t *testing.T) {
		c, f, _, _ := setup(t, "attended")
		steer := &Steering{Harness: c.Harness, RunID: runID, Push: c.Push, Gate: c.Gate}
		c.Observe(action("c1", "gh pr create --fill"))
		c.OnStatus(t.Context(), waiting)
		if err := steer.Deliver(t.Context(), wire.Deliver{Ref: 5, Text: "hold on"}); err != nil {
			t.Fatal(err)
		}
		if w := f.written(); !slices.Equal(w, []string{"send", "respond false", "run"}) || f.answers()[0] != textSuperseded {
			t.Fatalf("writes %v, answers %v: in without running, rejected, then run", w, f.answers())
		}
		_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c1", Allow: true, Ref: 9})
		c.Observe(action("c2", "ls"))
		c.OnStatus(t.Context(), waiting) // stale: read before the rejection
		if w := f.written(); len(w) != 3 {
			t.Fatalf("a late decision, or a status read before the rejection, answers nothing: %v", w)
		}
		c.OnStatus(t.Context(), waiting)
		if w := f.written(); !slices.Equal(w, []string{"send", "respond false", "run", "respond true"}) {
			t.Fatalf("the next step is answered on its own: %v", w)
		}
	})
	t.Run("an interrupted step is rejected before the message", func(t *testing.T) {
		c, f, _, _ := setup(t, "attended")
		steer := &Steering{Harness: c.Harness, RunID: runID, Push: c.Push, Gate: c.Gate}
		c.Observe(action("c1", "gh pr create --fill"))
		c.OnStatus(t.Context(), waiting)
		_ = steer.Interrupt(t.Context(), wire.Interrupt{Ref: 4})
		_ = steer.Deliver(t.Context(), wire.Deliver{Ref: 5, Text: "go on"})
		if w := f.written(); !slices.Equal(w, []string{"interrupt", "send", "respond false", "run"}) {
			t.Fatalf("writes %v", w)
		}
	})
	t.Run("decisions received are acknowledged with the rejection", func(t *testing.T) {
		c, _, _, items := setup(t, "attended")
		c.Observe(action("c1", "gh pr create --fill"))
		c.Observe(action("c2", "gh issue close 4"))
		c.OnStatus(t.Context(), waiting)
		_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c1", Allow: true, Ref: 7})
		if err := c.Gate(t.Context(), func(bool) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if k := kinds(*items); !slices.Equal(k, []string{"decision_applied 7 " + runID}) {
			t.Fatalf("logged %v", k)
		}
	})
	t.Run("no waiting step, no rejection", func(t *testing.T) {
		c, f, _, _ := setup(t, "attended")
		steer := &Steering{Harness: c.Harness, RunID: runID, Push: c.Push, Gate: c.Gate}
		c.Observe(action("c1", "ls"))
		c.OnStatus(t.Context(), waiting)
		_ = steer.Deliver(t.Context(), wire.Deliver{Ref: 5, Text: "thanks"})
		if w := f.written(); !slices.Equal(w, []string{"respond true", "send", "run"}) { // 409: running
			t.Fatalf("writes %v", w)
		}
	})
	t.Run("a rejection the harness refuses skips the run, and leaves the step to the loop", func(t *testing.T) {
		c, f, _, _ := setup(t, "attended")
		steer := &Steering{Harness: c.Harness, RunID: runID, Push: c.Push, Gate: c.Gate}
		c.Observe(action("c1", "gh pr create --fill"))
		c.OnStatus(t.Context(), waiting)
		f.mu.Lock()
		f.refuse = http.StatusServiceUnavailable
		f.mu.Unlock()
		if err := steer.Deliver(t.Context(), wire.Deliver{Ref: 5, Text: "hold on"}); err != nil {
			t.Fatalf("the message is in; replaying it would repeat it: %v", err)
		}
		if w := f.written(); !slices.Equal(w, []string{"send"}) {
			t.Fatalf("writes %v", w)
		}
	})
}

// Review I1: OpenHands parks a rejected conversation idle; the bridge resumes
// it, so the model reads the rejection's guidance.
func TestARejectedConversationIsResumed(t *testing.T) {
	t.Run("a deny resumes", func(t *testing.T) {
		c, f, _, _ := setup(t, "unattended")
		c.Observe(action("c1", "gh pr merge 1"))
		c.OnStatus(t.Context(), waiting)
		if w := f.written(); !slices.Equal(w, []string{"respond false", "run"}) {
			t.Fatalf("writes %v", w)
		}
	})
	t.Run("an accept does not", func(t *testing.T) {
		c, f, _, _ := setup(t, "unattended")
		c.Observe(action("c1", "ls"))
		c.OnStatus(t.Context(), waiting)
		if w := f.written(); !slices.Equal(w, []string{"respond true"}) {
			t.Fatalf("writes %v", w)
		}
	})
	t.Run("a decline resumes", func(t *testing.T) {
		c, f, _, _ := setup(t, "attended")
		c.Observe(action("c1", "gh pr create --fill"))
		c.OnStatus(t.Context(), waiting)
		_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c1", Reason: "no"})
		if w := f.written(); !slices.Equal(w, []string{"respond false", "run"}) {
			t.Fatalf("writes %v", w)
		}
	})
	t.Run("a failed resume is retried at the next status; a run in progress is resumed", func(t *testing.T) {
		c, f, _, _ := setup(t, "unattended")
		f.runCode = http.StatusServiceUnavailable
		c.Observe(action("c1", "gh pr merge 1"))
		c.OnStatus(t.Context(), waiting)
		f.mu.Lock()
		f.runCode, f.status = 0, "running"
		f.mu.Unlock()
		c.OnStatus(t.Context(), "idle") // 409: already running, done
		c.OnStatus(t.Context(), "idle")
		if w := f.written(); !slices.Equal(w, []string{"respond false", "run", "run"}) {
			t.Fatalf("writes %v", w)
		}
	})
}

func actionWithID(id, call, cmd string) RawEvent {
	var e RawEvent
	b, _ := json.Marshal(map[string]any{"id": id, "kind": "ActionEvent", "tool_name": "terminal", "tool_call_id": call,
		"action": map[string]string{"command": cmd}})
	_ = json.Unmarshal(b, &e)
	return e
}

func TestAnActionWithoutArgumentsIsShownAsAnEmptyDocument(t *testing.T) {
	c, _, ap, _ := setup(t, "attended")
	var e RawEvent
	_ = json.Unmarshal([]byte(`{"id":"e7","kind":"ActionEvent","tool_name":"github__create_issue","tool_call_id":"c7"}`), &e)
	c.Observe(e)
	c.OnStatus(t.Context(), waiting)
	if r := ap.requests(); len(r) != 1 || r[0].Class != "mcp.write" || string(r[0].Action) != "{}" || r[0].EventID != "e7" {
		t.Fatalf("%+v", r)
	}
}

// Review I2: tool call ids come from the model provider and may repeat.
func TestARepeatedCallIDNeverReusesADecision(t *testing.T) {
	c, f, ap, _ := setup(t, "attended")
	c.Observe(actionWithID("e1", "c1", "gh pr create --fill"))
	c.OnStatus(t.Context(), waiting)
	_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c1", Allow: true, Ref: 3})
	_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c1", Allow: true, Ref: 3}) // replayed
	if len(c.decided) != 0 {
		t.Fatalf("a decision for a spent approval is not held: %v", c.decided)
	}
	// e1 has no result yet; a new action reuses its call id, and a broker keyed
	// on the call id answers the spent approval again.
	c.Observe(actionWithID("e2", "c1", "gh pr edit 3 --title x"))
	if len(c.pending) != 2 {
		t.Fatalf("both actions are tracked: %d", len(c.pending))
	}
	c.OnStatus(t.Context(), waiting) // stale after the decision's answer
	c.OnStatus(t.Context(), waiting)
	if r := f.responded(); len(r) != 2 || r[1] || f.answers()[1] != textBrokerDown {
		t.Fatalf("a spent approval never approves a new action: %v %v", r, f.answers())
	}
	if r := ap.requests(); len(r) != 2 || r[0].EventID != "e1" || r[1].EventID != "e2" || r[1].CallID != "c1" {
		t.Fatalf("requests are keyed on the event: %+v", r)
	}
	// A result settles the oldest action with the call id.
	var res RawEvent
	_ = json.Unmarshal([]byte(`{"id":"r1","kind":"ObservationEvent","tool_call_id":"c1"}`), &res)
	c.Observe(res)
	if len(c.pending) != 1 || c.pending["e2"] == nil {
		t.Fatalf("pending %v", c.pending)
	}
	for i := range maxSpent + 1 {
		c.mu.Lock()
		c.spend(fmt.Sprint("s", i))
		c.mu.Unlock()
	}
	if len(c.spent) != maxSpent || c.spent["s0"] || !c.spent[fmt.Sprint("s", maxSpent)] {
		t.Fatalf("the spent set is bounded, oldest out: %d", len(c.spent))
	}
}

// Review M1 (mutant R1): a replayed decision for an answered action whose
// result never came must not settle the next, half-read step.
func TestAReplayForAnAnsweredActionSettlesNothing(t *testing.T) {
	c, f, _, _ := setup(t, "attended")
	c.Observe(action("c1", "gh pr create --fill"))
	c.OnStatus(t.Context(), waiting)
	_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c1", Allow: true, Ref: 3})
	c.Observe(action("c2", "ls")) // the next step, read so far: all allow
	_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c1", Allow: true, Ref: 3})
	if r := f.responded(); len(r) != 1 {
		t.Fatalf("answered a step the bridge has not finished reading: %v", r)
	}
}

func TestOneStepOneDeadline(t *testing.T) {
	t.Run("all asks of a step share one budget (M3)", func(t *testing.T) {
		c, _, _, _ := setup(t, "attended")
		var deadlines []time.Time
		c.Broker = approvalsFunc(func(ctx context.Context, r wire.ApprovalRequest) (wire.ApprovalAck, int, error) {
			d, _ := ctx.Deadline()
			deadlines = append(deadlines, d)
			return wire.ApprovalAck{ApprovalID: "ap-" + r.CallID}, 200, nil
		})
		start := time.Now()
		c.Observe(action("c1", "gh pr create --fill"))
		c.Observe(action("c2", "gh issue close 4"))
		c.OnStatus(t.Context(), waiting)
		if len(deadlines) != 2 || deadlines[0].IsZero() || !deadlines[0].Equal(deadlines[1]) ||
			deadlines[0].After(start.Add(askBudget+time.Second)) {
			t.Fatalf("deadlines %v", deadlines)
		}
	})
	t.Run("the earliest expiry decides the step", func(t *testing.T) {
		c, f, _, _ := setup(t, "attended")
		now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		c.Now = func() time.Time { return now }
		c.Broker = approvalsFunc(func(_ context.Context, r wire.ApprovalRequest) (wire.ApprovalAck, int, error) {
			exp := map[string]time.Duration{"c1": time.Hour, "c2": 10 * time.Minute}[r.CallID]
			return wire.ApprovalAck{ApprovalID: "ap-" + r.CallID, ExpiresAt: now.Add(exp)}, 200, nil
		})
		c.Observe(action("c1", "gh pr create --fill"))
		c.Observe(action("c2", "gh issue close 4"))
		c.OnStatus(t.Context(), waiting)
		now = now.Add(10*time.Minute + decisionGrace)
		c.OnStatus(t.Context(), waiting)
		if r := f.responded(); len(r) != 1 || r[0] {
			t.Fatalf("%v", r)
		}
	})
	t.Run("the next step starts without a deadline", func(t *testing.T) {
		c, f, ap, _ := setup(t, "attended")
		now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		c.Now = func() time.Time { return now }
		ap.expires = now.Add(time.Minute)
		c.Observe(action("c1", "gh pr create --fill"))
		c.OnStatus(t.Context(), waiting)
		_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c1", Allow: true})
		now = now.Add(time.Hour)
		ap.expires = now.Add(time.Hour)
		c.Observe(action("c2", "gh issue close 4"))
		c.OnStatus(t.Context(), waiting) // stale
		c.OnStatus(t.Context(), waiting)
		if r := f.responded(); len(r) != 1 {
			t.Fatalf("the old deadline leaked into the new step: %v", r)
		}
	})
	t.Run("a step with nothing pending clears it", func(t *testing.T) {
		c, _, _, _ := setup(t, "attended")
		c.deadline = time.Now()
		c.OnStatus(t.Context(), waiting)
		if !c.deadline.IsZero() {
			t.Fatal("deadline kept")
		}
	})
}

func TestRejectedStepsLogWhatHappened(t *testing.T) {
	t.Run("allows of a declined step are not logged (M4)", func(t *testing.T) {
		c, _, _, items := setup(t, "attended")
		c.Observe(action("c1", "git push origin agent/3kq7x2ma"))
		c.Observe(action("c2", "gh pr create --fill"))
		c.OnStatus(t.Context(), waiting)
		_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c2", Reason: "no", Ref: 8})
		if k := kinds(*items); !slices.Equal(k, []string{"decision_applied 8 " + runID}) {
			t.Fatalf("logged %v", k)
		}
	})
	t.Run("an expiry says no one decided (M5)", func(t *testing.T) {
		c, f, _, _ := setup(t, "attended")
		c.Observe(action("c1", "gh pr create --fill"))
		c.OnStatus(t.Context(), waiting)
		_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c1", Reason: reasonExpired, Ref: 8})
		if a := f.answers(); len(a) != 1 || a[0] != textTimeout {
			t.Fatalf("%v", a)
		}
	})
	t.Run("a timeout acknowledges the decisions received (M6)", func(t *testing.T) {
		c, _, ap, items := setup(t, "attended")
		now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		c.Now = func() time.Time { return now }
		ap.expires = now.Add(time.Minute)
		c.Observe(action("c1", "gh pr create --fill"))
		c.Observe(action("c2", "gh issue close 4"))
		c.OnStatus(t.Context(), waiting)
		_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c1", Allow: true, Ref: 8})
		now = now.Add(time.Hour)
		c.OnStatus(t.Context(), waiting)
		if k := kinds(*items); !slices.Equal(k, []string{"decision_applied 8 " + runID}) {
			t.Fatalf("logged %v", k)
		}
	})
}

// Review C2: confirmation mode never waits for the broker.
func TestAlwaysConfirmDoesNotWaitForTheBroker(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: "running"}
	hellos := make([]reply, 2000)
	for i := range hellos {
		hellos[i] = reply{code: http.StatusServiceUnavailable, reason: wire.ReasonLogUnavailable}
	}
	fb := &fakeBroker{hellos: hellos}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	confirming(r)
	ctx, stop := r.run(t)
	eventually(ctx, t, "AlwaysConfirm is set", func() bool { p, _ := f.policySet(); return p == "AlwaysConfirm" })
	stop()
	if slices.Contains(fb.callLog(), "hello OK") {
		t.Fatalf("the broker answered hello first: %v", fb.callLog())
	}
}
