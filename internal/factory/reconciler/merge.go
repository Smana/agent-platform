// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/narrate"
)

// awaitingCI is §4's merge gate: §5.1's rule decides the pull request, and a decided "arm"
// merges it now at the head that was decided (R52). A maintainer's Request changes is honoured
// here as it is in AwaitingHuman (Δ5): the gate's wait must not silence a review.
func (r *Reconciler) awaitingCI(ctx context.Context, t *v1alpha1.Task) error {
	n := t.Status.PullRequest.Number
	pr, err := r.readPR(ctx, t)
	if err != nil {
		return err
	}
	switch pr.State {
	case "MERGED": // a maintainer merged it through the bypass…
		if !forge.SameLogin(pr.MergedBy, r.repo(t).Merge.MergerLogin) {
			t.Status.PullRequest.MergedBy, t.Status.PullRequest.MergeCommitSHA = pr.MergedBy, pr.MergeCommitSHA
			// R41: a human merge records when, so it counts in the class's breaker window.
			now := metav1.NewTime(r.Now())
			t.Status.PullRequest.MergedAt = &now
			class := t.Spec.PredictedClass
			record(ctx, func(ctx context.Context) { r.Metrics.PROutcome(ctx, class, "human_merged") })
			return r.end(ctx, t, v1alpha1.PhaseDone, "merged")
		}
		// …or our own merge landed but its write was lost: it verifies like the auto-merge it is.
		r.to(t, v1alpha1.PhaseAutoMerging, "")
		return r.autoMerging(ctx, t)
	case "CLOSED":
		class := t.Spec.PredictedClass
		record(ctx, func(ctx context.Context) { r.Metrics.PROutcome(ctx, class, "closed") })
		return r.end(ctx, t, v1alpha1.PhaseClosed, "pr_closed")
	}
	r.countApproves(ctx, t, pr)
	if rvs := r.changesRequested(t, pr); len(rvs) > 0 {
		return r.revise(ctx, t, pr, rvs)
	}
	if !r.Cfg.NeedsMerger() {
		// No class is live or in shadow, so the factory may hold no merger key: there is no gate to
		// read. A maintainer owns the pull request, as in any other hand-off.
		r.to(t, v1alpha1.PhaseAwaitingHuman, "merge_gate_off")
		narrateLater(t, narrate.WaitingForHuman(t, "merge_gate_off"))
		return nil
	}
	checks, err := r.merger(ctx).PullRequestChecks(ctx, n)
	if err != nil {
		return err
	}
	paused, err := r.paused(ctx, r.repo(t), t.Spec.PredictedClass)
	if err != nil {
		return err
	}
	today, err := r.armedToday(ctx, r.repo(t))
	if err != nil {
		return err
	}
	in := ArmInputs{PR: pr, Checks: checks, Task: t, Cfg: r.Cfg, ArmedToday: today, Paused: paused,
		NotOwned: r.Owned == nil || !r.Owned(t.Spec.Repository)}
	if CIState(checks, r.repo(t).Merge.RequiredChecks) == "SUCCESS" {
		// R02/R03: the rule can only speak about a commit the room reported and the verifiers
		// approved. Neither read matters while CI decides on its own, so they wait for green.
		in.ReportedHead, err = r.reportedHead(ctx, t)
		if err != nil {
			return err
		}
		in.Verifiers = verifiers(t)
	}
	d := DecideArm(in)
	if (d.Verdict == "arm" || d.Verdict == "shadow") && (t.Spec.PredictedClass == "docs-links" || t.Spec.PredictedClass == "revert") {
		// forge.Files is read late: only a decision that would reach the LinksOnly gate of an
		// entry class pays its 2N+1 API calls. The first pass passed that gate vacuously; this
		// second pass decides on the real diff.
		base, head, ferr := r.merger(ctx).Files(ctx, "main", pr.HeadSHA)
		if ferr != nil {
			return ferr
		}
		in.Files.Base, in.Files.Head = base, head
		d = DecideArm(in)
	}
	if d.Verdict == "arm" || d.Verdict == "shadow" {
		// #82: R02 and R03 pass on a human's push the agent pulled and pushed on top of, before the
		// first gate or during a revision run. Only who moved the branch tells it apart, as on a CI
		// fix (humanCommit), so every path into a merge reads it.
		pushed, err := r.factoryPushed(ctx, t, pr.HeadSHA)
		if err != nil {
			return err
		}
		switch {
		case pushed == pushUnrecorded && r.Now().Before(pushDeadline(t)):
			r.log().Info("the head's push is not recorded yet: waiting", "task.id", t.Name, "head", pr.HeadSHA)
			return nil
		case pushed != pushFactory:
			d = ArmDecision{Verdict: "human", Reason: "human_commit_in_pr"}
		}
	}
	if d.Verdict != "wait" {
		class, verdict, reason := t.Spec.PredictedClass, d.Verdict, cmp.Or(d.Reason, "armed")
		record(ctx, func(ctx context.Context) { r.Metrics.MergeDecision(ctx, class, verdict, reason) })
	}
	switch d.Verdict {
	case "wait":
		t.Status.Reason = d.Reason
		return nil
	case "ci_red":
		return r.fixCI(ctx, t, pr, checks)
	case "ci_cancelled":
		return r.rerunCancelled(ctx, t, pr, checks)
	case "escalate": // R42: nothing the factory may fix
		narrateOn(t, pr.Number, narrate.WaitingForHuman(t, d.Reason))
		return r.end(ctx, t, v1alpha1.PhaseEscalated, d.Reason)
	case "human":
		if d.Matched != "" && d.Matched != t.Spec.PredictedClass {
			pred, matched := t.Spec.PredictedClass, d.Matched
			record(ctx, func(ctx context.Context) { r.Metrics.ClassMismatch(ctx, pred, matched) })
		}
		// R02 held before verdict_stale: the head is the factory's, so there is nothing for a human
		// to judge yet, only a verdict of this head to get (N1).
		if d.Reason == "verdict_stale" && t.Status.GateReentries < maxGateReentries &&
			r.nextVerifier(t, "implementer") != "" && !r.engagedSince(t, pr, t.Status.PhaseSince) {
			r.backToGate(t, d.Reason, "rereview_verdict_stale", pr.HeadSHA)
			return nil
		}
		t.Status.PullRequest.HeadSHA = pr.HeadSHA // what reenter holds the hand-off to
		r.to(t, v1alpha1.PhaseAwaitingHuman, d.Reason)
		narrateLater(t, narrate.WaitingForHuman(t, d.Reason))
		return nil
	case "shadow": // R32: the decision is recorded and narrated; a maintainer merges or closes
		r.to(t, v1alpha1.PhaseAwaitingHuman, d.Reason)
		narrateOn(t, pr.Number, narrate.WouldArm(t, d.Matched, pr.HeadSHA))
		return nil
	}
	// The decided head, pinned: GitHub checks it at merge time, so a push that landed since the
	// read refuses the merge (409) and the new head is decided again — never merged on this
	// decision (SC-14). A merge GitHub does not allow as it stands (405) is a human's to fix.
	if err := r.merger(ctx).Merge(ctx, pr.NodeID, pr.HeadSHA); err != nil {
		switch {
		case errors.Is(err, forge.ErrHeadMoved):
			r.to(t, v1alpha1.PhaseAwaitingCI, "head_moved")
			return nil
		case errors.Is(err, forge.ErrNotMergeable):
			r.to(t, v1alpha1.PhaseAwaitingHuman, "not_mergeable")
			narrateLater(t, narrate.WaitingForHuman(t, "not_mergeable"))
			return nil
		default:
			return err
		}
	}
	armed := metav1.NewTime(r.Now())
	t.Status.PullRequest.ArmedAt, t.Status.PullRequest.HeadSHA = &armed, pr.HeadSHA
	r.to(t, v1alpha1.PhaseAutoMerging, "")
	narrateOn(t, pr.Number, narrate.Armed(t))
	return nil
}

