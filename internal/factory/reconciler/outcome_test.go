// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/killswitch"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

func TestStuckRunEscalatesAndMentions(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	g.reconcile(t, "3buqdlot", 3)
	g.runs.set(rid(0), "Running")
	g.log.evs = append(g.log.evs, envelope.Event{Seq: 1, RunID: rid(0), Type: envelope.ToolCall, TS: now})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseImplementing {
		t.Fatal("fresh activity")
	}
	g.r.Now = func() time.Time { return now.Add(11 * time.Minute) }
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != "stuck" || g.runs.patches[rid(0)][runs.AnnRevoked] != "manual" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
	if c := g.f.Comments(7); !strings.Contains(c[len(c)-1], "@Smana") {
		t.Fatalf("an escalation mentions the maintainers (§6.3): %q", c[len(c)-1])
	}
}

func TestControlIssueEngagesTheStop(t *testing.T) {
	defer killswitch.SetIssue(false)
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	killswitch.SetIssue(true)
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseStopped || tk.Status.Reason != "kill_switch" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

func TestFitAndScores(t *testing.T) {
	done := func(tokens, budget int64) *v1alpha1.Task {
		return &v1alpha1.Task{Spec: v1alpha1.TaskSpec{Budget: v1alpha1.Budget{Tier: "standard", TaskTokens: budget}},
			Status: v1alpha1.TaskStatus{Phase: v1alpha1.PhaseDone, Usage: v1alpha1.Usage{Tokens: tokens}}}
	}
	if Fit(done(100_000, 3_000_000)) != "over" || Fit(done(2_000_000, 3_000_000)) != "fit" {
		t.Fatal("over below 20 % of the task budget, else fit (§7)")
	}
	under := &v1alpha1.Task{Status: v1alpha1.TaskStatus{Phase: v1alpha1.PhaseEscalated, Reason: "review_rounds_exhausted"}}
	if Fit(under) != "under" || Fit(&v1alpha1.Task{Status: v1alpha1.TaskStatus{Phase: v1alpha1.PhaseStopped}}) != "" {
		t.Fatal("under after a full attempt; a stop scores nothing")
	}
	if Fit(&v1alpha1.Task{Status: v1alpha1.TaskStatus{Phase: v1alpha1.PhaseEscalated, Reason: "agent_error"}}) != "" {
		t.Fatal("agent_error may be the gateway's: unscored (R53)")
	}
	cl := v1alpha1.Classification{Classifier: "semantic-router", Tier: "standard",
		Shadow: []v1alpha1.ShadowVerdict{{Classifier: "jev", Tier: "frontier"}, {Classifier: "other", Tier: "light"}}}
	got := map[string]string{}
	for _, s := range Score(cl, "standard", "under") { // the right tier was frontier
		got[s.Classifier] = s.Fit
	}
	if got["semantic-router"] != "under" || got["jev"] != "fit" || got["other"] != "under" {
		t.Fatalf("%v", got)
	}
}

func TestTaskFinalLinksTheAuditChain(t *testing.T) {
	var buf bytes.Buffer
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	g.r.Log = slog.New(slog.NewJSONHandler(&buf, nil))
	g.reconcile(t, "3buqdlot", 3)
	g.f.SetBranch("agent/3buqdlot", 12)
	g.f.SetPR(forge.PR{Number: 12, URL: "https://github.com/Smana/cloud-native-ref/pull/12", State: "OPEN"})
	g.runs.set(rid(0), "Succeeded")
	g.log.end(rid(0), "Succeeded", "agent_finished")
	g.reconcile(t, "3buqdlot", 1)
	g.f.SetPR(forge.PR{Number: 12, State: "MERGED", MergedBy: "Smana"})
	g.reconcile(t, "3buqdlot", 1)
	var final map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "task.final" {
			final = m
		}
	}
	if final == nil {
		t.Fatal("no task.final line: " + buf.String())
	}
	for _, k := range []string{"task.id", "key", "contentSHA256", "classifier", "runIds", "room", "pr", "mergedBy", "phase"} {
		if final[k] == nil || final[k] == "" {
			t.Errorf("task.final lacks %s: %v", k, final)
		}
	}
}
