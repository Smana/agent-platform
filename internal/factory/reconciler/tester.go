// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/narrate"
	"github.com/Smana/agent-platform/internal/factory/runs"
	"sigs.k8s.io/yaml"
)

// testerDue (F3-R1): the class writes its tests first, the task is public and holds no contract, and no
// tester ran yet, or the last one was lost and resumes. Records, not a status field, say the stage ran:
// a /factory retry after the tester's end does not run it twice.
func (r *Reconciler) testerDue(t *v1alpha1.Task) bool {
	if !r.repo(t).TestFirstFor(t.Spec.PredictedClass) || t.Spec.DataClass != "public" || t.Status.Contract != nil ||
		!slices.Contains(r.Cfg.Templates[t.Spec.Template].Roles, "implementer") {
		return false
	}
	if len(t.Status.Runs) > 0 && current(t).Role == "tester" {
		return nextTrigger(t) == "resume"
	}
	return !slices.ContainsFunc(t.Status.Runs, func(x v1alpha1.RunRecord) bool { return x.Role == "tester" })
}

// testerRun: the task's current run is its tester's.
func testerRun(t *v1alpha1.Task) bool { return len(t.Status.Runs) > 0 && current(t).Role == "tester" }

// testerSpec (F3-R2): the implementer's claim, on the task's own model and caps (R19), as a tester.
func (r *Reconciler) testerSpec(t *v1alpha1.Task, text string) runs.Spec {
	s := r.implementerSpec(t, text)
	s.Role = "tester"
	r.withChecks(&s, t)
	return s
}

// startTester starts the tester run, or resumes a lost one with its own brief.
func (r *Reconciler) startTester(ctx context.Context, t *v1alpha1.Task) error {
	text, ok, err := r.resumeBrief(ctx, t)
	if err != nil {
		return err
	}
	if !ok {
		sc, err := r.taskScope(ctx, t)
		if err != nil {
			return err
		}
		text = TesterBrief(t, r.Nonce(), sc)
	}
	return r.startRun(ctx, t, r.testerSpec(t, text), nextTrigger(t))
}

// contractPushWait is how long after the tester run's end GitHub's record of its push is waited for
// (F3-R4): the activity API trails a push by a moment.
const contractPushWait = 5 * time.Minute

// contractSubject is the fixed subject of the harness's contract commit (F1-R4).
const contractSubject = "test: the test contract"

// The CRD's bounds on status.contract: over them is contract_too_large, never a truncated contract.
const (
	maxContractItems = 50
	maxContractBytes = 512
)

var sha40 = regexp.MustCompile(`^[0-9a-f]{40}$`)

// testerEnds are the categories of the harness's end note a tester run names (F1): the note only
// names the category, never decides the contract.
var testerEnds = []string{"contract_ready", "not_testable", "contract_not_red", "contract_outside_tests"}

// testerEnded (R14, F3-R3..R5): a person or a budget stopping it escalates as any run; otherwise GitHub
// says whether the tester's harness pushed a contract.
func (r *Reconciler) testerEnded(ctx context.Context, t *v1alpha1.Task, reason string) error {
	if slices.Contains(escalatesScoper, reason) {
		return r.end(ctx, t, v1alpha1.PhaseEscalated, reason)
	}
	c, end, err := r.readContract(ctx, t)
	switch {
	case err != nil:
		return err
	case end == "wait":
		t.Status.Reason = "contract_push_unrecorded"
		return nil
	case end == "contract_invalid" || end == "contract_too_large":
		record(ctx, func(ctx context.Context) { r.Metrics.ContractEnd(ctx, t.Spec.PredictedClass, end) })
		return r.end(ctx, t, v1alpha1.PhaseEscalated, end)
	case c != nil:
		t.Status.Contract = c
		end = "ready"
		narrateLater(t, narrate.ContractReady(t, c))
	default:
		narrateLater(t, narrate.NoContract(t, end))
	}
	record(ctx, func(ctx context.Context) { r.Metrics.ContractEnd(ctx, t.Spec.PredictedClass, end) })
	t.Status.NextTrigger = "initial"
	r.to(t, v1alpha1.PhaseQueued, "")
	return nil
}