// maxGateReentries bounds the returns to the gate per task: an input that flaps between a
// transient hand-off and its clearing (a policy-bot status) returns this many times, then stays
// with a human.
const maxGateReentries = 3

// reenter returns a transient hand-off to the merge gate (N1). A hand-off is transient when its
// reason is one the gate's inputs can clear by themselves; every other one — a verdict of changes,
// a maintainer's review, a class or cap rule — was a human's to judge, and stays theirs. Even a
// transient one returns only on the head it was handed off at, while no maintainer has reviewed or
// commented since: no factory run is in flight in AwaitingHuman, so a push meanwhile is a human's,
// and a later revision must never auto-merge what a human was asked to judge. head_unreported
// returns only once the room reports that head from one of the task's implementer runs, so the
// factory pushed it (R02). verdict_stale never reaches here unless past the cap: the gate
// re-reviews it itself.
func (r *Reconciler) reenter(ctx context.Context, t *v1alpha1.Task, pr forge.PR) (bool, error) {
	why := t.Status.Reason
	switch why {
	case "policy_pending", "policy_absent", "head_unreported", "ci_cancelled":
	default:
		return false, nil
	}
	if t.Status.GateReentries >= maxGateReentries || pr.HeadSHA == "" || pr.HeadSHA != t.Status.PullRequest.HeadSHA ||
		r.engagedSince(t, pr, t.Status.PhaseSince) {
		return false, nil
	}
	if why == "head_unreported" {
		reported, err := r.reportedHead(ctx, t)
		if err != nil || reported != pr.HeadSHA {
			return false, err
		}
		r.backToGate(t, why, "reentered_head_reported", pr.HeadSHA)
		return true, nil
	}
	c, err := r.merger(ctx).PullRequestChecks(ctx, pr.Number)
	if err != nil {
		return false, err
	}
	if why == "ci_cancelled" {
		if CIState(c, r.repo(t).Merge.RequiredChecks) != "SUCCESS" {
			return false, nil
		}
		r.backToGate(t, why, "reentered_ci_green", pr.HeadSHA)
		return true, nil
	}
	if state, creator := c.StatusOf("policy-bot: main"); state != "SUCCESS" || !forge.SameLogin(creator, r.repo(t).Merge.PolicyBotLogin) {
		return false, nil
	}
	r.backToGate(t, why, "reentered_policy", pr.HeadSHA)
	return true, nil
}

