// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/forge"
	"github.com/Smana/maku/internal/factory/triage"
)

// c7 is triage answering tier as C7 would, the control group forcing frontier: classification.tier
// stays C7's own verdict (OD-14).
type c7 struct {
	cfg     *config.Config
	tier    string
	control bool
}

func (c c7) Triage(_ context.Context, t *v1alpha1.Task) (triage.Decision, error) {
	acting := c.tier
	if c.control {
		acting = "frontier"
	}
	return triage.Decision{Template: triage.Matrix("review", "issue", acting), PredictedClass: "review", DataClass: t.Spec.DataClass,
		Budget:         triage.BudgetFor(c.cfg, acting),
		Classification: v1alpha1.Classification{Tier: c.tier, Classifier: "c7", Fallback: "none", Control: c.control}}, nil
}

// parentText is the parent issue as labelled: the whole task, with its own acceptance section.
const parentText = "# Fix both links to runbook 07\n\n## Acceptance check\n- the whole issue's check"

// splitRigWith is the rig with the host's USD tiers, loops.review {scope: scoper, split:
// {maxChildren: 3}} and C7 answering tier; edit shapes the task before its triage.
func splitRigWith(t *testing.T, tier string, control bool, edit func(*v1alpha1.Task), objs ...client.Object) *rig {
	t.Helper()
	tk := issueTask("3buqdlot", 7, parentText)
	if edit != nil {
		edit(tk)
	}
	g := newRig(t, append([]client.Object{tk}, objs...)...)
	c := g.r.Cfg
	c.Tiers["light"] = config.Tier{Model: "agent-default", RunTokens: 300_000, TaskTokens: 600_000, RunMinutes: 20, RunUSD: 0.51, TaskUSD: 1.02}
	for name, usd := range map[string][2]float64{"standard": {2.55, 5.10}, "frontier": {6.80, 13.60}} {
		tr := c.Tiers[name]
		tr.RunUSD, tr.TaskUSD = usd[0], usd[1]
		c.Tiers[name] = tr
	}
	c.Loops = map[string]config.Loop{"review": {Scope: config.ScopeScoper, Split: &config.SplitPolicy{MaxChildren: 3}}}
	g.r.Triage = c7{cfg: c, tier: tier, control: control}
	return g
}

func splitRig(t *testing.T, tier string, control bool) *rig {
	return splitRigWith(t, tier, control, nil)
}

// Splitter design SL3, SP1, offline proof 3: spec.split is set on C7's own frontier verdict only, a
// control-forced frontier included out; the parent's own budget stays its tier's (SR3).
func TestTriageOffersASplitOnC7sOwnFrontier(t *testing.T) {
	for name, c := range map[string]struct {
		tier    string
		control bool
		offered bool
	}{
		"C7 frontier":                      {"frontier", false, true},
		"C7 frontier in the control group": {"frontier", true, true},
		"a control-forced frontier":        {"standard", true, false},
		"C7 standard":                      {"standard", false, false},
	} {
		t.Run(name, func(t *testing.T) {
			tk := splitRig(t, c.tier, c.control).reconcile(t, "3buqdlot", 1)
			if (tk.Spec.Split != nil) != c.offered {
				t.Fatalf("split %+v", tk.Spec.Split)
			}
			if c.offered && *tk.Spec.Split != (v1alpha1.SplitSpec{MaxChildren: 3, FamilyMicroUSD: 20_400_000, ChildTier: "standard"}) {
				t.Fatalf("split %+v", tk.Spec.Split)
			}
			if c.offered && tk.Spec.Budget.TaskMicroUSD != 13_600_000 {
				t.Fatalf("the parent's cap is its tier's: %d", tk.Spec.Budget.TaskMicroUSD)
			}
		})
	}
}

