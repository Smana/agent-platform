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
	rec, err := r.recordedContract(ctx, t)
	if err != nil {
		return nil, "", err
	}
	if len(ps) == 0 {
		note, err := r.testerNote(ctx, t)
		if err != nil {
			return nil, "", err
		}
		// A run that succeeded, whose harness reported its commit or said the contract was ready, has a
		// push GitHub has not recorded yet; a run that failed with no note pushed none.
		pending := rec.n > 0 || note == "contract_ready" || (note == "" && cur.Phase == "Succeeded")
		if pending && cur.Finished != nil && r.Now().Sub(cur.Finished.Time) < contractPushWait {
			return nil, "wait", nil
		}
		if note == "" || note == "contract_ready" {
			note = "unreported"
		}
		return nil, note, nil
	}
	// The push record is GitHub's, but a model can push too (the credential helper stays): a push the
	// agents App made while a tester run held the room is not therefore the harness's. Adopted only
	// when it is one creation, and the commit is the very one the harness reported through room_contract
	// (a tool the model cannot call), on the base the harness recorded, with the fixed subject, and
	// its trailers name exactly the files it changes.
	tip := ps[0].After
	pc, err := r.pushedBy(ctx, branch, []string{r.Cfg.AgentsLogin}, tip)
	if err != nil {
		return nil, "", err
	}
	if pc != pushFactory || len(ps) != 1 || ps[0].Type != "branch_creation" || !sha40.MatchString(tip) || rec.n != 1 || rec.commit != tip {
		return nil, "contract_invalid", nil
	}
	cm, err := r.gh(ctx).Commit(ctx, tip)
	if err != nil {
		return nil, "", err
	}
	files, ids := cm.Trailers("Agent-Contract-File"), cm.Trailers("Agent-Contract-Id")
	if cm.Trailer("Agent-Contract") != cur.ID || cm.Trailer("Agent-Run") != cur.ID || cm.Subject() != contractSubject ||
		len(cm.Parents) != 1 || cm.Parents[0] != rec.base || cm.FilesTruncated || len(files) == 0 || len(ids) == 0 ||
		slices.Contains(files, "") || slices.Contains(ids, "") || !sameSet(files, cm.Files) {
		return nil, "contract_invalid", nil
	}
	if !within(files) || !within(ids) {
		return nil, "contract_too_large", nil // a bound the CRD holds, never a truncated contract
	}
	return &v1alpha1.ContractStatus{RunID: cur.ID, Commit: tip, Files: files, IDs: ids}, "", nil
}

// recorded is what the tester harness reported through room_contract: how many distinct reports, and the
// commit and base of the last.
type recorded struct {
	n            int
	commit, base string
}

// recordedContract reads the current run's room_contract events: the broker stamps their origin from
// the run's credential and only the harness's client may call the tool, so the model cannot write one.
func (r *Reconciler) recordedContract(ctx context.Context, t *v1alpha1.Task) (recorded, error) {
	cur := current(t)
	evs, _, err := r.roomTail(ctx, t.Status.RoomRef, cur.StartSeq, func(e envelope.Event) bool {
		return e.Type == envelope.Message && e.RunID == cur.ID && e.Origin == envelope.OriginClient &&
			e.Actor.Kind == envelope.ActorAgent && e.Actor.ID == "agent:"+cur.ID && e.Actor.Role == "tester"
	})
	if err != nil {
		return recorded{}, err
	}
	var out recorded
	seen := map[string]bool{}
	for _, e := range evs {
		var p envelope.MessagePayload
		if json.Unmarshal(e.Payload, &p) != nil || p.Kind != envelope.KindContract {
			continue
		}
		if k := p.Commit + "/" + p.Base; !seen[k] {
			seen[k] = true
			out.n++
		}
		out.commit, out.base = p.Commit, p.Base
	}
	return out, nil
}

// sameSet: a and b hold the same strings, whatever their order or repeats.
func sameSet(a, b []string) bool {
	in := func(x string, l []string) bool { return slices.Contains(l, x) }
	for _, x := range a {
		if !in(x, b) {
			return false
		}
	}
	for _, x := range b {
		if !in(x, a) {
			return false
		}
	}
	return true
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
