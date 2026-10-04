// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/config"
)

func investigateRig(t *testing.T) *rig {
	tk := issueTask("3buqdlot", 7, "# image-gallery crash-loops")
	tk.Spec.Source.Kind, tk.Spec.DataClass = "runlore", "internal"
	g := newRig(t, tk)
	g.r.Cfg.Templates["investigate"] = config.Template{Roles: []string{"triager"}}
	g.r.Triage = staticWith("investigate")
	return g
}

// R38: the triager proposes public text; no implementer ever starts from an internal task.
func TestInvestigateEndsOnAProposal(t *testing.T) {
	g := investigateRig(t)
	tkk := g.reconcile(t, "3buqdlot", 3)
	s := g.runs.specs[rid(0)]
	if tkk.Status.Phase != v1alpha1.PhaseImplementing || s.Role != "triager" || s.DataClass != "internal" ||
		!strings.Contains(s.TaskText, "public issue") {
		t.Fatalf("%s %+v", tkk.Status.Phase, s)
	}
	g.log.evs = append(g.log.evs, envelope.Event{Seq: int64(len(g.log.evs) + 1), RunID: rid(0), Type: envelope.Handoff,
		Actor:   envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:" + rid(0)},
		Payload: envelope.Must(envelope.HandoffPayload{FromRole: "triager", ToRole: "implementer", Summary: "Rename S3_BUCKET back in the chart values."})})
	g.runs.set(rid(0), "Succeeded")
	g.log.end(rid(0), "Succeeded", "agent_finished")
	tkk = g.reconcile(t, "3buqdlot", 1)
	if tkk.Status.Phase != v1alpha1.PhaseDone || tkk.Status.Reason != "proposal_ready" || len(g.runs.specs) != 1 {
		t.Fatalf("%s %s %d runs", tkk.Status.Phase, tkk.Status.Reason, len(g.runs.specs))
	}
	c := strings.Join(g.f.Comments(7), "\n")
	if !strings.Contains(c, "/r/") || !strings.Contains(c, "factory/ready") || strings.Contains(c, "S3_BUCKET") {
		t.Fatalf("the room link and the next step, never the proposal: %q", c)
	}
}

func TestTriagerWithoutHandoffIsNoOp(t *testing.T) {
	g := investigateRig(t)
	g.reconcile(t, "3buqdlot", 3)
	g.runs.set(rid(0), "Succeeded")
	g.log.end(rid(0), "Succeeded", "agent_finished")
	if got := g.reconcile(t, "3buqdlot", 1); got.Status.Phase != v1alpha1.PhaseNoOp || got.Status.Reason != "no_action" {
		t.Fatalf("%s %s", got.Status.Phase, got.Status.Reason)
	}
}

// R38: a retry of an escalated triager task runs the triager again: no implementer run ever
// starts from an internal-origin task.
func TestTriagerRetryRunsTheTriagerAgain(t *testing.T) {
	g := investigateRig(t)
	g.reconcile(t, "3buqdlot", 3)
	g.runs.set(rid(0), "Failed")
	g.log.end(rid(0), "Failed", "agent_error")
	if got := g.reconcile(t, "3buqdlot", 1); got.Status.Phase != v1alpha1.PhaseEscalated {
		t.Fatalf("%s", got.Status.Phase)
	}
	// What escalated() does for a maintainer's /factory retry: back to Queued with the trigger,
	// runs kept.
	var tk v1alpha1.Task
	if err := g.c.Get(t.Context(), types.NamespacedName{Namespace: "agent-system", Name: "3buqdlot"}, &tk); err != nil {
		t.Fatal(err)
	}
	tk.Status.Phase, tk.Status.NextTrigger, tk.Status.Reason = v1alpha1.PhaseQueued, "retry", ""
	if err := g.c.Status().Update(t.Context(), &tk); err != nil {
		t.Fatal(err)
	}
	tkk := g.reconcile(t, "3buqdlot", 1)
	s := g.runs.specs[rid(1)]
	if tkk.Status.Phase != v1alpha1.PhaseImplementing || s.Role != "triager" || s.DataClass != "internal" {
		t.Fatalf("%s %+v", tkk.Status.Phase, s)
	}
}