// backToGate is one return to the merge gate, counted against maxGateReentries: to AwaitingCI,
// or to Queued for the first verifier when only a verdict of the head clears the hand-off. The
// reason is the status reason, which the room's facts record, and it is narrated.
func (r *Reconciler) backToGate(t *v1alpha1.Task, why, reason, head string) {
	t.Status.GateReentries++
	next := v1alpha1.PhaseAwaitingCI
	if reason == "rereview_verdict_stale" {
		next, t.Status.NextRole = v1alpha1.PhaseQueued, r.nextVerifier(t, "implementer")
	}
	r.to(t, next, reason)
	narrateLater(t, narrate.BackToGate(t, reason, t.Status.GateReentries))
	r.log().Info("transient hand-off returns to the merge gate", "task.id", t.Name, "handoff", why, "reason", reason,
		"head", head, "reentries", t.Status.GateReentries)
}

// humanCommit hands a CI fix's pull request to a human when its head is not the one the room last
// reported from one of the task's implementer runs: a human pushed, and a fix on top, reviewed and
// approved by the factory, would merge that human's commit (owner: a human-authored change never
// auto-merges through the factory). A log read short of its end reports no head, so it hands off.
//
// A reported head is not enough (#74): a human's push during the fix run, pulled and pushed on by
// the agent, ends under the agent's own head. So every commit of the pull request must claim one
// of the task's implementer runs in its Agent-Run trailer (only a claim: see factoryCommits) — the
// whole list, since an agent's squash or rebase rewrites every SHA — and every push to the branch
// must be the agents App's (#76, factoryPushed): what binds the commits to the factory, since a
// human can copy a trailer but cannot push as the App without access to the agents namespace or
// the App key. The binding is to the App, not the task: octo-sts's implementer trust policy
// (cloud-native-ref .github/chainguard/agent-implementer.sts.yaml) is repository-wide, so any
// implementer run of any task can push as the bot, and only the trailer, a claim, narrows that to
// this task's runs. The author login is never read: it is whatever GitHub matched the commit's
// email to, which anyone can set. A merge from GitHub's "Update branch" carries no trailer and
// hands off here as it already did on its head; that stays the fail-safe side. A pull request
// past GitHub's 250 listed commits cannot be read whole, so it hands off too.
//
// It returns true when the caller must stop: the task was handed off, or GitHub has not recorded
// the head's push yet and the task waits, unchanged, for a later reconcile (pushRecordGrace).
func (r *Reconciler) humanCommit(ctx context.Context, t *v1alpha1.Task, head string) (bool, error) {
	reported, err := r.reportedHead(ctx, t)
	if err != nil {
		return false, err
	}
	if reported == head {
		factoryMade, err := r.factoryCommits(ctx, t, head)
		if err != nil {
			return false, err
		}
		if factoryMade {
			pushed, err := r.factoryPushed(ctx, t, head)
			if err != nil {
				return false, err
			}
			switch {
			case pushed == pushFactory:
				return false, nil
			case pushed == pushUnrecorded && r.Now().Before(pushDeadline(t)):
				r.log().Info("the head's push is not recorded yet: waiting", "task.id", t.Name, "head", head)
				return true, nil
			}
		}
	}
	t.Status.NextTrigger = ""
	t.Status.PullRequest.HeadSHA = head
	r.to(t, v1alpha1.PhaseAwaitingHuman, "human_commit_in_pr")
	narrateLater(t, narrate.WaitingForHuman(t, "human_commit_in_pr"))
	return true, nil
}

