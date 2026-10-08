// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/rooms"
)

// factsOf is what the room's summary shows of a task: phase, current run, budget, issue, PR.
func factsOf(t *v1alpha1.Task) envelope.TaskFacts {
	f := envelope.TaskFacts{Phase: t.Status.Phase, Reason: t.Status.Reason}
	if len(t.Status.Runs) > 0 {
		run := current(t)
		f.Run = &envelope.RunFact{ID: run.ID, Role: run.Role, Trigger: run.Trigger}
		if run.Started != nil {
			// As the CRD stores it, so the hash of a task read back matches the one posted.
			f.Run.StartedAt = run.Started.UTC().Truncate(time.Second)
		}
	}
	if t.Spec.Budget.TaskTokens > 0 {
		f.Budget = &envelope.BudgetFact{UsedTokens: t.Status.Usage.Tokens, LimitTokens: t.Spec.Budget.TaskTokens}
	}
	if t.Spec.Issue > 0 {
		f.Issue = &envelope.IssueFact{Number: t.Spec.Issue,
			URL:    fmt.Sprintf("https://github.com/%s/issues/%d", t.Spec.Repository, t.Spec.Issue),
			Author: t.Spec.IssueAuthor}
		if login, ok := strings.CutPrefix(t.Spec.Source.RequestedBy, "github:"); ok { // system:runlore labelled nothing
			f.Issue.LabelledBy = login
		}
	}
	if pr := t.Status.PullRequest; pr != nil && pr.Number > 0 {
		f.PR = &envelope.PRFact{Number: pr.Number, URL: pr.URL, Author: pr.Author, Reviewers: pr.Reviewers}
	}
	return f
}

func factsHash(f envelope.TaskFacts) string {
	b, _ := json.Marshal(f) // strings, ints, a time and slices: Marshal cannot fail
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// factsDue reports facts the room does not have yet.
func (r *Reconciler) factsDue(t *v1alpha1.Task) bool {
	if t.Status.RoomRef == "" || r.Rooms == nil {
		return false
	}
	l := t.Status.Facts
	return l == nil || !l.Posted || l.Hash != factsHash(factsOf(t))
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
	h := factsHash(f)
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
	case errors.Is(err, rooms.ErrNoRoom), errors.Is(err, rooms.ErrNotPermitted), errors.Is(err, rooms.ErrSealed):
		r.log().Info("task facts wait for the broker", "task.id", t.Name, "seq", t.Status.Facts.Seq, "err", err)
		return nil
	case err != nil:
		return fmt.Errorf("facts: room %s seq %d: %w", t.Status.RoomRef, t.Status.Facts.Seq, err)
	}
	t.Status.Facts.Posted = true
	return nil
}
