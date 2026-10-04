// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// StuckAfter is how long a Running run may show no room activity before the factory ends it (§6.3).
const StuckAfter = 10 * time.Minute

// stuck: a Running run with no room event for 10 minutes is deleted (§6.3). Activity starts
// counting when the factory first sees the run Running, so a cold node's 3 minutes are free.
func (r *Reconciler) stuck(ctx context.Context, t *v1alpha1.Task, run runs.Run) (bool, error) {
	if run.Phase != "Running" {
		return false, nil
	}
	cur := current(t)
	last := r.Now()
	// !Before, not After: the first observation stores Started itself when the run is seen at
	// once, and that value must count as this run's activity.
	if a := t.Status.LastActivity; a != nil && cur.Started != nil && !a.Before(cur.Started) {
		last = a.Time
	}
	// roomTail, not one EventsSince: a long room's newest activity is past a single 10,000-event
	// page, and a run busy in the room is exactly the run this must not end.
	evs, _, err := r.roomTail(ctx, t.Status.RoomRef, cur.StartSeq, func(e envelope.Event) bool { return e.RunID == run.ID })
	if err != nil {
		return false, err
	}
	for _, e := range evs {
		if e.TS.After(last) {
			last = e.TS
		}
	}
	lt := metav1.NewTime(last)
	t.Status.LastActivity = &lt
	if r.Now().Sub(last) < StuckAfter {
		return false, nil
	}
	if err := r.Runs.Annotate(ctx, run.ID, map[string]string{runs.AnnRevoked: "manual"}); err != nil {
		return false, err
	}
	return true, r.Runs.Delete(ctx, run.ID)
}

// agent_error is not here: it includes gateway and provider failures, which score nothing (R53).
var underReasons = []string{"stuck", "agent_stuck", "review_rounds_exhausted", "ci_red", "budget-run", "budget-task"}

// Fit is §7's after-the-fact score of the tier that ran: under when the task escalated for
// capability after a full attempt, over when it succeeded on < 20 % of the tier's task budget.
func Fit(t *v1alpha1.Task) string {
	switch {
	case t.Status.Phase == v1alpha1.PhaseEscalated && slices.Contains(underReasons, t.Status.Reason):
		return "under"
	case t.Status.Phase == v1alpha1.PhaseDone && t.Spec.Budget.TaskTokens > 0 && t.Status.Usage.Tokens*5 < t.Spec.Budget.TaskTokens:
		return "over"
	case t.Status.Phase == v1alpha1.PhaseDone:
		return "fit"
	}
	return ""
}

// TierScore is one classifier's tier rated against the tier that would have fit.
type TierScore struct{ Classifier, Tier, Fit string }

var tiers = []string{"light", "standard", "frontier"}

// Score rates every classifier, acting and shadow, against the tier that would have fit: a
// budget-fit heuristic (§7); the control group is not a counterfactual for lower tiers (R53).
func Score(cl v1alpha1.Classification, acting, fit string) []TierScore {
	right := slices.Index(tiers, acting)
	switch fit {
	case "under":
		right = min(right+1, 2)
	case "over":
		right = max(right-1, 0)
	}
	rate := func(tier string) string {
		switch i := slices.Index(tiers, tier); {
		case i == right:
			return "fit"
		case i < right:
			return "under"
		}
		return "over"
	}
	out := []TierScore{{Classifier: cl.Classifier, Tier: cl.Tier, Fit: rate(cl.Tier)}}
	for _, s := range cl.Shadow {
		out = append(out, TierScore{Classifier: s.Classifier, Tier: s.Tier, Fit: rate(s.Tier)})
	}
	return out
}

// outcome runs once per task, when it first ends: tier fit (SC-10) and the task.final line (SC-7).
func (r *Reconciler) outcome(ctx context.Context, t *v1alpha1.Task) {
	if cl := t.Status.Classification; cl != nil && cl.Fit == "" {
		if fit := Fit(t); fit != "" {
			cl.Fit = fit
			for _, s := range Score(*cl, t.Spec.Budget.Tier, fit) {
				record(ctx, func(ctx context.Context) { r.Metrics.TierFit(ctx, s.Classifier, s.Tier, s.Fit, cl.Control) })
			}
		}
	}
	if !v1alpha1.TerminalPhase(t.Status.Phase) {
		return
	}
	ids := make([]string, 0, len(t.Status.Runs))
	for _, x := range t.Status.Runs {
		ids = append(ids, x.ID)
	}
	var pr, mergedBy, classifier, tier string
	control := false
	if ref := t.Status.PullRequest; ref != nil {
		pr, mergedBy = ref.URL, ref.MergedBy
	}
	if cl := t.Status.Classification; cl != nil {
		classifier, tier, control = cl.Classifier, cl.Tier, cl.Control
	}
	r.log().Info("task.final", "task.id", t.Name, "source", t.Spec.Source.Kind, "key", t.Spec.Source.Key,
		"contentSHA256", t.Spec.Source.ContentSHA256, "classifier", classifier, "tier", tier, "control", control,
		"runIds", strings.Join(ids, ","), "room", t.Status.RoomRef, "pr", pr, "mergedBy", mergedBy,
		"phase", t.Status.Phase, "reason", t.Status.Reason, "tokens", t.Status.Usage.Tokens)
}

// interventions counts, once per run, how humans steered it from the room (§7).
func (r *Reconciler) interventions(ctx context.Context, t *v1alpha1.Task) {
	evs, _, err := r.roomTail(ctx, t.Status.RoomRef, current(t).StartSeq, func(e envelope.Event) bool {
		return e.Actor.Kind == envelope.ActorHuman || e.Type == envelope.Driver
	})
	if err != nil {
		return
	}
	for _, e := range evs {
		switch e.Type {
		case envelope.Message:
			if e.Actor.Kind != envelope.ActorHuman {
				continue
			}
			var p envelope.MessagePayload
			if json.Unmarshal(e.Payload, &p) == nil && p.Delivery == envelope.DeliverySteering {
				record(ctx, func(ctx context.Context) { r.Metrics.Intervention(ctx, "steer") })
			}
		case envelope.Driver:
			var p envelope.DriverPayload
			if json.Unmarshal(e.Payload, &p) == nil && strings.HasPrefix(p.To, "human:") {
				record(ctx, func(ctx context.Context) { r.Metrics.Intervention(ctx, "takeover") })
			}
		}
	}
}