// engagedSince: a maintainer reviewed or commented on the pull request after since.
func (r *Reconciler) engagedSince(t *v1alpha1.Task, pr forge.PR, since *metav1.Time) bool {
	if since == nil {
		return true // no hand-off time to read activity against: never assume there was none
	}
	for _, rv := range pr.Reviews {
		if r.repo(t).IsMaintainer(rv.Author) && rv.At.After(since.Time) {
			return true
		}
	}
	for _, c := range pr.Comments {
		if r.repo(t).IsMaintainer(c.Author) && c.At.After(since.Time) {
			return true
		}
	}
	return false
}

// reportedHead is the commit of the latest handoff or final room event whose broker-stamped
// actor is one of the task's implementer runs (R02's input to DecideArm). A log that could not
// be read to its end reports nothing: the newest event may be past what was read, and deciding
// on a stale newest-commit is exactly what R02 refuses.
func (r *Reconciler) reportedHead(ctx context.Context, t *v1alpha1.Task) (string, error) {
	impl := implementerRuns(t)
	evs, complete, err := r.roomTail(ctx, t.Status.RoomRef, 0, briefEvent)
	if err != nil {
		return "", err
	}
	if !complete {
		return "", nil
	}
	head := ""
	for _, e := range evs {
		if !impl[e.RunID] {
			continue
		}
		if c := eventCommit(e); c != "" {
			head = c
		}
	}
	return head, nil
}

// factoryCommits: head is among the pull request's commits, and each of them claims one of the
// task's implementer runs. The claim authenticates nothing: run IDs are public (the PR footer,
// every agent commit on the branch) and an amend or a cherry-pick keeps the trailer. factoryPushed
// is what binds the commits to the factory; the claim stays because it narrows that to this task's
// implementer runs, where the App's push identity is shared by every implementer run of any task.
func (r *Reconciler) factoryCommits(ctx context.Context, t *v1alpha1.Task, head string) (bool, error) {
	cs, err := r.gh(ctx).PullRequestCommits(ctx, t.Status.PullRequest.Number)
	if errors.Is(err, forge.ErrCommitsTruncated) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// GitHub orders the list by commit date, not ancestry, so the head is looked for, not assumed
	// last. A force-push between the two reads drops it, and hands off; a push on top adds a
	// commit, which the trailer test below then reads.
	if !slices.ContainsFunc(cs, func(c forge.Commit) bool { return c.SHA == head }) {
		return false, nil
	}
	impl := implementerRuns(t)
	return !slices.ContainsFunc(cs, func(c forge.Commit) bool { return !impl[c.Trailer("Agent-Run")] }), nil
}

// pushRecordGrace is how long after the task's last implementer run finished the gate waits for
// GitHub's activity to record the run's push; past it, an unrecorded head hands off.
const pushRecordGrace = 2 * time.Minute

// pushCheck is factoryPushed's answer.
type pushCheck int

const (
	pushStranger   pushCheck = iota // a move not the App's, a broken chain, or a history not read whole
	pushFactory                     // every move the App's, chained from the creation to the head
	pushUnrecorded                  // the App's history so far, but the head's push not in it yet
)

// pushDeadline is when an unrecorded head stops being waited for: pushRecordGrace after the
// latest finish of the task's implementer runs; zero, so no wait, when none has finished.
func pushDeadline(t *v1alpha1.Task) time.Time {
	var last time.Time
	for _, rec := range t.Status.Runs {
		if rec.Role == "implementer" && rec.Finished != nil && rec.Finished.After(last) {
			last = rec.Finished.Time
		}
	}
	if last.IsZero() {
		return last
	}
	return last.Add(pushRecordGrace)
}

