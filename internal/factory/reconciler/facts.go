// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/factory/rooms"
	"github.com/Smana/maku/internal/factory/runs"
)

// factsOf is what the room's summary shows of a task: phase, current run, budget, issue, PR.
func factsOf(t *v1alpha1.Task) envelope.TaskFacts {
	f := envelope.TaskFacts{Phase: t.Status.Phase, Reason: t.Status.Reason, Origin: t.Spec.Origin}
	// An unfinished record is over (M4) when a later run superseded it (a retry from Escalated), ended
	// at the next run's start; or, for the last one, when the task ended or escalated as stuck, at its
	// phase's start. Otherwise a view would read a dead run as running.
	over := v1alpha1.TerminalPhase(t.Status.Phase) || t.Status.Phase == v1alpha1.PhaseEscalated
	fact := func(i int) envelope.RunFact {
		r := t.Status.Runs[i]
		rf := runFact(r)
		if r.Finished != nil {
			return rf
		}
		var end *metav1.Time
		switch {
		case i < len(t.Status.Runs)-1:
			end = t.Status.Runs[i+1].Started
		case over:
			end = t.Status.PhaseSince
		default:
			return rf
		}
		rf.Outcome = envelope.OutcomeEnded
		if end != nil && !end.UTC().Truncate(time.Second).Before(rf.StartedAt) {
			rf.EndedAt = end.UTC().Truncate(time.Second)
		}
		return rf
	}
	if len(t.Status.Runs) > 0 {
		rf := fact(len(t.Status.Runs) - 1)
		run := current(t)
		if p := run.Provenance; !p.IsZero() {
			rf.Provenance = &envelope.ProvenanceFact{FactoryVersion: p.FactoryVersion, CompositionRevision: p.CompositionRevision,
				HarnessImage: p.HarnessImage, ServedModels: slices.Clone(p.ServedModels)}
		}
		f.Run = &rf
		for i := max(0, len(t.Status.Runs)-envelope.MaxRuns); i < len(t.Status.Runs); i++ {
			f.Runs = append(f.Runs, fact(i))
		}
	}
	// Known limit: a task mixing priced and unpriced runs shows the priced dollars only, while its token cap binds.
	if u, limit := t.Status.Usage.CostMicroUSD, t.Spec.Budget.TaskMicroUSD; u > 0 || limit > 0 {
		f.Spend = &envelope.SpendFact{UsedMicroUSD: dollars(u), LimitMicroUSD: dollars(limit)}
	}
	if t.Spec.Budget.TaskTokens > 0 {
		f.Budget = &envelope.BudgetFact{UsedTokens: t.Status.Usage.Tokens, LimitTokens: t.Spec.Budget.TaskTokens}
	}
	if t.Spec.Issue > 0 {
		f.Issue = &envelope.IssueFact{Number: t.Spec.Issue,
			URL:   fmt.Sprintf("https://github.com/%s/issues/%d", t.Spec.Repository, t.Spec.Issue),
			Title: issueTitle(t.Spec.Text), Author: t.Spec.IssueAuthor}
		if login, ok := strings.CutPrefix(t.Spec.Source.RequestedBy, "github:"); ok { // system:runlore labelled nothing
			f.Issue.LabelledBy = login
		}
	}
	if pr := t.Status.PullRequest; pr != nil && pr.Number > 0 {
		f.PR = &envelope.PRFact{Number: pr.Number, URL: pr.URL, Author: pr.Author, Reviewers: pr.Reviewers}
	}
	return f
}

// runFact is a run record as the room's facts carry it, without provenance (only the current run has it).
// Times are as the CRD stores them, so the hash of a task read back matches the one posted.
func runFact(r v1alpha1.RunRecord) envelope.RunFact {
	f := envelope.RunFact{ID: r.ID, Role: r.Role, Trigger: r.Trigger, Verdict: r.Verdict, MicroUSD: dollars(r.CostMicroUSD), Outcome: outcomeOf(r)}
	if r.Started != nil {
		f.StartedAt = r.Started.UTC().Truncate(time.Second)
	}
	if r.Finished != nil {
		f.EndedAt = r.Finished.UTC().Truncate(time.Second)
	}
	return f
}

// outcomeOf is how a run ended (R3), empty while it has not (no Finished). A lost run is lost under
// any phase; otherwise the AgentRun's phase decides, so PodFailed under Failed is failed.
func outcomeOf(r v1alpha1.RunRecord) string {
	if r.Finished == nil {
		return ""
	}
	switch r.Reason {
	case runs.ReasonDisrupted, runs.ReasonPodLost, "pod_lost", "run_lost":
		return envelope.OutcomeLost
	}
	switch r.Phase {
	case "Succeeded":
		return envelope.OutcomeSucceeded
	case "Failed":
		return envelope.OutcomeFailed
	case "BudgetExhausted":
		return envelope.OutcomeBudgetExhausted
	case "Revoked":
		return envelope.OutcomeRevoked
	}
	return envelope.OutcomeEnded
}

