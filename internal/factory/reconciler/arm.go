// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"net/url"
	"regexp"
	"slices"
	"strings"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

// ArmInputs is everything §5.1's rule decides on. ReportedHead, Verifiers and Files serve the
// amendment (external reviews R02, R03; ruling R52): ReportedHead is the commit of the
// latest handoff or final room event whose broker-stamped actor is one of the task's implementer
// runs ("" when none named one); Verifiers are the task's reviewer and tester run records, each
// carrying the head it was given; Files is the forge.Files read of the pull request's diff.
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
}

// ArmDecision is the verdict and why. Verdict ∈ arm | shadow | wait | ci_red | human. Matched is
// what the class policy-bot's verdict implies: the predicted class on a clean success, review on
// a pending one, gate on an error; a difference from the prediction is a class_mismatch (§2).
type ArmDecision struct {
	Verdict string
	Reason  string
	Matched string
}

// CIState over the required checks: FAILURE if one failed or reported a state the rollup's
// vocabulary does not, PENDING while one is missing or running, SUCCESS when all passed.
// NEUTRAL and SKIPPED never reach here as words: forge.checkState already maps them to
// SUCCESS on the read. Fail-closed on the unrecognized: a renamed state is not green.
func CIState(c forge.Checks, required []string) string {
	state := "SUCCESS"
	for _, name := range required {
		i := slices.IndexFunc(c.Runs, func(x forge.Check) bool { return x.Name == name })
		switch {
		case i < 0 || c.Runs[i].State == "PENDING":
			state = "PENDING"
		case c.Runs[i].State != "SUCCESS":
			return "FAILURE"
		}
	}
	return state
}

// DecideArm is §5.1's merge-actor rule. Every "no" fails towards a human (property 3).
func DecideArm(in ArmInputs) ArmDecision {
	switch CIState(in.Checks, in.Cfg.Merge.RequiredChecks) {
	case "FAILURE":
		return ArmDecision{Verdict: "ci_red", Reason: "ci_red"}
	case "PENDING":
		return ArmDecision{Verdict: "wait", Reason: "ci_pending"}
	}
	human := func(reason, matched string) ArmDecision {
		return ArmDecision{Verdict: "human", Reason: reason, Matched: matched}
	}
	class := in.Task.Spec.PredictedClass
	state, creator := in.Checks.StatusOf("policy-bot: main")
	if creator != in.Cfg.Merge.PolicyBotLogin {
		return human("policy_absent", "") // policy-bot down, or a status someone else posted
	}
	switch state {
	case "ERROR":
		return human("gate_path", "gate") // unmergeable, even with an approval (S9)
	case "SUCCESS":
	default:
		return human("policy_pending", "review")
	}
	if in.PR.Author != in.Cfg.AgentsLogin {
		return human("not_agent_authored", "")
	}
	for _, rv := range in.PR.Reviews {
		if rv.State == "APPROVED" && in.Cfg.IsMaintainer(rv.Author) {
			return human("maintainer_approved", "review") // success came from a human: a human merges
		}
	}
	cl := in.Cfg.Classes[class]
	if !cl.Live && !cl.Shadow {
		return human("class_mismatch", "live") // the diff matched a live class the triage did not predict
	}
	if slices.Contains(in.Cfg.Templates[in.Task.Spec.Template].Roles, "reviewer") && in.Task.Status.Verdict != "approve" {
		return human("no_approving_verdict", class)
	}
	// R02: the rule can only speak about the commit the room named. An unreported head — a run
	// pushed without a handoff, or a push after the last report, the trailer inside the head
	// notwithstanding — is a commit nobody watched.
	if in.ReportedHead == "" || in.ReportedHead != in.PR.HeadSHA {
		return human("head_unreported", class)
	}
	// R03: an approve is of the head its run was given, and of it only. If the verifier's last
	// approve predates the head the room just reported, the approval was pulled out from under.
	for i := len(in.Verifiers) - 1; i >= 0; i-- {
		r := in.Verifiers[i]
		if r.Verdict != "approve" || (r.Role != "reviewer" && r.Role != "tester") {
			continue
		}
		if r.HeadSHA != in.PR.HeadSHA {
			return human("verdict_stale", class)
		}
		break
	}
	trailer := in.PR.Trailer("Agent-Run")
	if !slices.ContainsFunc(in.Task.Status.Runs, func(r v1alpha1.RunRecord) bool { return r.ID == trailer }) {
		return human("foreign_trailer", class) // SC-14, SP1 R9: another task's run pushed here
	}
	// R52: the two entry classes carry a promise about the diff, not just the head: a docs-links
	// or revert change that moved anything but link targets is not the class it claims.
	if class == "docs-links" || class == "revert" {
		if ok, _ := LinksOnly(in.Files.Base, in.Files.Head); !ok {
			return human("class_mismatch", class)
		}
	}
	if in.Paused {
		return human("class_paused", class)
	}
	if in.ArmedToday >= in.Cfg.Merge.AutoMergesPerDay {
		return human("auto_merge_cap", class)
	}
	if !cl.Live { // R32: every condition holds, and the gate is in shadow until the wave
		return ArmDecision{Verdict: "shadow", Reason: "shadow_would_arm", Matched: class}
	}
	return ArmDecision{Verdict: "arm", Matched: class}
}