// factoryPushed: GitHub records every move of the task's branch, since its latest creation, as
// the agents App's (cfg.agentsLogin, the identity octo-sts mints implementer tokens as), in one
// unbroken chain from that creation to head (#76). The pusher is GitHub's record of whose
// credential moved the ref, so a commit with a copied trailer, a force-push or GitHub's "Update
// branch" by anyone else is a stranger's. The chain is what makes the read whole: a gap, or a
// history past the page cap, is a stranger's too. One case waits instead: an empty history, or
// one wholly the App's whose newest move is not yet the head — GitHub records a push a little
// after it lands. A read error is returned, as factoryCommits's is: the caller retries.
func (r *Reconciler) factoryPushed(ctx context.Context, t *v1alpha1.Task, head string) (pushCheck, error) {
	ps, err := r.gh(ctx).BranchActivity(ctx, "agent/"+t.Name)
	if errors.Is(err, forge.ErrActivityTruncated) {
		return pushStranger, nil
	}
	if err != nil {
		return pushStranger, err
	}
	if len(ps) == 0 {
		return pushUnrecorded, nil
	}
	want := ps[0].After // newest first
	for _, p := range ps {
		if p.Actor == "" || !forge.SameLogin(p.Actor, r.Cfg.AgentsLogin) || p.After != want {
			return pushStranger, nil
		}
		if p.Type == "branch_creation" {
			if ps[0].After != head {
				return pushUnrecorded, nil
			}
			return pushFactory, nil
		}
		want = p.Before
	}
	return pushStranger, nil // the creation was never read
}

// implementerRuns are the IDs of the task's implementer runs.
func implementerRuns(t *v1alpha1.Task) map[string]bool {
	impl := make(map[string]bool, len(t.Status.Runs))
	for _, rec := range t.Status.Runs {
		if rec.Role == "implementer" {
			impl[rec.ID] = true
		}
	}
	return impl
}

// eventCommit is the commit a handoff or a review verdict names, "" when it names none.
func eventCommit(e envelope.Event) string {
	if e.Type == envelope.Handoff {
		var p envelope.HandoffPayload
		if json.Unmarshal(e.Payload, &p) == nil {
			return p.Commit
		}
		return ""
	}
	var p envelope.MessagePayload
	if json.Unmarshal(e.Payload, &p) == nil && p.Kind == envelope.KindReviewVerdict {
		return p.Commit
	}
	return ""
}

// verifiers are the task's reviewer and tester run records, each carrying the head it was
// given (R03's input to DecideArm).
func verifiers(t *v1alpha1.Task) []v1alpha1.RunRecord {
	var out []v1alpha1.RunRecord
	for _, rec := range t.Status.Runs {
		if rec.Role == "reviewer" || rec.Role == "tester" {
			out = append(out, rec)
		}
	}
	return out
}

func failing(c forge.Checks, required []string) []string {
	var out []string
	for _, x := range c.Runs {
		if x.State == "FAILURE" && slices.Contains(required, x.Name) {
			out = append(out, x.Name)
		}
	}
	return out
}

// rerunCancelled re-runs the cancelled required checks, up to merge.checkReruns per task, then
// hands the task to a human. A cancellation is the runner's or a concurrency group's, not the
// diff's: a fix run has nothing to change and spent 405k tokens on an empty commit (#40). The
// re-run is not a "wait": the checks read PENDING on the next pass, which is what waits.
func (r *Reconciler) rerunCancelled(ctx context.Context, t *v1alpha1.Task, pr forge.PR, c forge.Checks) error {
	var ids []int64
	for _, x := range c.Runs {
		if x.State == "CANCELLED" && slices.Contains(r.repo(t).Merge.RequiredChecks, x.Name) {
			ids = append(ids, x.ID)
		}
	}
	if t.Status.CheckRerunsHead != pr.HeadSHA { // a new head gets a fresh budget
		t.Status.CheckReruns, t.Status.CheckRerunsHead = 0, pr.HeadSHA
	}
	if t.Status.CheckReruns >= r.repo(t).Merge.CheckReruns {
		t.Status.PullRequest.HeadSHA = pr.HeadSHA
		r.to(t, v1alpha1.PhaseAwaitingHuman, "ci_cancelled")
		narrateLater(t, narrate.WaitingForHuman(t, "ci_cancelled"))
		return nil
	}
	// Every cancelled job is tried; the first error is returned after the loop. A re-run is spent
	// only once one has gone through, so a refused call leaves the budget for the next pass.
	var first error
	done := false
	for _, id := range ids {
		if err := r.merger(ctx).RerunCheck(ctx, id); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		done = true
	}
	if done {
		t.Status.CheckReruns++
		t.Status.Reason = "ci_rerun"
	}
	return first
}

