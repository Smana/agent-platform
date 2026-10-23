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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	roomv1 "github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/api"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/narrate"
	"github.com/Smana/agent-platform/internal/factory/rooms"
	"github.com/Smana/agent-platform/internal/factory/runs"
	"github.com/Smana/agent-platform/internal/factory/taskid"
	"github.com/Smana/agent-platform/internal/factory/tracing"
)

// runEndGrace is how long a finished run waits for the room's end reason before falling back to
// the AgentRun's phase: SP2 knows why a run ended, the AgentRun only says Failed.
const runEndGrace = time.Minute

// slotFree: the active-task and concurrent-run caps (§6.2), and never while a human drives (C4).
func (r *Reconciler) slotFree(ctx context.Context, t *v1alpha1.Task) (bool, string, error) {
	active, err := r.countTasks(ctx, func(o *v1alpha1.Task) bool { return o.Name != t.Name && v1alpha1.ActivePhase(o.Status.Phase) })
	if err != nil {
		return false, "", err
	}
	if active >= r.Cfg.Caps.ActiveTasks {
		return false, "waiting_active_tasks", nil
	}
	// A live class may auto-merge its PR, so human review WIP is not its bottleneck; the shadow
	// and unclassified work is exactly what lands on the review queue.
	if len(t.Status.Runs) == 0 && !r.repo(t).Classes[t.Spec.PredictedClass].Live {
		wip, err := r.countTasks(ctx, func(o *v1alpha1.Task) bool { return o.Status.Phase == v1alpha1.PhaseAwaitingHuman })
		if err != nil {
			return false, "", err
		}
		if wip >= r.Cfg.Caps.AwaitingHumanWIP {
			return false, "waiting_review_wip", nil // back-pressure on the reviewers (§6.2)
		}
	}
	all, err := r.Runs.List(ctx)
	if err != nil {
		return false, "", err
	}
	live := 0
	for _, x := range all {
		if runs.Terminal(x.Phase) {
			continue
		}
		if x.Principal == runs.PrincipalFactory {
			live++
		}
		// One live run per room (C3: a room's runs share its branch). The task's own was recorded
		// before this; anyone else's, a run a human started in the room, is waited for.
		if x.RoomRef == t.Status.RoomRef && x.TaskID != t.Name {
			return false, "waiting_room_busy", nil
		}
	}
	if live >= r.Cfg.Caps.ConcurrentRuns {
		return false, "waiting_run_slot", nil
	}
	var room roomv1.Room
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: t.Status.RoomRef}, &room); err != nil {
		return false, "", err
	}
	if rooms.HumanDriver(&room) {
		return false, "waiting_human_driver", nil
	}
	return true, "", nil
}

// runID is the deterministic id of a task's nth run (R48): the claim a replay recomputes, so a
// lost status write finds the run it created instead of minting another. A C2 id, like a Task
// name, so the claim's name is always <runs.namePrefix><id>.
func runID(task string, n int) string { return taskid.Run(task, n) }

// recordExisting records a claim the task's status does not know (R48): the run was created and
// the write that recorded it was lost, so the replay found the claim by its deterministic id.
// The claim is the record — its own role, start seq, head and tokens, whatever its phase — never
// the replay's spec, which may hold a newer brief or a later room seq.
func (r *Reconciler) recordExisting(t *v1alpha1.Task, x runs.Run) error {
	if x.TaskID != t.Name {
		return fmt.Errorf("record run %s of task %s: the id belongs to task %q", x.ID, t.Name, x.TaskID)
	}
	// A verifier's trigger is always "review", and the lost write is what cleared NextRole; an
	// implementer's survives in NextTrigger, which the same write would have cleared.
	trigger := nextTrigger(t)
	if x.Role == "reviewer" || x.Role == "tester" {
		trigger = "review"
	}
	started := metav1.NewTime(r.Now())
	if !x.Created.IsZero() {
		started = metav1.NewTime(x.Created) // when the claim really was created, not the replay
	}
	t.Status.Runs = append(t.Status.Runs, v1alpha1.RunRecord{ID: x.ID, Role: x.Role, Trigger: trigger,
		Round: t.Status.ReviewRounds, Phase: x.Phase, Tokens: x.Tokens, StartSeq: x.StartSeq, HeadSHA: x.Head,
		Started: &started, ChargedTo: x.ChargedTo})
	t.Status.NextTrigger, t.Status.NextRole = "", ""
	r.to(t, phaseFor(x.Role), "adopted")
	return nil
}

