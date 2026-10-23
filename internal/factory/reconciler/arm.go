// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"slices"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

// ArmInputs is everything §5.1's rule decides on. ReportedHead, Verifiers and Files serve the
// amendment (external reviews R02, R03; ruling R52): ReportedHead is the commit of the
// latest handoff or final room event whose broker-stamped actor is one of the task's implementer
// runs ("" when none named one); Verifiers are the task's reviewer and tester run records, each
// carrying the head it was given; Files is the forge.Diff read of the pull request's diff.
type ArmInputs struct {
	PR           forge.PR
	Checks       forge.Checks
	Task         *v1alpha1.Task
	Cfg          *config.Config
	ArmedToday   int
	Paused       bool
	ReportedHead string
	Verifiers    []v1alpha1.RunRecord
	Files        struct{ Base, Head map[string]string }
	// NotOwned: the repository's fence file does not name this factory (v2 D7). It never merges
	// there, whatever else holds.
	NotOwned bool
	// Stats is GitHub's list of the changed files, and DiffRead is true once Files and Stats hold
	// the pull request's diff: until then the declared conditions on the diff hold (MC1).
	Stats    []forge.FileStat
	DiffRead bool
	// ClassArmedToday is today's merges of the task's class, against the class's maxPerDay.
	ClassArmedToday int
}

// ArmDecision is the verdict and why. Verdict ∈ arm | shadow | wait | ci_red | ci_cancelled | escalate | human.
// Matched is what the class policy-bot's verdict implies: the predicted class on a clean success,
// review on a pending one, gate on an error; a difference from the prediction is a class_mismatch (§2).
type ArmDecision struct {
	Verdict string
	Reason  string
	Matched string
}

// CIState over the required checks: FAILURE if one failed or reported a state the rollup's
// vocabulary does not, CANCELLED if none failed but one was cancelled (not red: #40), PENDING while one is missing or running, SUCCESS when all passed.
// NEUTRAL and SKIPPED never reach here as words: forge.checkState already maps them to
// SUCCESS on the read. Fail-closed on the unrecognized: a renamed state is not green.
func CIState(c forge.Checks, required []string) string {
	state := "SUCCESS"
	cancelled := false
	for _, name := range required {
		i := slices.IndexFunc(c.Runs, func(x forge.Check) bool { return x.Name == name })
		switch {
		case i < 0 || c.Runs[i].State == "PENDING":
			state = "PENDING"
		case c.Runs[i].State == "CANCELLED":
			cancelled = true
		case c.Runs[i].State != "SUCCESS":
			return "FAILURE"
		}
	}
	if state == "PENDING" {
		return state // a job is re-run only once its workflow run is complete: GitHub refuses it (403) before
	}
	if cancelled {
		return "CANCELLED" // a failure elsewhere has returned above: it outranks a cancellation
	}
	return state
}

// DecideArm is §5.1's merge-actor rule: a fixed base no class can remove, one author mode, and
// the conditions the class declares (merge gate classes MC1). Every "no" fails towards a human
// (property 3), each with its own reason.
func DecideArm(in ArmInputs) ArmDecision {
	rc := in.Cfg.RepositoryFor(in.Task.Spec.Repository)
	if d, done := baseChecks(in, rc); done {
		return d
	}
	human := func(reason, matched string) ArmDecision {
		return ArmDecision{Verdict: "human", Reason: reason, Matched: matched}
	}
	if !forge.SameLogin(in.PR.Author, in.Cfg.AgentsLogin) {
		return human("not_agent_authored", "")
	}
	for _, rv := range in.PR.Reviews {
		if rv.State == "APPROVED" && rc.IsMaintainer(rv.Author) {
			return human("maintainer_approved", "review") // success came from a human: a human merges
		}
	}
	class := in.Task.Spec.PredictedClass
	cl := gateOf(rc, class)
	if !cl.Live && !cl.Shadow {
		return human("class_mismatch", "live") // the diff matched a live class the triage did not predict
	}
	if reason := agentEvidence(in); reason != "" {
		return human(reason, class)
	}
	if reason, wait := declared(cl, in); wait {
		return ArmDecision{Verdict: "wait", Reason: reason, Matched: class}
	} else if reason != "" {
		return human(reason, class)
	}
	if in.Paused {
		return human("class_paused", class)
	}
	if in.ArmedToday >= rc.Merge.AutoMergesPerDay || (cl.MaxPerDay > 0 && in.ClassArmedToday >= cl.MaxPerDay) {
		return human("auto_merge_cap", class)
	}
	if !cl.Live { // R32: every condition holds, and the gate is in shadow until the wave
		return ArmDecision{Verdict: "shadow", Reason: "shadow_would_arm", Matched: class}
	}
	return ArmDecision{Verdict: "arm", Matched: class}
}