// maxTitle bounds the title the facts carry, in runes.
const maxTitle = 120

// issueTitle is the first line of the snapshot ("# title", intake.Snapshot), bounded: the
// room list shows it, and the Room status keeps it.
func issueTitle(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	title, ok := strings.CutPrefix(line, "# ")
	if !ok {
		return ""
	}
	title = strings.Join(strings.Fields(title), " ")
	if r := []rune(title); len(r) > maxTitle {
		title = string(r[:maxTitle]) + "…"
	}
	return title
}

// factsKey is what decides that the room's facts changed. Token usage counts in steps (R12): 10% of
// the cap, an overrun one step, or 100 k tokens without a cap; dollars step the same way, $0.10 without one. Every meter tick would otherwise post
// facts and write status twice, and a room seals at its event limit. Facts posted carry the exact
// count.
func factsKey(f envelope.TaskFacts) string {
	if b := f.Budget; b != nil {
		step := *b
		if step.LimitTokens > 0 {
			step.UsedTokens = min(step.UsedTokens*10/step.LimitTokens, 10)
		} else {
			step.UsedTokens /= 100_000
		}
		f.Budget = &step
	}
	if sp := f.Spend; sp != nil {
		step := *sp
		if step.LimitMicroUSD > 0 {
			step.UsedMicroUSD = min(step.UsedMicroUSD*10/step.LimitMicroUSD, 10)
		} else {
			step.UsedMicroUSD /= 100_000
		}
		f.Spend = &step
	}
	// A run's dollars count once it has ended: a running run's tick never posts (R4).
	f.Run = settled(f.Run)
	f.Runs = slices.Clone(f.Runs)
	for i := range f.Runs {
		f.Runs[i] = *settled(&f.Runs[i])
	}
	b, _ := json.Marshal(f) // strings, ints, a time and slices: Marshal cannot fail
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// settled is the run with its dollars left out while it has not ended.
func settled(r *envelope.RunFact) *envelope.RunFact {
	if r == nil || !r.EndedAt.IsZero() || r.Outcome != "" {
		return r
	}
	c := *r
	c.MicroUSD = 0
	return &c
}

// factsDue reports facts the room does not have yet.
func (r *Reconciler) factsDue(t *v1alpha1.Task) bool {
	if t.Status.RoomRef == "" || r.Rooms == nil {
		return false
	}
	l := t.Status.Facts
	return l == nil || !l.Posted || l.Hash != factsKey(factsOf(t))
}

// postFacts writes the task's facts into its room. New facts take roomSeq + 1, persisted before
// the post (ruling SK); a retry of the same facts resends that seq, which the broker keeps once.
// The caller's status write records Posted. A room the broker has no log for yet, or a broker that
// does not allow the factory yet (FR-1), is a wait, as for the snapshot: the facts stay due. So is a
// sealed room: it never takes them, and an error would hold the task in backoff instead of its poll.
func (r *Reconciler) postFacts(ctx context.Context, t *v1alpha1.Task) error {
	if !r.factsDue(t) {
		return nil
	}
	f := factsOf(t)
	h := factsKey(f)
	if l := t.Status.Facts; l == nil || l.Hash != h {
		prevSeq, prevLedger := t.Status.RoomSeq, t.Status.Facts
		t.Status.RoomSeq++
		t.Status.Facts = &v1alpha1.FactsLedger{Seq: t.Status.RoomSeq, Hash: h}
		if err := r.Client.Status().Update(ctx, t); err != nil {
			t.Status.RoomSeq, t.Status.Facts = prevSeq, prevLedger
			return fmt.Errorf("facts: persist room seq: %w", err)
		}
	}
	err := r.Rooms.TaskFacts(ctx, t.Status.RoomRef, f, t.Status.Facts.Seq)
	switch {
	case errors.Is(err, rooms.ErrNoRoute):
		// A broker older than the factory (R28): every task would say so on every reconcile.
		r.oldBroker.Do(func() {
			r.log().Warn("task facts wait for a broker that serves them: upgrade the broker", "err", err)
		})
		return nil
	case errors.Is(err, rooms.ErrNoRoom), errors.Is(err, rooms.ErrNotPermitted), errors.Is(err, rooms.ErrSealed):
		r.log().Info("task facts wait for the broker", "task.id", t.Name, "seq", t.Status.Facts.Seq, "err", err)
		return nil
	case err != nil:
		return fmt.Errorf("facts: room %s seq %d: %w", t.Status.RoomRef, t.Status.Facts.Seq, err)
	}
	t.Status.Facts.Posted = true
	return nil
}

// dollars is a micro-USD amount clamped to the envelope's bound: a task past it still posts its facts,
// read as the cap.
func dollars(v int64) int64 { return min(max(v, 0), envelope.MaxMicroUSD) }