// readContract is F3-R4: the contract the tester's push made, from GitHub's record of the push and
// the commit's trailers, or the end when there is none: "wait" (the push is not recorded yet),
// "contract_invalid", the end note's category, or "unreported".
func (r *Reconciler) readContract(ctx context.Context, t *v1alpha1.Task) (*v1alpha1.ContractStatus, string, error) {
	cur := current(t)
	branch := "agent/" + t.Name
	ps, err := r.gh(ctx).BranchActivity(ctx, branch)
	switch {
	case errors.Is(err, forge.ErrActivityTruncated):
		return nil, "contract_invalid", nil
	case err != nil:
		return nil, "", err
	}
	if len(ps) == 0 {
		note, err := r.testerNote(ctx, t)
		if err != nil {
			return nil, "", err
		}
		// A run that succeeded, or whose harness said the contract was ready, has a
		// push GitHub has not recorded yet; a run that failed with no note pushed none.
		pending := note == "contract_ready" || (note == "" && cur.Phase == "Succeeded")
		if pending && cur.Finished != nil && r.Now().Sub(cur.Finished.Time) < contractPushWait {
			return nil, "wait", nil
		}
		if note == "" || note == "contract_ready" {
			note = "unreported"
		}
		return nil, note, nil
	}
	// Nothing here trusts who pushed: the credential helper stays in the sandbox, so a push by the agents
	// App may be the model's own. The contract is verified by what GitHub holds: one creation, a commit
	// of the fixed subject on a parent that is on the default branch, whose every change is a plain
	// addition or edit of a test file under the declaration's tests.paths, read at that parent, and whose
	// trailers name exactly those files. That the tests are red on the base is proven at merge, by the
	// required contract check (row 2b), not here.
	tip := ps[0].After
	pc, err := r.pushedBy(ctx, branch, []string{r.Cfg.AgentsLogin}, tip)
	if err != nil {
		return nil, "", err
	}
	if pc != pushFactory || len(ps) != 1 || ps[0].Type != "branch_creation" || !sha40.MatchString(tip) {
		return nil, "contract_invalid", nil
	}
	cm, err := r.gh(ctx).Commit(ctx, tip)
	if err != nil {
		return nil, "", err
	}
	files, ids := cm.Trailers("Agent-Contract-File"), cm.Trailers("Agent-Contract-Id")
	if cm.Trailer("Agent-Contract") != cur.ID || cm.Trailer("Agent-Run") != cur.ID || cm.Subject() != contractSubject ||
		len(cm.Parents) != 1 || cm.FilesTruncated || len(files) == 0 || len(ids) == 0 ||
		slices.Contains(files, "") || slices.Contains(ids, "") {
		return nil, "contract_invalid", nil
	}
	if ok, err := r.onDefaultBranch(ctx, cm.Parents[0]); err != nil {
		return nil, "", err
	} else if !ok {
		return nil, "contract_invalid", nil
	}
	if ok, err := r.testsOnly(ctx, cm, files); err != nil {
		return nil, "", err
	} else if !ok {
		return nil, "contract_invalid", nil
	}
	if !within(files) || !within(ids) {
		return nil, "contract_too_large", nil // a bound the CRD holds, never a truncated contract
	}
	return &v1alpha1.ContractStatus{RunID: cur.ID, Commit: tip, Files: files, IDs: ids}, "", nil
}

// defaultBranch is the ref a contract's parent must be on: the base every run is cut from.
const defaultBranch = "main"

// onDefaultBranch: sha is the default branch's head or one of its ancestors, so a parent the model crafted
// (carrying changes that never went through review) is not one.
func (r *Reconciler) onDefaultBranch(ctx context.Context, sha string) (bool, error) {
	if !sha40.MatchString(sha) {
		return false, nil
	}
	return r.merger(ctx).IsAncestor(ctx, sha, defaultBranch)
}

// checksDecl is the part of .github/agent-checks.yaml a contract is held to: the paths a tester may write
// and the runner's own files, which are never a contract.
type checksDecl struct {
	Tests struct {
		Paths  []string `json:"paths"`
		Runner []string `json:"runner"`
	} `json:"tests"`
}

// underPath is the declaration's rule: the path itself, or any path under a prefix ending in "/".
func underPath(path string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool { return path == p || (strings.HasSuffix(p, "/") && strings.HasPrefix(path, p)) })
}

// testsOnly: the declaration at the contract's parent (never the change's own) names the paths, and every
// file of the commit is added or modified, under tests.paths and not a runner file, and the trailers'
// files are exactly them. An absent or unreadable declaration is a refusal.
func (r *Reconciler) testsOnly(ctx context.Context, cm forge.Commit, trailers []string) (bool, error) {
	raw, err := r.merger(ctx).FileAt(ctx, ".github/agent-checks.yaml", cm.Parents[0])
	if err != nil {
		return false, err
	}
	var d checksDecl
	if raw == "" || yaml.Unmarshal([]byte(raw), &d) != nil || len(d.Tests.Paths) == 0 {
		return false, nil
	}
	var changed []string
	for _, f := range cm.Stats {
		if (f.Status != "added" && f.Status != "modified") || f.Previous != "" ||
			!underPath(f.Path, d.Tests.Paths) || underPath(f.Path, d.Tests.Runner) {
			return false, nil
		}
		changed = append(changed, f.Path)
	}
	return len(changed) > 0 && sameSet(changed, trailers), nil
}

// sameSet: a and b hold the same strings, whatever their order or repeats.
func sameSet(a, b []string) bool {
	return !slices.ContainsFunc(a, func(x string) bool { return !slices.Contains(b, x) }) &&
		!slices.ContainsFunc(b, func(x string) bool { return !slices.Contains(a, x) })
}

// within: no more values than the CRD holds, each of a bounded length.
func within(vs []string) bool {
	return len(vs) <= maxContractItems && !slices.ContainsFunc(vs, func(v string) bool { return len(v) > maxContractBytes })
}

// testerNote is the category the tester harness's end note names ("" for none): the room's progress
// message "Harness (tester): <end>: ..." of the current run. A hint only: the forge decides.
func (r *Reconciler) testerNote(ctx context.Context, t *v1alpha1.Task) (string, error) {
	cur := current(t)
	evs, _, err := r.roomTail(ctx, t.Status.RoomRef, cur.StartSeq, func(e envelope.Event) bool {
		return e.Type == envelope.Message && e.RunID == cur.ID
	})
	if err != nil {
		return "", err
	}
	note := ""
	for _, e := range evs {
		var p envelope.MessagePayload
		if json.Unmarshal(e.Payload, &p) != nil || p.Kind != envelope.KindProgress {
			continue
		}
		if rest, ok := strings.CutPrefix(p.Text, "Harness (tester): "); ok {
			if word, _, _ := strings.Cut(rest, ":"); slices.Contains(testerEnds, word) {
				note = word
			}
		}
	}
	return note, nil
}
