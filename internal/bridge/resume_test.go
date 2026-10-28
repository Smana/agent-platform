// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/Smana/maku/internal/wire"
)

// A suspended run comes back as a new pod on the same PVC (v2 suspend, plan
// P6): agent-run reattaches the persisted conversation and sends it nothing,
// and agent-server reloads it paused, the SIGTERM path's /interrupt having
// fallen back to pause() with its action still unmatched. Only this bridge
// continues it, by answering that confirmation from the room's decision: a new
// user message would reject the call, and a bare /run would execute it.
func resumedRun(t *testing.T, sse ...string) (*fakeAgentServer, *fakeBroker, *rig) {
	t.Helper()
	f := &fakeAgentServer{pageSize: 100, status: statusPaused}
	f.add(chatEvent("on it"))
	f.add(actionEvent("c1", "gh pr create --fill"))
	f.add(map[string]any{"kind": "InterruptEvent", "source": "user"})
	f.add(map[string]any{"kind": "PauseEvent", "source": "user"})
	fb := &fakeBroker{sse: sse, resume: wire.Resume{AfterHarnessSeq: SeqFor(4, 0),
		Approvals: wire.ApprovalPolicy{Profile: "attended"}}}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	confirming(r).Suspendable = true
	return f, fb, r
}

func TestAResumedRunIsContinuedOnlyByTheRoomsDecision(t *testing.T) {
	cases := []struct {
		name   string
		frame  string
		writes []string // agent-server writes, in order
		reason string   // the answer's reason
	}{
		{"an approval accepts the reloaded call",
			"event: decision\ndata: {\"approvalId\":\"ap-c1\",\"allow\":true,\"ref\":42}\n\n",
			[]string{"respond true"}, ""},
		{"a decline rejects it, then lets the model read why",
			"event: decision\ndata: {\"approvalId\":\"ap-c1\",\"allow\":false,\"reason\":\"draft first\",\"ref\":42}\n\n",
			[]string{"respond false", "run"}, declined("draft first")},
		{"an expiry rejects it as a timeout",
			"event: decision\ndata: {\"approvalId\":\"ap-c1\",\"allow\":false,\"reason\":\"expired\",\"ref\":42}\n\n",
			[]string{"respond false", "run"}, textTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, fb, r := resumedRun(t, tc.frame)
			ctx, stop := r.run(t)
			eventually(ctx, t, "the decision is applied", func() bool {
				return slices.Contains(statusKinds(fb), "decision_applied 42 "+runID)
			})
			f.polls(ctx, t, 5)
			stop()
			if w := f.written(); !slices.Equal(w, tc.writes) {
				t.Fatalf("agent-server got %v, want %v: never a message, never a /run before the answer", w, tc.writes)
			}
			if a := f.answers(); len(a) != 1 || a[0] != tc.reason {
				t.Fatalf("answered %q", a)
			}
			fb.mu.Lock()
			defer fb.mu.Unlock()
			if len(fb.approvals) != 1 || fb.approvals[0].CallID != "c1" || fb.approvals[0].EventID == "" {
				t.Fatalf("the reloaded call re-asks its approval once, by its event id: %v", fb.approvals)
			}
		})
	}
}

func TestAResumedRunStillPendingWaits(t *testing.T) {
	f, fb, r := resumedRun(t)
	ctx, stop := r.run(t)
	eventually(ctx, t, "the approval is re-asked", func() bool {
		fb.mu.Lock()
		defer fb.mu.Unlock()
		return len(fb.approvals) == 1
	})
	f.polls(ctx, t, 10)
	stop()
	if w := f.written(); len(w) != 0 {
		t.Fatalf("a pending approval leaves the reloaded call waiting: agent-server got %v", w)
	}
}

// A paused step past its deadline is rejected like a waiting one: a broker
// that never delivers the expiry must not hold the resumed run forever.
func TestAPausedStepPastItsDeadlineIsRejected(t *testing.T) {
	c, f, ap, _ := setup(t, "attended")
	c.Suspendable, f.status = true, statusPaused
	ap.expires = time.Now().Add(-2 * decisionGrace)
	c.Observe(action("c1", "gh pr create --fill"))
	c.OnStatus(t.Context(), statusPaused)
	c.OnStatus(t.Context(), statusPaused)
	if a := f.answers(); len(a) != 1 || a[0] != textTimeout {
		t.Fatalf("answered %q", a)
	}
	if w := f.written(); !slices.Equal(w, []string{"respond false", "run"}) {
		t.Fatalf("agent-server got %v", w)
	}
}

// Outside a resumed conversation, paused is a human's interrupt: the loop
// leaves it paused, as a run that is not suspendable always does.
func TestOnlyAnInheritedPauseIsContinued(t *testing.T) {
	cases := []struct {
		name        string
		suspendable bool
		statuses    []string
		pending     bool
		writes      []string
	}{
		{"a run that is not suspendable", false, []string{statusPaused, statusPaused}, true, nil},
		{"a pause after the run moved", true, []string{statusRunning, statusPaused, statusPaused}, true, nil},
		{"suspended between steps: one /run continues it", true, []string{statusPaused, statusPaused, statusPaused}, false,
			[]string{"run"}},
		{"a pause after the inherited one was continued", true, []string{statusPaused, statusRunning, statusPaused}, false,
			[]string{"run"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f, ap, _ := setup(t, "attended")
			c.Suspendable = tc.suspendable
			if tc.pending {
				c.Observe(action("c1", "ls"))
			} else {
				f.matched = true // nothing is pending: a /run confirms nothing
			}
			for _, s := range tc.statuses {
				f.mu.Lock()
				f.status = s
				f.mu.Unlock()
				c.OnStatus(t.Context(), s)
			}
			if w := f.written(); !slices.Equal(w, tc.writes) || len(ap.requests()) != 0 {
				t.Fatalf("agent-server got %v, approvals %v", w, ap.requests())
			}
		})
	}
}

// A /run agent-server refused is asked again at the next paused status, and
// only until one is taken.
func TestARefusedInheritedRunIsRetried(t *testing.T) {
	c, f, _, _ := setup(t, "attended")
	c.Suspendable, f.status, f.matched, f.runCode = true, statusPaused, true, http.StatusServiceUnavailable
	c.OnStatus(t.Context(), statusPaused)
	f.mu.Lock()
	f.runCode = 0
	f.mu.Unlock()
	c.OnStatus(t.Context(), statusPaused)
	c.OnStatus(t.Context(), statusPaused)
	if w := f.written(); !slices.Equal(w, []string{"run", "run"}) {
		t.Fatalf("agent-server got %v", w)
	}
}

// A local allow on an inherited step is answered at once, as on a waiting one.
func TestAnInheritedAllowIsAnsweredLocally(t *testing.T) {
	c, f, ap, _ := setup(t, "attended")
	c.Suspendable, f.status = true, statusPaused
	c.Observe(action("c1", "ls"))
	c.OnStatus(t.Context(), statusPaused)
	if w := f.written(); !slices.Equal(w, []string{"respond true"}) || len(ap.requests()) != 0 {
		t.Fatalf("agent-server got %v, approvals %v", w, ap.requests())
	}
}