// split.anyTier: off, a standard task is not offered a split; on, it is, and the scoper is told.
func TestSplitAnyTierOffersEveryTier(t *testing.T) {
	for name, c := range map[string]struct {
		tier    string
		anyTier bool
		offered bool
	}{
		"standard, anyTier off": {"standard", false, false},
		"standard, anyTier on":  {"standard", true, true},
		"frontier, anyTier off": {"frontier", false, true},
		"frontier, anyTier on":  {"frontier", true, true},
	} {
		t.Run(name, func(t *testing.T) {
			g := splitRig(t, c.tier, false)
			g.r.Cfg.Loops = map[string]config.Loop{"review": {Scope: config.ScopeScoper, Split: &config.SplitPolicy{MaxChildren: 3, AnyTier: c.anyTier}}}
			tk := g.reconcile(t, "3buqdlot", 1)
			if (tk.Spec.Split != nil) != c.offered {
				t.Fatalf("split %+v", tk.Spec.Split)
			}
			if got := strings.Contains(ScoperBrief(tk, "n0nce234"), "hand off split {why, children}"); got != c.offered {
				t.Fatalf("ScoperBrief carries splitHow = %v, want %v", got, c.offered)
			}
		})
	}
}

// No row, a row without split, a mode other than scoper, or no issue: no split.
func TestNoSplitOutsideItsRow(t *testing.T) {
	for name, edit := range map[string]func(*rig){
		"a scoper row without split": func(g *rig) { g.r.Cfg.Loops = map[string]config.Loop{"review": {Scope: config.ScopeScoper}} },
		"a steering row with split": func(g *rig) {
			g.r.Cfg.Loops = map[string]config.Loop{"review": {Scope: config.ScopeSteering, Split: &config.SplitPolicy{MaxChildren: 3}}}
		},
		"no loops": func(g *rig) { g.r.Cfg.Loops = nil },
	} {
		t.Run(name, func(t *testing.T) {
			g := splitRig(t, "frontier", false)
			edit(g)
			if tk := g.reconcile(t, "3buqdlot", 1); tk.Spec.Split != nil {
				t.Fatalf("split %+v", tk.Spec.Split)
			}
		})
	}
	g := splitRigWith(t, "frontier", false, func(tk *v1alpha1.Task) { tk.Spec.Issue, tk.Spec.Source.Kind = 0, "schedule" })
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Spec.Split != nil {
		t.Fatal("a task without an issue has nowhere to propose")
	}
}

// Offline proof 7, the scoper's half: the split clause is in the scoping brief only when offered.
func TestTheScoperIsToldItMaySplit(t *testing.T) {
	tk := issueTask("3buqdlot", 7, parentText)
	if b := ScoperBrief(tk, "n0nce234"); strings.Contains(b, "split") {
		t.Fatalf("a scoper not offered a split hears of one:\n%s", b)
	}
	tk.Spec.Split = &v1alpha1.SplitSpec{MaxChildren: 3, FamilyMicroUSD: 20_400_000, ChildTier: "standard"}
	for _, want := range []string{"2 to 3 changes", "hand off split {why, children}", "beside scope", "in the order they merge"} {
		if b := ScoperBrief(tk, "n0nce234"); !strings.Contains(b, want) {
			t.Errorf("the split clause lacks %q", want)
		}
	}
}

// part is the scope of part k of a split: its own file and check.
func part(k int) envelope.Scope {
	return envelope.Scope{State: "ready", Task: fmt.Sprintf("Fix the link to runbook 07 on page %d and change nothing else.", k),
		Touches: []string{fmt.Sprintf("docs/page-%d.md", k)}, Plan: []string{"Point the link at runbook 08."},
		Acceptance: []envelope.Acceptance{{Check: "task docs (CI: validate)", Proves: "every relref resolves"}}}
}

// parts is a split of n parts.
func parts(n int) envelope.Split {
	s := envelope.Split{Why: "Two pages link the removed runbook; each fix is verified and merged alone."}
	for k := 1; k <= n; k++ {
		s.Children = append(s.Children, part(k))
	}
	return s
}

// splitHandoffOf appends run n's handoff carrying the whole task's scope and sp to the rig's room.
func splitHandoffOf(g *rig, n int, sc envelope.Scope, sp envelope.Split) {
	g.log.evs = append(g.log.evs, envelope.Event{Seq: int64(len(g.log.evs) + 1), RunID: rid(n), Type: envelope.Handoff,
		Origin: envelope.OriginClient, Actor: envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:" + rid(n), Role: "triager"},
		Payload: envelope.Must(envelope.HandoffPayload{FromRole: "triager", ToRole: "implementer", Summary: "Scoped and split.",
			Commit: strings.Repeat("c", 40), Scope: &sc, Split: &sp})})
}