// fixCI: two fix runs, then escalate with the failure on the PR (§6.3).
func (r *Reconciler) fixCI(ctx context.Context, t *v1alpha1.Task, pr forge.PR, c forge.Checks) error {
	names := failing(c, r.repo(t).Merge.RequiredChecks)
	if t.Status.FixRuns >= r.repo(t).Merge.FixRuns {
		narrateOn(t, pr.Number, narrate.CIExhausted(t, names))
		return r.end(ctx, t, v1alpha1.PhaseEscalated, "ci_red")
	}
	t.Status.FixRuns++
	var why []string
	for _, x := range c.Runs {
		if slices.Contains(names, x.Name) {
			why = append(why, fmt.Sprintf("`%s`: %s", x.Name, strings.ToLower(x.State)))
		}
	}
	narrateLater(t, narrate.CIFixing(t, pr.Number, pr.HeadSHA, why, int(t.Status.FixRuns), int(r.repo(t).Merge.FixRuns)))
	msg := fmt.Sprintf("CI failed on %s: %s. Read the failing jobs with `gh pr checks %d` and fix them on the same branch.",
		pr.HeadSHA, strings.Join(names, ", "), pr.Number)
	if err := r.Rooms.Enqueue(ctx, t.Status.RoomRef, "ci", msg, int64(t.Status.FixRuns)); err != nil {
		return err
	}
	t.Status.NextTrigger = "ci"
	r.to(t, v1alpha1.PhaseQueued, "")
	return nil
}

// autoMerging watches the merge the decision issued. R52 armed nothing on GitHub — the merge
// either landed or did not — so there is no disarm path: an open pull request whose head moved
// never merged, and the new head is decided again from AwaitingCI.
func (r *Reconciler) autoMerging(ctx context.Context, t *v1alpha1.Task) error {
	pr, err := r.readPR(ctx, t)
	if err != nil {
		return err
	}
	ref := t.Status.PullRequest
	class := t.Spec.PredictedClass
	switch {
	case pr.State == "MERGED":
		if forge.SameLogin(pr.MergedBy, r.repo(t).Merge.MergerLogin) {
			r.label(ctx, pr, AutoMergedLabel(class))
		}
		now := metav1.NewTime(r.Now())
		ref.MergedBy, ref.MergeCommitSHA, ref.MergedAt = pr.MergedBy, pr.MergeCommitSHA, &now
		ref.AutoMerged = forge.SameLogin(pr.MergedBy, r.repo(t).Merge.MergerLogin) // AutoMerged: merged by the merger App (R52)
		if !ref.AutoMerged {
			record(ctx, func(ctx context.Context) { r.Metrics.PROutcome(ctx, class, "human_merged") })
			return r.end(ctx, t, v1alpha1.PhaseDone, "merged")
		}
		record(ctx, func(ctx context.Context) { r.Metrics.PROutcome(ctx, class, "auto_merged") })
		r.to(t, v1alpha1.PhaseVerifying, "")
	case pr.State == "CLOSED":
		record(ctx, func(ctx context.Context) { r.Metrics.PROutcome(ctx, class, "closed") })
		return r.end(ctx, t, v1alpha1.PhaseClosed, "pr_closed")
	case pr.HeadSHA != ref.HeadSHA:
		r.to(t, v1alpha1.PhaseAwaitingCI, "head_moved")
	}
	return nil
}

// MainState is §6.4's watch of main's CI on the merge commit: FAILURE if a watched check that ran
// failed, PENDING while one that ran is still running, else SUCCESS. A watched check that never
// ran is not failing: a path-filtered push workflow never reports on a docs merge.
func MainState(runs []forge.Check, watched []string) string {
	state := "SUCCESS"
	for _, c := range runs {
		if !slices.Contains(watched, c.Name) {
			continue
		}
		switch c.State {
		case "FAILURE":
			return "FAILURE"
		case "PENDING", "CANCELLED": // a cancelled check on main is not green and not red: keep watching
			state = "PENDING"
		}
	}
	return state
}