func phaseFor(role string) string {
	if role == "reviewer" || role == "tester" {
		return v1alpha1.PhaseReviewing
	}
	return v1alpha1.PhaseImplementing
}

// sourceURL is the issue a task narrates on, the harness footer's Agent-Task (ruling SF).
func sourceURL(t *v1alpha1.Task) string {
	if t.Spec.Issue == 0 {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/issues/%d", t.Spec.Repository, t.Spec.Issue)
}

// implementerSpec: every run of a task writes agent/<taskId> (C3); baseRef main, because the
// harness resumes origin/<branch> when an earlier run pushed it (SP1 R7).
func (r *Reconciler) implementerSpec(t *v1alpha1.Task, text string) runs.Spec {
	s := runs.Spec{TaskID: t.Name, Role: "implementer", Repository: t.Spec.Repository, BaseRef: "main",
		Branch: "agent/" + t.Name, TaskText: text, Principal: runs.PrincipalFactory, DataClass: t.Spec.DataClass,
		Model: t.Spec.Budget.Model, RoomRef: t.Status.RoomRef, SourceURL: sourceURL(t), Queue: runs.QueueFactory,
		MaxTokens: t.Spec.Budget.RunTokens, MaxMinutes: t.Spec.Budget.RunMinutes, Traceparent: traceparent(t), Tier: t.Spec.Budget.Tier}
	r.withChecks(&s, t)
	return s
}

// withChecks switches the repository's checks on for a public implementer or reviewer run of a class
// that has them (v3 feature 3, RC8): spec.loop.checks and the repository's checks.egress. Any other
// run, an internal one included, is today's.
func (r *Reconciler) withChecks(s *runs.Spec, t *v1alpha1.Task) {
	on, egress := r.repo(t).ChecksFor(t.Spec.PredictedClass)
	if !on || t.Spec.DataClass != "public" || (s.Role != "implementer" && s.Role != "reviewer") {
		s.Checks, s.EgressProfiles = false, nil
		return
	}
	s.Checks, s.EgressProfiles = true, egress
}

// reserveFactoryDay is §6.2's "SP3 at admission" for the factory's own principal (R34), admitted
// as the API admits (#78): the run about to start is reserved in the day's ledger under its
// deterministic id before its claim exists, so runs started on one reading cannot overshoot the
// day, and a replay re-reserves the same entry, never a second. The meter drops it when the run
// ends. No room: the factory's room exclusion is slotFree's, on the claims, and a reservation
// that names the room would hold the task's next run until the meter's next tick.
//
// With per-person caps on, the same entry also charges the person who caused the run (D2, D3).
func (r *Reconciler) reserveFactoryDay(ctx context.Context, t *v1alpha1.Task, limit int64) (string, error) {
	if r.Ledger == nil {
		return "", errors.New("the daily cap needs the ledger client")
	}
	person := r.person(t)
	return api.Reserve(ctx, r.Ledger, r.Namespace, r.Now(), runID(t.Name, len(t.Status.Runs)),
		api.Reservation{Principal: runs.PrincipalFactory, Person: person, MaxMicroUSD: runMicroUSD(t.Spec.Budget)}, limit,
		r.Cfg.Budgets.DailyMicroUSD(person), r.Cfg.Budgets.EnforcePrincipal)
}

// runMicroUSD is what one run of the task may spend, as the day ledger reserves it: its USD run
// cap, or its token cap at the reference rate for a tier capped in tokens (TC7).
func runMicroUSD(b v1alpha1.Budget) int64 {
	if b.RunMicroUSD > 0 {
		return b.RunMicroUSD
	}
	return config.RefMicroUSD(b.RunTokens)
}

// person is who the task's next run is charged to beside the fleet, "" with per-person caps off.
func (r *Reconciler) person(t *v1alpha1.Task) string {
	if !r.Cfg.Budgets.PerPerson.Enabled {
		return ""
	}
	return t.Payer()
}

// traceparent is the task span's W3C header for its runs (R46); empty when tracing is off.
func traceparent(t *v1alpha1.Task) string {
	if t.Status.Trace == nil {
		return ""
	}
	return tracing.Traceparent(t.Status.Trace.TraceID, t.Status.Trace.SpanID)
}

func (r *Reconciler) startRun(ctx context.Context, t *v1alpha1.Task, s runs.Spec, trigger string) error {
	// The room's lastSeq before the run: its handoff, verdict and end are read after it, never
	// from the start of a long room (EventsSince stops at 10,000 events). It rides the claim
	// (AnnStartSeq), so a replay that finds the claim records the seq the run was given.
	last, err := r.Rooms.LastSeq(ctx, t.Status.RoomRef)
	if err != nil {
		return err
	}
	s.RunID, s.StartSeq, s.ChargedTo = runID(t.Name, len(t.Status.Runs)), last, r.person(t)
	// The loop policy closes implementer runs only (L2); every one of them, revisions and CI fixes
	// included, carries the task's policy, read at triage.
	if l := t.Spec.Loop; l != nil && s.Role == "implementer" {
		s.Loop = &runs.Loop{StallSteps: l.StallSteps, Continuations: l.Continuations, JudgeModel: l.JudgeModel}
	}
	if err := r.Runs.Create(ctx, s); err != nil {
		// The deterministic id already has a claim: the run was created and its record lost
		// (R48). Recorded, never re-created.
		if !apierrors.IsAlreadyExists(err) {
			// The cluster refused a claim carrying spec.loop, checks included (Strict validation: the host's XRD lacks it).
			// Retrying cannot help; hand the task to a maintainer instead of backing off forever.
			if (s.Loop != nil || s.Checks) && (apierrors.IsInvalid(err) || apierrors.IsBadRequest(err)) {
				r.log().Warn("loop or repository checks claim refused by the cluster", "task.id", t.Name, "err", err)
				return r.end(ctx, t, v1alpha1.PhaseEscalated, "loop_unsupported")
			}
			return err
		}
		x, found, gerr := r.Runs.Get(ctx, s.RunID)
		if gerr != nil {
			return errors.Join(err, gerr)
		}
		if !found {
			return err
		}
		return r.recordExisting(t, x)
	}
	now := metav1.NewTime(r.Now())
	t.Status.Runs = append(t.Status.Runs, v1alpha1.RunRecord{ID: s.RunID, Role: s.Role, Trigger: trigger,
		Round: t.Status.ReviewRounds, StartSeq: last, HeadSHA: s.Head, Started: &now, ChargedTo: s.ChargedTo})
	t.Status.NextTrigger = ""
	r.to(t, phaseFor(s.Role), "")
	narrateLater(t, narrate.Started(t, s, r.Cfg.RoomsURL))
	return nil
}

// current is the task's last run record.
func current(t *v1alpha1.Task) *v1alpha1.RunRecord { return &t.Status.Runs[len(t.Status.Runs)-1] }

// observe lists the task's claims once and copies each one's usage into its record, so a
// reading the meter annotated after a run ended — the current run or any before it — still
// lands (R49). A record's tokens never go down, since a stale read of a run must not lower
// them, so neither does the sum; a deleted claim leaves its record's last reading in place.
// Nor does its phase go back to Pending: the composition reports a started run whose Sandbox
// is not Ready as Pending (its harness exited and the sidecars drain, or a probe fails), and
// the record is what remembers it was admitted.
func (r *Reconciler) observe(ctx context.Context, t *v1alpha1.Task) (runs.Run, bool, error) {
	if len(t.Status.Runs) == 0 {
		return runs.Run{}, false, nil
	}
	all, err := r.Runs.List(ctx)
	if err != nil {
		return runs.Run{}, false, err
	}
	x, found := absorb(t, all)
	r.price(ctx, t)
	return x, found, nil
}

// absorb is observe on claims already listed: each record takes its claim's usage, and the
// current run's claim is returned. A stop calls it before deleting the claims (#55).
func absorb(t *v1alpha1.Task, all []runs.Run) (runs.Run, bool) {
	claims := make(map[string]runs.Run, len(all))
	for _, x := range all {
		claims[x.ID] = x
	}
	cur := current(t)
	var sum int64
	for i := range t.Status.Runs {
		rec := &t.Status.Runs[i]
		if x, ok := claims[rec.ID]; ok {
			rec.Tokens = max(rec.Tokens, x.Tokens)
			stamp(&rec.Provenance, x)
			if rec.ID == cur.ID && (!pending(x.Phase) || pending(rec.Phase)) {
				rec.Phase = x.Phase
			}
		}
		sum += rec.Tokens
	}
	t.Status.Usage.Tokens = sum
	x, found := claims[cur.ID]
	return x, found
}

// pending is a claim that exists but never started: not admitted, no phase written yet.
func pending(phase string) bool { return phase == "" || phase == "Pending" }

// boundPending ends a run that never started (P): nothing else bounds Pending —
// activeDeadlineSeconds counts from the pod's start, and Kueue queues unadmitted work forever.
// A run still Pending past caps.maxPendingMinutes whose record never saw it start (observe) and
// whose claim carries no usage never ran, so deleting it loses no usage; the task escalates as
// run_unschedulable, and a maintainer's /factory retry starts a fresh run.
func (r *Reconciler) boundPending(ctx context.Context, t *v1alpha1.Task, run runs.Run) error {
	if !pending(current(t).Phase) || run.Tokens > 0 || run.Created.IsZero() ||
		r.Now().Sub(run.Created) < time.Duration(r.Cfg.Caps.MaxPendingMinutes)*time.Minute {
		return nil
	}
	if err := r.deleteRun(ctx, run.ID); err != nil {
		return err
	}
	return r.end(ctx, t, v1alpha1.PhaseEscalated, "run_unschedulable")
}

// deleteRun deletes a claim and releases its reservation (#55): a claim deleted while live
// never reaches the terminal phase the meter releases on. The release is best effort: end
// releases again by run id, and the meter's sweep is the floor under both.
func (r *Reconciler) deleteRun(ctx context.Context, id string) error {
	if err := r.Runs.Delete(ctx, id); err != nil {
		return err
	}
	r.release(ctx, id)
	return nil
}

// release drops ids' reservations from today's ledger, the only one admission reads. A failure
// is logged, never returned: an ending task must not wait on the ledger, and the meter releases
// an unclaimed reservation after its grace.
func (r *Reconciler) release(ctx context.Context, ids ...string) {
	if r.Ledger == nil || len(ids) == 0 {
		return
	}
	if err := api.Release(ctx, r.Ledger, r.Namespace, r.Now(), ids...); err != nil {
		r.log().Warn("reservation not released; the meter will", "run.ids", ids, "err", err)
	}
}

// releaseUnheld releases every reservation of the task that no claim holds (#55): its recorded
// runs whose claims are gone or being deleted, and the next run's id, which Queued may have
// reserved without its claim ever being created. By run id, so a replay of a stop whose claims
// are already gone still releases them. An ended claim's is the meter's, as before.
func (r *Reconciler) releaseUnheld(ctx context.Context, t *v1alpha1.Task) {
	if r.Ledger == nil {
		return
	}
	all, err := r.Runs.List(ctx)
	if err != nil {
		r.log().Warn("reservations not released; the meter will", "task.id", t.Name, "err", err)
		return
	}
	held := map[string]bool{}
	for _, x := range all {
		if !x.Deleting {
			held[x.ID] = true
		}
	}
	var ids []string
	for _, id := range append(recordIDs(t), runID(t.Name, len(t.Status.Runs))) {
		if !held[id] {
			ids = append(ids, id)
		}
	}
	r.release(ctx, ids...)
}

func recordIDs(t *v1alpha1.Task) []string {
	ids := make([]string, 0, len(t.Status.Runs)+1)
	for _, rec := range t.Status.Runs {
		ids = append(ids, rec.ID)
	}
	return ids
}

// roomReason is the broker's end reason for the task's current run, if the room has it.
func (r *Reconciler) roomReason(ctx context.Context, t *v1alpha1.Task) (string, bool) {
	cur := current(t)
	evs, _, err := r.roomTail(ctx, t.Status.RoomRef, cur.StartSeq, func(e envelope.Event) bool {
		return e.Type == envelope.StateChanged && e.RunID == cur.ID
	})
	if err != nil {
		r.log().Warn("room log unreadable", "task.id", t.Name, "run.id", cur.ID, "err", err)
		return "", false
	}
	end, ok := rooms.LastRunEnd(evs, cur.ID)
	return end.Reason, ok
}

// finished returns the run's end reason once the room has recorded it, or after runEndGrace
// without it ("" while waiting).
func (r *Reconciler) finished(ctx context.Context, t *v1alpha1.Task, run runs.Run) string {
	cur := current(t)
	if cur.Finished == nil {
		now := metav1.NewTime(r.Now())
		cur.Finished = &now
	}
	if reason, ok := r.roomReason(ctx, t); ok {
		return reason
	}
	if r.Now().Sub(cur.Finished.Time) < runEndGrace {
		return ""
	}
	if run.Revoked != "" {
		return run.Revoked
	}
	switch run.Reason {
	case runs.ReasonDisrupted, runs.ReasonPodLost, runs.ReasonPodFailed:
		return run.Reason // the composition read the pod (disruption design §3)
	}
	return strings.ToLower(run.Phase)
}

func (r *Reconciler) implementing(ctx context.Context, t *v1alpha1.Task) error {
	run, found, err := r.observe(ctx, t)
	if err != nil {
		return err
	}
	if !found {
		why := r.lostReason(ctx, t)
		if scoperRun(t) && why == "run_lost" { // SC11: a scoper whose claim vanished fails open
			return r.scopeEnd(ctx, t, "failed", 0)
		}
		return r.end(ctx, t, v1alpha1.PhaseEscalated, why)
	}
	if err := r.boundPending(ctx, t, run); err != nil {
		return err
	}
	if t.Status.PullRequest == nil {
		if err := r.detectPR(ctx, t, run); err != nil {
			return err
		}
	}
	if !runs.Terminal(run.Phase) {
		if s, err := r.stuck(ctx, t, run); err != nil || s {
			if err != nil {
				return err
			}
			if scoperRun(t) { // SC11: a stuck scoper fails open
				return r.scopeEnd(ctx, t, "failed", 0)
			}
			return r.end(ctx, t, v1alpha1.PhaseEscalated, "stuck")
		}
		return nil
	}
	reason := r.finished(ctx, t, run)
	if reason == "" {
		return nil
	}
	if reason == "pod_lost" {
		switch run.Reason {
		case runs.ReasonDisrupted, runs.ReasonPodLost, runs.ReasonPodFailed:
			reason = run.Reason // the broker's pod_lost reads the same for a drain, a deletion and a crash; the AgentRun read the pod
		}
	}
	current(t).Reason = reason
	r.interventions(ctx, t)
	if infraLost(run) {
		done, err := r.handedOff(ctx, t)
		if err != nil {
			return err
		}
		switch why := r.resumeBlock(t); {
		case done:
			run.Phase = "Succeeded" // it finished before its sandbox was lost: nothing to resume
		case why == "":
			r.resume(ctx, t, run)
			return nil
		default:
			reason = why
		}
	}
	if t.Spec.Loop != nil && current(t).Role == "implementer" {
		end := "other"
		switch {
		case envelope.LoopEnd(reason):
			end = reason
		case run.Phase == "Succeeded":
			end = "finished"
		}
		record(ctx, func(ctx context.Context) { r.Metrics.LoopRun(ctx, t.Spec.PredictedClass, end) })
	}
	if scoperRun(t) {
		return r.scoped(ctx, t, run.Phase, reason)
	}
	// R38: the triager's output reaches a public implementer only through a human. Its summary
	// stays in the room (internal); the issue gets the room link and the next step, nothing else.
	if current(t).Role == "triager" {
		if run.Phase != "Succeeded" {
			return r.end(ctx, t, v1alpha1.PhaseEscalated, reason)
		}
		evs, _, err := r.Rooms.EventsSince(ctx, t.Status.RoomRef, current(t).StartSeq, handoffTo("implementer"))
		if err != nil {
			return err
		}
		if len(evs) == 0 {
			return r.end(ctx, t, v1alpha1.PhaseNoOp, "no_action")
		}
		narrateLater(t, narrate.ProposalReady(t, r.Cfg.RoomsURL))
		return r.end(ctx, t, v1alpha1.PhaseDone, "proposal_ready")
	}
	switch {
	case run.Phase == "Succeeded" && t.Status.PullRequest != nil:
		return r.afterWriter(ctx, t)
	case run.Phase == "Succeeded":
		return r.end(ctx, t, v1alpha1.PhaseNoOp, "no_pr")
	default:
		return r.end(ctx, t, v1alpha1.PhaseEscalated, reason)
	}
}

// handoffTo selects the handoffs to role: the triager's proposal exists only if it made one (R38).
func handoffTo(role string) func(envelope.Event) bool {
	return func(e envelope.Event) bool {
		var p envelope.HandoffPayload
		return e.Type == envelope.Handoff && json.Unmarshal(e.Payload, &p) == nil && p.ToRole == role
	}
}

// lostReason is why a run of the task vanished: SP2 records a deleted claim as Revoked, reason
// deleted (its P15); say so when it has.
func (r *Reconciler) lostReason(ctx context.Context, t *v1alpha1.Task) string {
	if why, ok := r.roomReason(ctx, t); ok && why == "deleted" {
		return why
	}
	return "run_lost"
}

// infraLost: the AgentRun says its pod was disrupted or lost (disruption design §3). The room's
// pod_lost is no trigger: it reads the same for a harness that crashed, and a crashing agent must
// never be resumed in a loop.
// A revoked run is never resumed: someone stopped it.
func infraLost(run runs.Run) bool {
	return run.Phase == "Failed" && run.Revoked == "" && (run.Reason == runs.ReasonDisrupted || run.Reason == runs.ReasonPodLost)
}

// resumeBlock is why a run lost to its infrastructure is not resumed, "" when it is: no automatic
// resume is left (resume.maxPerTask), or the task's token cap cannot hold one more run (runFits). The
// cap is enforced here whatever budgets.enforceTask says: a resume is the factory's own decision,
// so it never spends past the cap, even while the cap is shadow elsewhere (disruption design §4).
// The usage it reads can trail the meter by one poll.meter, so a resume can overshoot the cap by
// what the lost run spent in its last poll, and no more.
func (r *Reconciler) resumeBlock(t *v1alpha1.Task) string {
	if int(t.Status.Resumes) >= r.Cfg.Resume.MaxPerTask {
		return "resumes_exhausted"
	}
	if !runFits(t) {
		return "resume_budget"
	}
	return ""
}

// resumable: the run was lost to its infrastructure and resumeBlock lets it resume.
func (r *Reconciler) resumable(t *v1alpha1.Task, run runs.Run) bool {
	return infraLost(run) && r.resumeBlock(t) == ""
}

// handedOff: the room holds a handoff of the task's current run, so its agent finished its work
// before the sandbox was lost: a resume would run finished work again.
func (r *Reconciler) handedOff(ctx context.Context, t *v1alpha1.Task) (bool, error) {
	cur := current(t)
	evs, _, err := r.roomTail(ctx, t.Status.RoomRef, cur.StartSeq, func(e envelope.Event) bool {
		return e.Type == envelope.Handoff && e.RunID == cur.ID
	})
	return len(evs) > 0, err
}

// resumeBrief is a resumed run's brief: ResumeNotice before the lost run's own, read back from its
// claim. A brief built now would miss what the lost one carried: the queued messages it quoted are
// consumed, and the room after its start holds no verdict it revises. ok is false for a run that
// resumes nothing, and once the lost run's claim is gone.
func (r *Reconciler) resumeBrief(ctx context.Context, t *v1alpha1.Task) (string, bool, error) {
	if nextTrigger(t) != "resume" || len(t.Status.Runs) == 0 {
		return "", false, nil
	}
	x, found, err := r.Runs.Get(ctx, current(t).ID)
	if err != nil || !found || x.TaskText == "" {
		return "", false, err
	}
	return ResumeNotice(t) + strings.TrimPrefix(x.TaskText, ResumeNotice(t)), true, nil
}

// resume sends the task back to Queued for a new run of the lost run's role, on the same branch
// and in the same room (disruption design §4). The new run is a new AgentRun with the task's next
// deterministic id (R48), so a replay after a lost status write never starts two; the lost one
// stays Failed. A reviewer's or tester's re-run spends no review round.
func (r *Reconciler) resume(ctx context.Context, t *v1alpha1.Task, run runs.Run) {
	t.Status.Resumes++
	n, why := int(t.Status.Resumes), run.Reason
	record(ctx, func(ctx context.Context) { r.Metrics.Resumed(ctx, why) })
	if run.Role == "reviewer" || run.Role == "tester" {
		r.requestVerifier(t, run.Role) // no ReviewRounds++: the loss was the platform's, not the review's
	} else {
		t.Status.NextTrigger = "resume"
		r.to(t, v1alpha1.PhaseQueued, "")
	}
	narrateLater(t, narrate.Resuming(t, run.ID, run.Role, n, r.Cfg.Resume.MaxPerTask))
}

// cause is why the current implementer run's work exists: a resumed run carries on the run it
// replaces, so it takes that run's trigger, and a revision a maintainer asked for still goes back
// to the maintainer.
func cause(t *v1alpha1.Task) string {
	for i := len(t.Status.Runs) - 1; i >= 0; i-- {
		if tr := t.Status.Runs[i].Trigger; tr != "resume" {
			return tr
		}
	}
	return "initial"
}

// afterWriter: a revision a maintainer asked for goes straight back to the maintainer; otherwise
// the template's first verifier after the implementer runs, or the task is ready (solo). A CI fix
// is verified like any revision: it moved the head, and the gate only arms on a verdict of the
// head it decides (R03), so skipping the review would hand it off as verdict_stale (N1).
func (r *Reconciler) afterWriter(ctx context.Context, t *v1alpha1.Task) error {
	if cause(t) == "human" {
		r.to(t, v1alpha1.PhaseAwaitingHuman, "")
		return nil
	}
	if cause(t) == "ci" { // a push after the fix's own is a human's: never reviewed into a merge
		pr, err := r.readPR(ctx, t)
		if err != nil {
			return err
		}
		if handed, err := r.humanCommit(ctx, t, pr.HeadSHA); err != nil || handed {
			return err
		}
	}
	if next := r.nextVerifier(t, "implementer"); next != "" {
		r.requestVerifier(t, next)
		return nil
	}
	return r.ready(ctx, t)
}

func (r *Reconciler) detectPR(ctx context.Context, t *v1alpha1.Task, run runs.Run) error {
	n, err := r.gh(ctx).PullRequestForBranch(ctx, "agent/"+t.Name)
	if err != nil || n == 0 {
		return err
	}
	pr, err := r.gh(ctx).PullRequest(ctx, n)
	if err != nil {
		return err
	}
	t.Status.PullRequest = &v1alpha1.PullRequestRef{Number: pr.Number, URL: pr.URL, NodeID: pr.NodeID, HeadSHA: pr.HeadSHA}
	notePR(t.Status.PullRequest, pr)
	if err := r.Runs.Annotate(ctx, run.ID, map[string]string{runs.AnnPullRequest: pr.URL}); err != nil {
		return err
	}
	if err := r.gh(ctx).AddLabels(ctx, pr.Number, "factory/class:"+t.Spec.PredictedClass); err != nil {
		return err
	}
	d, source, tier, tmpl := r.Now().Sub(t.CreationTimestamp.Time), t.Spec.Source.Kind, t.Spec.Budget.Tier, t.Spec.Template
	record(ctx, func(ctx context.Context) {
		r.Metrics.TimeToPR(ctx, d, source, tier, tmpl)
		r.Metrics.PROpened(ctx)
	})
	narrateLater(t, narrate.PROpened(t, pr.Number, pr.URL, run.ID))
	return nil
}

func (r *Reconciler) awaitingHuman(ctx context.Context, t *v1alpha1.Task) error {
	pr, err := r.readPR(ctx, t)
	if err != nil {
		return err
	}
	if r.prEnded(ctx, t, pr) {
		return nil
	}
	r.countApproves(ctx, t, pr)
	if rvs := r.changesRequested(t, pr); len(rvs) > 0 {
		return r.revise(ctx, t, pr, rvs)
	}
	// A return the factory cannot read is no return: it never blocks the reminder and stale close.
	if back, err := r.reenter(ctx, t, pr); err != nil {
		r.log().Warn("merge gate return unreadable", "task.id", t.Name, "reason", t.Status.Reason, "err", err)
	} else if back {
		return nil
	}
	return r.remind(ctx, t, pr)
}

// countApproves counts each maintainer's APPROVED review once (§7: how dark the factory is):
// a gate that still needed a human's approve is not dark. The review id rides status.handled,
// the same list that dedups request-changes reviews and commands.
func (r *Reconciler) countApproves(ctx context.Context, t *v1alpha1.Task, pr forge.PR) {
	for _, rv := range pr.Reviews {
		if rv.State == "APPROVED" && r.repo(t).IsMaintainer(rv.Author) && !slices.Contains(t.Status.Handled, rv.ID) {
			markHandled(t, rv.ID)
			record(ctx, func(ctx context.Context) { r.Metrics.Intervention(ctx, "approve") })
		}
	}
}

// prEnded ends the task when its pull request was merged or closed. A closed one carrying
// factory/stale was the stale close's (remind): its replay after a lost status write still says so.
func (r *Reconciler) prEnded(ctx context.Context, t *v1alpha1.Task, pr forge.PR) bool {
	class := t.Spec.PredictedClass
	switch pr.State {
	case "MERGED":
		t.Status.PullRequest.MergedBy, t.Status.PullRequest.MergeCommitSHA = pr.MergedBy, pr.MergeCommitSHA
		// R41: like the CI gate's bypass record, the timestamp is what puts the merge in the
		// class's breaker window — without it a demoted class' human merges never refill it.
		now := metav1.NewTime(r.Now())
		t.Status.PullRequest.MergedAt = &now
		record(ctx, func(ctx context.Context) { r.Metrics.PROutcome(ctx, class, "human_merged") })
		_ = r.end(ctx, t, v1alpha1.PhaseDone, "merged")
		return true
	case "CLOSED":
		reason := "pr_closed"
		if slices.Contains(pr.Labels, labelStale) {
			reason = "stale"
		}
		record(ctx, func(ctx context.Context) { r.Metrics.PROutcome(ctx, class, "closed") })
		_ = r.end(ctx, t, v1alpha1.PhaseClosed, reason)
		return true
	}
	return false
}

// stamp fills what the run ran on from its claim (R10): each field once, never changed, so a later
// read of a claim a newer CompositionRevision re-bound still records the one the run started on.
// A claim without a factory version predates the field: its run stays empty, as the spec says. A
// value over its CRD bound is not recorded, since the API server would refuse every status write.
func stamp(p *v1alpha1.RunProvenance, x runs.Run) {
	fill := func(dst *string, v string, bound int) {
		if len(v) <= bound {
			*dst = cmp.Or(*dst, v)
		}
	}
	fill(&p.FactoryVersion, x.FactoryVersion, maxFactoryVersionLen)
	if x.FactoryVersion == "" {
		return
	}
	fill(&p.CompositionRevision, x.CompositionRevision, maxCompositionRevisionLen)
	fill(&p.HarnessImage, x.HarnessImage, maxHarnessImageLen)
}

// served adds the models the meter saw serve the run (R10): a set that only grows, sorted, and
// bounded by the CRD's maxItems and maxLength. A longer name is skipped, never truncated: a
// truncated name would be a guess, and an unbounded label would wedge the task's status writes.
func served(p *v1alpha1.RunProvenance, models []string) {
	for _, m := range models {
		if m != "" && len(m) <= maxServedModelLen && !slices.Contains(p.ServedModels, m) && len(p.ServedModels) < maxServedModels {
			p.ServedModels = append(p.ServedModels, m)
		}
	}
	slices.Sort(p.ServedModels)
}

// The CRD's bounds on RunProvenance (Interfaces table of the run-provenance spec).
const (
	maxFactoryVersionLen      = 64
	maxCompositionRevisionLen = 253
	maxHarnessImageLen        = 320
	maxServedModelLen         = 64
)

// maxServedModels is RunProvenance.ServedModels' maxItems.
const maxServedModels = 8