// proposed is a split rig whose scoper handed off the whole task and a two-part split.
func proposed(t *testing.T, objs ...client.Object) *rig {
	t.Helper()
	g := splitRigWith(t, "frontier", false, nil, objs...)
	g.reconcile(t, "3buqdlot", 3) // Received → Triaged → Queued → the scoper's run
	splitHandoffOf(g, 0, readyScope(), parts(2))
	endScoper(t, g, "Succeeded", "agent_finished")
	return g
}

// SL4, offline proof 4: a split handoff gives Escalated, split_proposed, one comment quoting the
// plan, the label, and no child, no issue and no count yet.
func TestASplitIsProposedAndWaits(t *testing.T) {
	g := splitRig(t, "frontier", false)
	g.reconcile(t, "3buqdlot", 3)
	before := len(g.f.Comments(7))
	splitHandoffOf(g, 0, readyScope(), parts(2))
	tk := endScoper(t, g, "Succeeded", "agent_finished")
	if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != "split_proposed" || tk.Status.Split == nil ||
		tk.Status.Split.State != "proposed" || tk.Status.Split.Of != 2 || tk.Status.Scope == nil || tk.Status.Scope.State != "ready" ||
		tk.Status.Scope.Seq != tk.Status.Split.Seq {
		t.Fatalf("%s %s %+v %+v", tk.Status.Phase, tk.Status.Reason, tk.Status.Split, tk.Status.Scope)
	}
	c := g.f.Comments(7)[before:]
	if len(c) != 1 || !strings.Contains(c[0], "Part 1:") || !strings.Contains(c[0], "on page 2") ||
		!strings.Contains(c[0], "/factory split") || !strings.Contains(c[0], "/factory retry") || strings.Contains(c[0], "Tokens used") {
		t.Fatalf("comments %q", c)
	}
	if !slices.Equal(g.f.Added(7), []string{"factory/split-proposed"}) || len(g.f.Created()) != 0 {
		t.Fatalf("labels %v issues %v", g.f.Added(7), g.f.Created())
	}
	var l v1alpha1.TaskList
	if err := g.c.List(t.Context(), &l); err != nil || len(l.Items) != 1 {
		t.Fatalf("no child before /factory split (SL4): %d tasks, %v", len(l.Items), err)
	}
	if slices.ContainsFunc(g.metrics.recorded, func(m string) bool { return strings.HasPrefix(m, "split ") }) ||
		!slices.Contains(g.metrics.recorded, "scoper review ready") {
		t.Fatalf("metrics %v", g.metrics.recorded)
	}
}

// C7: an internal task's parts stay in the room behind a link.
func TestAnInternalSplitStaysInTheRoom(t *testing.T) {
	g := splitRigWith(t, "frontier", false, func(tk *v1alpha1.Task) { tk.Spec.DataClass = "internal" })
	g.reconcile(t, "3buqdlot", 3)
	before := len(g.f.Comments(7))
	splitHandoffOf(g, 0, readyScope(), parts(2))
	endScoper(t, g, "Succeeded", "agent_finished")
	c := g.f.Comments(7)[before:]
	if len(c) != 1 || strings.Contains(c[0], "on page 2") || !strings.Contains(c[0], "/r/3buqdlot") {
		t.Fatalf("comments %q", c)
	}
}

// SL14, SR6: a split the task was not offered, or holding more parts than maxChildren, is ignored,
// counted once, and the task runs on its scope.
func TestASplitNotOfferedIsIgnored(t *testing.T) {
	for name, c := range map[string]struct {
		tier string
		n    int
	}{
		"not offered":             {"standard", 2},
		"more parts than allowed": {"frontier", 4},
	} {
		t.Run(name, func(t *testing.T) {
			g := splitRig(t, c.tier, false)
			g.reconcile(t, "3buqdlot", 3)
			splitHandoffOf(g, 0, readyScope(), parts(c.n))
			tk := endScoper(t, g, "Succeeded", "agent_finished")
			if tk.Status.Phase != v1alpha1.PhaseQueued || tk.Status.Split == nil || tk.Status.Split.State != "ignored" ||
				tk.Status.Scope.State != "ready" || !slices.Contains(g.metrics.recorded, "split review ignored") {
				t.Fatalf("%s %+v %v", tk.Status.Phase, tk.Status.Split, g.metrics.recorded)
			}
			if g.reconcile(t, "3buqdlot", 1); g.runs.specs[rid(1)].Role != "implementer" {
				t.Fatal("the task runs on its scope")
			}
		})
	}
}