// verifying watches main's CI on the merge commit for merge.verifyFor (§6.4). Red, or a
// maintainer's factory/revert, reverts; not red for the whole window, with nothing still running,
// is done.
func (r *Reconciler) verifying(ctx context.Context, t *v1alpha1.Task) error {
	if t.Annotations[v1alpha1.AnnotationRevert] != "" {
		return r.revert(ctx, t, "revert_requested")
	}
	ref := t.Status.PullRequest
	runs, err := r.merger(ctx).CommitChecks(ctx, ref.MergeCommitSHA)
	if err != nil {
		return err
	}
	switch MainState(runs, r.repo(t).Merge.VerifyChecks) {
	case "FAILURE":
		return r.revert(ctx, t, "main_red")
	case "SUCCESS":
		if ref.MergedAt != nil && r.Now().Sub(ref.MergedAt.Time) >= r.repo(t).Merge.VerifyFor.Duration {
			return r.end(ctx, t, v1alpha1.PhaseDone, "merged_verified")
		}
	}
	return nil
}

// revertable: a Done, auto-merged task a maintainer asked to revert inside the window.
func (r *Reconciler) revertable(t *v1alpha1.Task) bool {
	ref := t.Status.PullRequest
	return t.Status.Phase == v1alpha1.PhaseDone && t.Annotations[v1alpha1.AnnotationRevert] != "" &&
		ref != nil && ref.AutoMerged && ref.RevertNumber == 0 && ref.MergedAt != nil &&
		r.Now().Sub(ref.MergedAt.Time) <= r.repo(t).Merge.RevertWindow.Duration
}

// revert opens the merger-authored revert PR and arms it: the policy's `revert` rule carries
// docs-links' paths and caps, so GitHub merges it once its checks are green (§6.4). Human
// merges are never reverted: only an auto-merged task reaches here, so before the wave (R32)
// no revert is ever opened.
func (r *Reconciler) revert(ctx context.Context, t *v1alpha1.Task, why string) error {
	ref := t.Status.PullRequest
	if ref.RevertNumber > 0 { // a lost write's replay: the revert exists; end, never open a second
		return r.end(ctx, t, v1alpha1.PhaseReverted, why)
	}
	pr, err := r.gh(ctx).PullRequest(ctx, ref.Number)
	if err != nil {
		return err
	}
	r.label(ctx, pr, AutoMergedLabel(t.Spec.PredictedClass)) // a second chance: its revert is read against it
	rv, err := r.merger(ctx).RevertPR(ctx, ref.NodeID, fmt.Sprintf("Revert %q", pr.Title),
		fmt.Sprintf("Reverts #%d (agent factory task `%s`): %s.", ref.Number, t.Name, narrate.Reason(why)))
	if err != nil {
		return err
	}
	ref.RevertNumber = rv.Number
	if r.repo(t).Classes["revert"].Live { // not live (R32): the revert PR opens and a maintainer merges it
		if err := r.merger(ctx).EnableAutoMerge(ctx, rv.NodeID, ""); err != nil { // the merger's own revert-* branch
			return err
		}
	}
	class := t.Spec.PredictedClass
	record(ctx, func(ctx context.Context) { r.Metrics.PROutcome(ctx, class, "reverted") })
	narrateOn(t, ref.Number, narrate.RevertOpened(t, rv.Number, why))
	if n := r.Cfg.ControlFor().Issue; n > 0 { // R41: where the kill switch lives
		b := r.repo(t).Merge.Breaker
		narrateOn(t, n, narrate.ClassDemoted(t, b.Window, b.MaxReverts))
	}
	return r.end(ctx, t, v1alpha1.PhaseReverted, why)
}

// revertPending: a Reverted task whose revert PR has not been seen merged, closed or stalled.
func (r *Reconciler) revertPending(t *v1alpha1.Task) bool {
	ref := t.Status.PullRequest
	return t.Status.Phase == v1alpha1.PhaseReverted && ref != nil && ref.RevertNumber > 0 &&
		!slices.ContainsFunc(t.Status.Narrated, func(k string) bool { return strings.HasPrefix(k, "revert-end-") })
}