// gateOf is the class as the gate reads it: its config, with the diff shape built into
// docs-links and revert (R52) when the config names none.
func gateOf(rc *config.Repository, class string) config.Class {
	cl := rc.Classes[class]
	if cl.DiffShape == "" {
		cl.DiffShape = config.BuiltInShape(class)
	}
	return cl
}

// baseChecks are the base's rows before any class is read: the secret scan, CI, the fence and
// policy-bot. done is false when all of them hold.
func baseChecks(in ArmInputs, rc *config.Repository) (ArmDecision, bool) {
	// R42 (review G6): TruffleHog runs --only-verified, so a red scan is a live credential in the
	// diff. A fix run would remove it from the head and leave it in a public history.
	if i := slices.IndexFunc(in.Checks.Runs, func(x forge.Check) bool { return x.Name == rc.Merge.LeakScanCheck }); i >= 0 &&
		in.Checks.Runs[i].State == "FAILURE" {
		return ArmDecision{Verdict: "escalate", Reason: "secret_scan_red"}, true
	}
	switch CIState(in.Checks, rc.Merge.RequiredChecks) {
	case "FAILURE":
		return ArmDecision{Verdict: "ci_red", Reason: "ci_red"}, true
	case "CANCELLED":
		return ArmDecision{Verdict: "ci_cancelled", Reason: "ci_cancelled"}, true
	case "PENDING":
		return ArmDecision{Verdict: "wait", Reason: "ci_pending"}, true
	}
	if in.NotOwned {
		return ArmDecision{Verdict: "human", Reason: "not_owned"}, true
	}
	state, creator := in.Checks.StatusOf("policy-bot: main")
	if !forge.SameLogin(creator, rc.Merge.PolicyBotLogin) {
		return ArmDecision{Verdict: "human", Reason: "policy_absent"}, true // policy-bot down, or a status someone else posted
	}
	switch state {
	case "ERROR":
		return ArmDecision{Verdict: "human", Reason: "gate_path", Matched: "gate"}, true // unmergeable, even with an approval (S9)
	case "SUCCESS":
		return ArmDecision{}, false
	default:
		return ArmDecision{Verdict: "human", Reason: "policy_pending", Matched: "review"}, true
	}
}

// agentEvidence is the agents author mode's evidence (R02, R03, SC-14): "" when the room
// reported this head, the verifiers approved it and its trailer names one of the task's runs.
func agentEvidence(in ArmInputs) string {
	if slices.Contains(in.Cfg.Templates[in.Task.Spec.Template].Roles, "reviewer") && in.Task.Status.Verdict != "approve" {
		return "no_approving_verdict"
	}
	// R02: the rule can only speak about the commit the room named. An unreported head — a run
	// pushed without a handoff, or a push after the last report, the trailer inside the head
	// notwithstanding — is a commit nobody watched.
	if in.ReportedHead == "" || in.ReportedHead != in.PR.HeadSHA {
		return "head_unreported"
	}
	// R03: an approve is of the head its run was given, and of it only. If the verifier's last
	// approve predates the head the room just reported, the approval was pulled out from under.
	for i := len(in.Verifiers) - 1; i >= 0; i-- {
		r := in.Verifiers[i]
		if r.Verdict != "approve" || (r.Role != "reviewer" && r.Role != "tester") {
			continue
		}
		if r.HeadSHA != in.PR.HeadSHA {
			return "verdict_stale"
		}
		break
	}
	trailer := in.PR.Trailer("Agent-Run")
	if !slices.ContainsFunc(in.Task.Status.Runs, func(r v1alpha1.RunRecord) bool { return r.ID == trailer }) {
		return "foreign_trailer" // SC-14, SP1 R9: another task's run pushed here
	}
	return ""
}