// splitBy is a maintainer's /factory split, posted after the escalation.
func splitBy(id int64, author string) forge.Comment {
	return forge.Comment{ID: id, Author: author, Body: "/factory split", At: now.Add(30 * time.Minute)}
}

// SL4 (a), SL11, offline proof 4: a maintainer with write approves; the label goes, the approver is
// recorded, and the parent waits in Split. A non-maintainer's command is ignored.
func TestAMaintainerApprovesTheSplit(t *testing.T) {
	g := proposed(t)
	g.r.Now = func() time.Time { return now.Add(time.Hour) }
	g.f.SetComments(7, splitBy(80, "octocat"))
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Split.State != "proposed" {
		t.Fatalf("a non-maintainer approved: %s %+v", tk.Status.Phase, tk.Status.Split)
	}
	g.f.SetComments(7, splitBy(80, "octocat"), splitBy(81, "Smana"))
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseSplit || tk.Status.Split.State != "approved" || tk.Status.Split.ApprovedBy != "github:smana" {
		t.Fatalf("%s %+v", tk.Status.Phase, tk.Status.Split)
	}
	if !slices.Equal(g.f.Removed(7), []string{"factory/split-proposed"}) || !slices.Contains(g.metrics.recorded, "split review approved") ||
		!slices.Contains(g.metrics.recorded, "intervention split") {
		t.Fatalf("removed %v metrics %v", g.f.Removed(7), g.metrics.recorded)
	}
}

// SL11, D4: an approver whose day is spent is told once; the split keeps waiting.
func TestAnApproverPastTheirDayIsRefused(t *testing.T) {
	g := proposed(t, spentLedger(map[string]int64{"github:smana": 5_000_000}))
	g.r.Cfg.Budgets = personBudgets(true, true)
	g.r.Now = func() time.Time { return now.Add(time.Hour) }
	g.f.SetComments(7, splitBy(81, "Smana"))
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Split.State != "proposed" || !slices.Contains(tk.Status.Handled, 81) ||
		!strings.Contains(strings.Join(g.f.Comments(7), "\n"), "is spent") {
		t.Fatalf("%s %+v %v %q", tk.Status.Phase, tk.Status.Split, tk.Status.Handled, g.f.Comments(7))
	}
	// Re-labelling would supersede the proposal: the comment says to comment again, never to re-label.
	said := strings.Join(g.f.Comments(7), "\n")
	if !strings.Contains(said, "comment `/factory split` again") || strings.Contains(said, "label") ||
		!slices.Contains(g.metrics.recorded, "admission_refused person_budget_exhausted") {
		t.Fatalf("%q %v", said, g.metrics.recorded)
	}
}

// D4 in shadow: the approver's spent day is logged and refuses nothing.
func TestASpentApproverInShadowIsApproved(t *testing.T) {
	g := proposed(t, spentLedger(map[string]int64{"github:smana": 5_000_000}))
	g.r.Cfg.Budgets = personBudgets(true, false)
	g.r.Now = func() time.Time { return now.Add(time.Hour) }
	g.f.SetComments(7, splitBy(81, "Smana"))
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseSplit || slices.Contains(g.metrics.recorded, "admission_refused person_budget_exhausted") {
		t.Fatalf("%s %v", tk.Status.Phase, g.metrics.recorded)
	}
}

// A ledger that cannot be read does not consume the command: the next pass sees it again.
func TestALedgerErrorKeepsTheCommand(t *testing.T) {
	g := proposed(t)
	g.r.Cfg.Budgets = personBudgets(true, true)
	g.r.Ledger = brokenLedger{g.c}
	g.r.Now = func() time.Time { return now.Add(time.Hour) }
	g.f.SetComments(7, splitBy(81, "Smana"))
	if _, err := g.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: taskKey("3buqdlot")}); err == nil {
		t.Fatal("a ledger error must be returned")
	}
	var tk v1alpha1.Task
	if err := g.c.Get(t.Context(), taskKey("3buqdlot"), &tk); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(tk.Status.Handled, 81) || tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Split.State != "proposed" {
		t.Fatalf("consumed: %s %+v %v", tk.Status.Phase, tk.Status.Split, tk.Status.Handled)
	}
	g.r.Ledger = g.c // the ledger answers again
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseSplit || tk.Status.Split.State != "approved" {
		t.Fatalf("%s %+v", tk.Status.Phase, tk.Status.Split)
	}
}