// revertWatch: a revert opened while main is red for another reason never goes green, and must
// not stay armed indefinitely. Merged or closed ends the watch; still open after verifyFor is
// disarmed and handed to a maintainer (§6.4). The narration key ends the watch, once.
func (r *Reconciler) revertWatch(ctx context.Context, t *v1alpha1.Task) error {
	ref := t.Status.PullRequest
	pr, err := r.gh(ctx).PullRequest(ctx, ref.RevertNumber)
	if err != nil {
		return err
	}
	r.label(ctx, pr, RevertOfLabel(ref.Number)) // every pass until it holds: the watch reads it again
	switch {
	case pr.State == "MERGED" || pr.State == "CLOSED":
		t.Status.Narrated = append(t.Status.Narrated, "revert-end-"+strings.ToLower(pr.State))
		return nil
	case t.Status.PhaseSince != nil && r.Now().Sub(t.Status.PhaseSince.Time) >= r.repo(t).Merge.VerifyFor.Duration:
		if err := r.merger(ctx).DisableAutoMerge(ctx, pr.NodeID); err != nil {
			return err
		}
		return r.narrator(ctx).Post(ctx, t, ref.Number, narrate.RevertStalled(t, ref.RevertNumber))
	}
	return nil
}

// paused: the circuit breaker (§6.4, extended by R41 for review G5). It reads outcomes, not the
// config: a class whose last merge.breaker.window merges hold merge.breaker.maxReverts reverts
// (a maintainer's factory/revert, or main_red) is demoted to human review, whatever the config
// hash. Maintainers' merges of a demoted class refill the window, so clean ones lift it. The
// merges are the Tasks' united with GitHub's labelled ones (v2 D1), so a rebuild that lost every
// Task still reads a demoted class as demoted. Only auto-merges carry a label: maintainers' merges
// before a rebuild are not on GitHub's side, which can only keep a class demoted longer.
func (r *Reconciler) paused(ctx context.Context, rc *config.Repository, class string) (bool, error) {
	gh, err := r.githubLedger(ctx, rc)
	if err != nil {
		return false, err
	}
	var l v1alpha1.TaskList
	if err := r.Client.List(ctx, &l, client.InNamespace(r.Namespace)); err != nil {
		return false, err
	}
	var tasks []Merge
	for i := range l.Items {
		o := &l.Items[i]
		if ref := o.Status.PullRequest; o.Spec.PredictedClass == class && ref != nil && ref.MergedAt != nil && r.onRepo(o, rc) {
			tasks = append(tasks, Merge{Number: ref.Number, At: ref.MergedAt.Time, Reverted: o.Status.Phase == v1alpha1.PhaseReverted})
		}
	}
	demoted, _ := Demoted(uniteMerges(tasks, gh[class]), rc.Merge.Breaker)
	return demoted, nil
}

// Demoted reports whether the newest b.Window merges hold at least b.MaxReverts reverts, and how
// many they hold. A zero-value breaker demotes every class (0 ≥ 0): fail-closed; strict decode
// and Validate keep it off the production path, so a test rig that builds config.Merge by hand
// must set a breaker.
func Demoted(merges []Merge, b config.Breaker) (bool, int) {
	newestFirst(merges)
	reverts := 0
	for _, m := range merges[:min(len(merges), b.Window)] {
		if m.Reverted {
			reverts++
		}
	}
	return reverts >= b.MaxReverts, reverts
}

// armedToday counts the merges the factory issued today: Arming is recorded whichever way the
// merge itself went, and §5.1's cap counts the decisions, not their outcomes. GitHub's labelled
// merges of today, of every class, count too (v2 D1), once per pull request.
func (r *Reconciler) armedToday(ctx context.Context, rc *config.Repository) (int, error) {
	gh, err := r.githubLedger(ctx, rc)
	if err != nil {
		return 0, err
	}
	var l v1alpha1.TaskList
	if err := r.Client.List(ctx, &l, client.InNamespace(r.Namespace)); err != nil {
		return 0, err
	}
	n, seen := 0, map[int]bool{}
	for i := range l.Items {
		if ref := l.Items[i].Status.PullRequest; ref != nil && ref.ArmedAt != nil && sameUTCDay(ref.ArmedAt.Time, r.Now()) &&
			r.onRepo(&l.Items[i], rc) {
			n++
			seen[ref.Number] = true
		}
	}
	for _, ms := range gh {
		for _, m := range ms {
			if sameUTCDay(m.At, r.Now()) && !seen[m.Number] {
				n++
				seen[m.Number] = true
			}
		}
	}
	return n, nil
}
