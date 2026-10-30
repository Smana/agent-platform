// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"regexp"
	"slices"
	"strings"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/decision"
	"github.com/Smana/maku/internal/factory/forge"
	"github.com/Smana/maku/internal/factory/steering"
)

// ArmInputs is everything §5.1's rule decides on. ReportedHead, Verifiers and Files serve the
// amendment (external reviews R02, R03; ruling R52): ReportedHead is the commit of the
// latest handoff or final room event whose broker-stamped actor is one of the task's implementer
// runs ("" when none named one); Verifiers are the task's reviewer run records, each
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
	// Adoption is what an adopted class's author mode reads beyond the pull request (MC10): the
	// commits' authors and who pushed the branch. Read is false until awaitingCI has read them.
	Adoption Adoption
	// Contract is what row 2c reads of a task with a contract: the files changed since the tester's
	// commit. Read is false until awaitingCI has read them, and an unread one never passes.
	Contract ContractRead
}

// ContractRead is the files a pull request changed since the tester's contract commit (row 2c).
type ContractRead struct {
	Read    bool
	Changed []string
}

// Adoption is what an adopted class's author mode reads beyond the pull request (MC10).
type Adoption struct {
	Read          bool
	CommitAuthors []string
	Pushed        pushCheck
}

// ArmDecision is the verdict and why. Verdict ∈ arm | shadow | wait | ci_red | ci_cancelled | escalate | human.
// Matched is what the class policy-bot's verdict implies: the predicted class on a clean success,
// review on a pending one, gate on an error; a difference from the prediction is a class_mismatch (§2).
type ArmDecision struct {
	Verdict string
	Reason  string
	Matched string
	// Approver is the maintainer whose approval armed row 3 (AM1); "" otherwise.
	Approver string
	// Paused and Capped say what a shadow decision would have met as a live one (AM4).
	Paused, Capped bool
	// Rows are what the decision read, row by row (AM8): its record, on every decision.
	Rows decision.Rows
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
// (property 3), each with its own reason. Every decision carries the rows it read (AM8).
func DecideArm(in ArmInputs) (d ArmDecision) {
	rc := in.Cfg.RepositoryFor(in.Task.Spec.Repository)
	rows := recordOf(in, gateOf(rc, in.Task.Spec.PredictedClass))
	defer func() { d.Rows = rows }()
	if d, done := baseChecks(in, rc); done {
		return d
	}
	human := func(reason, matched string) ArmDecision {
		return ArmDecision{Verdict: "human", Reason: reason, Matched: matched}
	}
	class := in.Task.Spec.PredictedClass
	cl := gateOf(rc, class)
	if cl.Adopted() {
		if reason := adoptAuthor(in, cl); reason != "" {
			return human(reason, class)
		}
	} else if !forge.SameLogin(in.PR.Author, in.Cfg.AgentsLogin) {
		return human("not_agent_authored", "")
	}
	if !cl.Adopted() && rc.Merge.ApproveToMerge { // R-B1: adopted classes keep today's path (AM1)
		switch rv, st := approval(in, rc); st {
		case "veto":
			return human("changes_requested", "review")
		case "stale":
			return human("approval_stale", "review")
		case "approved":
			return ArmDecision{Verdict: "arm", Reason: "maintainer_approved", Matched: "review", Approver: rv.Author}
		}
	} else {
		for _, rv := range in.PR.Reviews {
			if rv.State == "APPROVED" && rc.IsMaintainer(rv.Author) {
				return human("maintainer_approved", "review") // success came from a human: a human merges
			}
		}
	}
	// An agents pull request answers a veto with a revision run (watch.go). An adopted one has no
	// run, so the veto is the gate's: nothing merges, or is counted as would-merge, over it.
	// A review list cut at GitHub's page may hide a veto: it is one.
	if cl.Adopted() && (in.PR.ReviewsTruncated || len(openChangeRequests(membersReviews(in.PR.Reviews), true)) > 0) {
		return human("changes_requested", class)
	}
	if !cl.Live && !cl.Shadow {
		// The predicted class is one the factory never merges (docs, review): a maintainer merges it.
		// Not a mismatch: no class was matched, so none is counted.
		return human("class_unarmed", "")
	}
	if cl.Adopted() {
		rows.Evidence = adoptEvidence(in, cl)
	} else {
		rows.Evidence = agentEvidence(in)
	}
	if rows.Evidence != "" {
		return human(rows.Evidence, class)
	}
	rows.Evidence = "ok"
	if in.DiffRead && in.Cfg.SteeringOn() && touchesSteering(in.Stats) {
		return human("steering_path", class) // no agent pull request changes a note (scoper SC3)
	}
	rows, reason, wait := declared(cl, in, rows)
	if wait {
		return ArmDecision{Verdict: "wait", Reason: reason, Matched: class}
	} else if reason != "" {
		return human(reason, class)
	}
	paused, capped := in.Paused, in.ArmedToday >= rc.Merge.AutoMergesPerDay || (cl.MaxPerDay > 0 && in.ClassArmedToday >= cl.MaxPerDay)
	rows.Paused, rows.Capped = paused, capped
	if !cl.Live { // R32, AM4: every condition holds; the gate is in shadow, which nothing blocks
		return ArmDecision{Verdict: "shadow", Reason: "shadow_would_arm", Matched: class, Paused: paused, Capped: capped}
	}
	if paused {
		return human("class_paused", class)
	}
	if capped {
		return human("auto_merge_cap", class)
	}
	return ArmDecision{Verdict: "arm", Matched: class}
}

// recordOf is the record's start (AM8), known before any row is read: the class's caps, the
// reviewer model, and the diff's counts once the diff is read, whatever the decision refuses.
func recordOf(in ArmInputs, cl config.Class) decision.Rows {
	rows := decision.Rows{MaxFiles: cl.MaxPatchFiles, MaxLines: cl.MaxModifiedLines, Reviewer: reviewerModel(in.Task), Counted: in.DiffRead}
	if in.DiffRead {
		rows.Files = len(in.Stats)
		for _, f := range in.Stats {
			rows.Lines += f.Additions + f.Deletions
		}
	}
	return rows
}

// reviewerModel is the record's reviewer (AM8, AM10): the newest reviewer run's served models
// joined with "+", "none" without a reviewer run, "" (unread) when its provenance names none.
func reviewerModel(t *v1alpha1.Task) string {
	for i := len(t.Status.Runs) - 1; i >= 0; i-- {
		if r := t.Status.Runs[i]; r.Role == "reviewer" {
			return strings.Join(r.Provenance.ServedModels, "+")
		}
	}
	return "none"
}

// approval is row 3's read of an agents pull request with merge.approveToMerge on (human path
// AM1, ruling R-B1): the newest review of each maintainer. veto when one of them is a request for
// changes, or when GitHub cut the list, so a veto may be unseen; stale when the newest approval is
// of another head, or of no head GitHub named (fail closed); approved when it is of the current
// head. DecideArm calls it for no other pull request: off, or adopted, today's loop reads.
func approval(in ArmInputs, rc *config.Repository) (forge.Review, string) {
	if in.PR.ReviewsTruncated {
		return forge.Review{}, "veto"
	}
	newest := map[string]forge.Review{}
	for _, rv := range in.PR.Reviews {
		if !rc.IsMaintainer(rv.Author) || rv.State == "COMMENTED" {
			continue
		}
		login := strings.ToLower(rv.Author)
		if cur, ok := newest[login]; !ok || rv.At.After(cur.At) {
			newest[login] = rv
		}
	}
	var approved forge.Review
	for _, rv := range newest {
		switch rv.State {
		case "CHANGES_REQUESTED":
			return rv, "veto"
		case "APPROVED":
			if approved.Commit != in.PR.HeadSHA { // an approval of the head wins, whatever the map's order
				approved = rv
			}
		}
	}
	switch {
	case approved.Author == "":
		return forge.Review{}, ""
	case approved.Commit == in.PR.HeadSHA:
		return approved, "approved"
	default:
		return approved, "stale"
	}
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

// requiredFor is row 2's list (R21, F3-R8): merge.requiredChecks, and merge.contractCheck on a task whose
// tester wrote a contract. Every reader of the required checks calls this, never the config's list.
func requiredFor(rc *config.Repository, t *v1alpha1.Task) []string {
	if t == nil || t.Status.Contract == nil || rc.Merge.ContractCheck == "" {
		return rc.Merge.RequiredChecks
	}
	return append(slices.Clone(rc.Merge.RequiredChecks), rc.Merge.ContractCheck)
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
	// A task holding a contract while the repository no longer names merge.contractCheck has nothing that
	// proves the contract: fail closed to a human, never drop row 2b.
	if in.Task.Status.Contract != nil && rc.Merge.ContractCheck == "" {
		return ArmDecision{Verdict: "human", Reason: "contract_check_unset"}, true
	}
	switch CIState(in.Checks, requiredFor(rc, in.Task)) {
	case "FAILURE":
		return ArmDecision{Verdict: "ci_red", Reason: "ci_red"}, true
	case "CANCELLED":
		return ArmDecision{Verdict: "ci_cancelled", Reason: "ci_cancelled"}, true
	case "PENDING":
		return ArmDecision{Verdict: "wait", Reason: "ci_pending"}, true
	}
	// Row 2c (R21, F3-R9): the tests the tester wrote are the task's contract, and no pull request changes
	// them. A person decides, never a fix run: it was the implementer's own change (TF8).
	if c := in.Task.Status.Contract; c != nil {
		if !in.Contract.Read {
			return ArmDecision{Verdict: "wait", Reason: "contract_unread"}, true
		}
		if slices.ContainsFunc(c.Files, func(f string) bool { return slices.Contains(in.Contract.Changed, f) }) {
			return ArmDecision{Verdict: "human", Reason: "contract_modified"}, true
		}
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
		if r.Verdict != "approve" || r.Role != "reviewer" {
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

// adoptAuthor is the adopted author mode's identity rows (MC10): the pull request's author is one
// of the class's logins and its head branch matches one of its branches. Both are read from
// GitHub, never from the description.
func adoptAuthor(in ArmInputs, cl config.Class) string {
	if in.PR.CrossRepository {
		return "fork_pull_request" // the branch name and the author are a fork's to choose
	}
	a := cl.Author.Adopt
	if !slices.ContainsFunc(a.Logins, func(l string) bool { return forge.SameLogin(in.PR.Author, l) }) {
		return "not_adopted_author"
	}
	// A branch pattern is ^ and a literal prefix (Validate): quoted, so no character widens it.
	prefixes := make([]string, len(a.Branches))
	for i, b := range a.Branches {
		prefixes[i] = "^" + regexp.QuoteMeta(strings.TrimPrefix(b, "^"))
	}
	if !matchesAny(compile(prefixes, false), in.PR.HeadRef) {
		return "branch_not_allowed"
	}
	return ""
}

// adoptEvidence is the adopted mode's evidence in place of the agents': every commit of the pull
// request is one of the class's logins' and so is every push of its branch (GitHub's record, not
// a claim). A decision that has not read both never passes: awaitingCI reads them first.
func adoptEvidence(in ArmInputs, cl config.Class) string {
	if !in.Adoption.Read {
		return "adoption_unread"
	}
	logins := cl.Author.Adopt.Logins
	for _, a := range in.Adoption.CommitAuthors {
		if !slices.ContainsFunc(logins, func(l string) bool { return a != "" && forge.SameLogin(a, l) }) {
			return "human_commit_in_pr"
		}
	}
	switch in.Adoption.Pushed {
	case pushFactory:
	case pushUnrecorded:
		return "push_unrecorded" // awaitingCI waits for it a while before it hands off
	default:
		return "human_commit_in_pr"
	}
	return ""
}

// touchesSteering is true when a changed file, or the path it was renamed from, is under the
// steering notes' directory.
func touchesSteering(stats []forge.FileStat) bool {
	return slices.ContainsFunc(stats, func(f forge.FileStat) bool {
		return strings.HasPrefix(f.Path, steering.Dir) || strings.HasPrefix(f.Previous, steering.Dir)
	})
}