// reLinkTarget matches one link's target: a reference definition's ([label]: target at a line
// start) or an inline one's (](target), angle-bracketed or bare). Titles, labels and every other
// byte stay in the compared text, so a change beside a link is visible. Deliberately not a full
// markdown parser: a diff that hides a change inside what this reads as a link target is refused
// by the target rule below, and the classes it gates stay shadow until the wave re-reads this
// against a month of forecasts (task 7.9, R52).
var reLinkTarget = regexp.MustCompile(`(?m)^ {0,3}\[[^()\]\r\n]+\]:[ \t]*(<[^<>\r\n]*>|\S+)|\]\((<[^<>\r\n]*>|[^\s()<]*)`)

// linkPlaceholder blanks a target so the rest of the text compares byte for byte. NUL: not in
// any file a class like this edits, and counted below if one ever appears.
const linkPlaceholder = "\x00"

// linksOf is the text with every link target outside a code fence replaced by the placeholder,
// and those targets in order of appearance. Fenced lines are left unsubstituted on purpose:
// fenced content is code, and a docs-links diff that moved a link inside a fence moved code,
// which the byte compare below then sees.
func linksOf(s string) (string, []string) {
	var out []string
	m := reLinkTarget.FindAllStringSubmatchIndex(s, -1)
	if m == nil {
		return s, nil
	}
	fences := fencesOf(s)
	var b strings.Builder
	last := 0
	for _, ix := range m {
		if inFence(fences, ix[0]) {
			continue // the fenced text still gets written: last never jumps over it
		}
		start, end := ix[4], ix[5] // the inline alternative
		if ix[2] >= 0 {
			start, end = ix[2], ix[3] // the reference-definition alternative
		}
		b.WriteString(s[last:start])
		b.WriteString(linkPlaceholder)
		t := s[start:end]
		if len(t) >= 2 && strings.HasPrefix(t, "<") {
			t = t[1 : len(t)-1]
		}
		out = append(out, t)
		last = end
	}
	b.WriteString(s[last:])
	return b.String(), out
}

// fencesOf are the byte ranges of ```-fenced blocks, fence lines included. An unbalanced
// fence runs to the end of the text: its links go uncounted, which fails the byte compare
// for any change there — the safe direction.
func fencesOf(s string) [][2]int {
	var out [][2]int
	in, start := false, 0
	for off := 0; off < len(s); {
		end := strings.IndexByte(s[off:], '\n')
		if end < 0 {
			end = len(s)
		} else {
			end += off
		}
		if strings.HasPrefix(strings.TrimLeft(s[off:end], " \t"), "```") {
			if in {
				out = append(out, [2]int{start, end})
			} else {
				start, in = off, true
			}
		}
		if end == len(s) {
			break
		}
		off = end + 1
	}
	if in {
		out = append(out, [2]int{start, len(s)})
	}
	return out
}

func inFence(ranges [][2]int, off int) bool {
	return slices.ContainsFunc(ranges, func(r [2]int) bool { return r[0] <= off && off < r[1] })
}

// targetKind is a link target's scheme and host, with ("", "") only for a target relative to
// the current document. A schemeless one with a host is protocol-relative — host-absolute, and
// its non-empty host keeps it out of the relative clause. An unparseable one gets a scheme of
// its own so no pair can match it.
func targetKind(t string) (scheme, host string) {
	u, err := url.Parse(t)
	if err != nil {
		return "!", ""
	}
	if u.Scheme == "" && u.Host == "" {
		return "", "" // a path, a query, a fragment: relative
	}
	return strings.ToLower(u.Scheme), strings.ToLower(u.Host)
}

// retargetOK is R52's rule for a changed pair: relative to relative — hosts empty on both sides,
// so a protocol-relative //host is never "relative" — or https staying on its host, or landing
// on the archive. Never another scheme, never a host jump.
func retargetOK(from, to string) bool {
	fs, fh := targetKind(from)
	ts, th := targetKind(to)
	switch {
	case fs == "" && fh == "" && ts == "" && th == "":
		return true
	case fs == "https" && ts == "https" && (fh == th || th == "web.archive.org"):
		return true
	default:
		return false
	}
}

// LinksOnly is R52's promise of the docs-links and revert classes: between two commits of the
// tree, every changed file changed only its link targets, the counts are equal, and every
// retarget moves relatively, keeps its host, or lands on web.archive.org. The bool decides;
// the string is the first violation, named for the shadow forecast's record (task 7.9).
func LinksOnly(base, head map[string]string) (bool, string) {
	paths := make([]string, 0, len(base)+len(head))
	for p := range base {
		paths = append(paths, p)
	}
	for p := range head {
		if _, ok := base[p]; !ok {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	for _, p := range paths {
		b, h := base[p], head[p]
		if b == h {
			continue
		}
		nb, tb := linksOf(b)
		nh, th := linksOf(h)
		if nb != nh {
			return false, p + ": changed beyond link targets"
		}
		if len(tb) != len(th) { // redundant with nb == nh unless the text holds a real NUL
			return false, p + ": added or removed a link"
		}
		for i := range tb {
			if tb[i] != th[i] && !retargetOK(tb[i], th[i]) {
				return false, p + ": retargeted " + tb[i] + " to " + th[i]
			}
		}
	}
	return true, ""
}