// Two /factory split comments approve once and count once.
func TestTwoSplitCommentsApproveOnce(t *testing.T) {
	g := proposed(t)
	g.r.Now = func() time.Time { return now.Add(time.Hour) }
	g.f.SetComments(7, splitBy(81, "Smana"), splitBy(82, "Smana"))
	tk := g.reconcile(t, "3buqdlot", 3)
	count := func(want string) (n int) {
		for _, m := range g.metrics.recorded {
			if m == want {
				n++
			}
		}
		return n
	}
	if tk.Status.Split.State != "approved" || count("split review approved") != 1 || count("intervention split") != 1 {
		t.Fatalf("%+v %v", tk.Status.Split, g.metrics.recorded)
	}
}

// A maintainer's stop declines the proposed split, counted, the label removed.
func TestAMaintainersStopDeclinesTheSplit(t *testing.T) {
	g := proposed(t)
	var tk v1alpha1.Task
	if err := g.c.Get(t.Context(), taskKey("3buqdlot"), &tk); err != nil {
		t.Fatal(err)
	}
	tk.Annotations = map[string]string{v1alpha1.AnnotationStop: "label"}
	if err := g.c.Update(t.Context(), &tk); err != nil {
		t.Fatal(err)
	}
	if got := g.reconcile(t, "3buqdlot", 1); got.Status.Phase != v1alpha1.PhaseStopped || got.Status.Split.State != "declined" ||
		!slices.Contains(g.metrics.recorded, "split review declined") || !slices.Equal(g.f.Removed(7), []string{"factory/split-proposed"}) {
		t.Fatalf("%s %+v %v", got.Status.Phase, got.Status.Split, g.metrics.recorded)
	}
}

// SL4 (b): /factory retry declines the split; the whole task runs on its scope at its own tier,
// with no second scoper run and no child.
func TestARetryRunsTheTaskWhole(t *testing.T) {
	g := proposed(t)
	g.r.Now = func() time.Time { return now.Add(time.Hour) }
	g.f.SetComments(7, retryBy(77, "Smana", -30*time.Minute))
	tk := g.reconcile(t, "3buqdlot", 2)
	s := g.runs.specs[rid(1)]
	if tk.Status.Split.State != "declined" || s.Role != "implementer" || s.Tier != "frontier" || !strings.Contains(s.TaskText, "SCOPE-DATA-") {
		t.Fatalf("%+v %+v", tk.Status.Split, s)
	}
	if !slices.Equal(g.f.Removed(7), []string{"factory/split-proposed"}) || !slices.Contains(g.metrics.recorded, "split review declined") ||
		len(g.f.Created()) != 0 {
		t.Fatalf("removed %v metrics %v created %v", g.f.Removed(7), g.metrics.recorded, g.f.Created())
	}
}

// SL4 (c), (d): a superseding re-label and the expiry each end the wait, counted, the label removed.
func TestTheOtherSplitExitsAreCounted(t *testing.T) {
	g := proposed(t)
	var tk v1alpha1.Task
	if err := g.c.Get(t.Context(), taskKey("3buqdlot"), &tk); err != nil {
		t.Fatal(err)
	}
	tk.Annotations = map[string]string{v1alpha1.AnnotationStop: "superseded"}
	if err := g.c.Update(t.Context(), &tk); err != nil {
		t.Fatal(err)
	}
	if got := g.reconcile(t, "3buqdlot", 1); got.Status.Phase != v1alpha1.PhaseStopped || got.Status.Split.State != "superseded" ||
		!slices.Contains(g.metrics.recorded, "split review superseded") || !slices.Equal(g.f.Removed(7), []string{"factory/split-proposed"}) {
		t.Fatalf("%s %+v %v", got.Status.Phase, got.Status.Split, g.metrics.recorded)
	}
	g = proposed(t)
	g.r.Cfg.Caps.EscalatedHours = new(168)
	g.r.Now = func() time.Time { return now.Add(200 * time.Hour) }
	if got := g.reconcile(t, "3buqdlot", 1); got.Status.Reason != "escalation_expired" || got.Status.Split.State != "expired" ||
		!slices.Contains(g.metrics.recorded, "split review expired") {
		t.Fatalf("%s %+v %v", got.Status.Reason, got.Status.Split, g.metrics.recorded)
	}
}
