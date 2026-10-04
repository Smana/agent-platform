// SPDX-License-Identifier: Apache-2.0

// Package narrate is the factory talking where people already are (Δ6): one issue or PR comment
// per event, posted by the factory App and never twice (R22), and the task's task_state messages
// in its room, once per clientSeq (ruling SK). A narration carries the factory's own words and
// ids only, never text a user wrote.
package narrate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/rooms"
	"github.com/Smana/agent-platform/internal/factory/runs"
	"github.com/Smana/agent-platform/internal/factory/sanitize"
	"github.com/Smana/agent-platform/internal/github"
)

// MaxNarrated is status.narrated's maxItems in the Task CRD: Post trims the oldest keys past it.
const MaxNarrated = 512

// roomPrefix marks a room message's key in status.narrated: room/<clientSeq>/<key>.
const roomPrefix = "room/"

// Event is one narration: its idempotency key and its markdown body.
type Event struct {
	Key  string
	Body string
}

// commenter is the part of the forge narration uses.
type commenter interface {
	RecentComments(ctx context.Context, number int) ([]forge.Comment, error)
	Comment(ctx context.Context, number int, body string) error
}

// Narrator posts as the factory App. Login is config.factoryLogin: a marker counts only in that
// login's comments, since anyone can comment on a public issue and would otherwise silence a
// narration by posting its marker first.
type Narrator struct {
	Forge commenter
	Login string
}

// Marker is the hidden line that makes a comment findable again (R22).
func Marker(scope, key string) string {
	return fmt.Sprintf("<!-- agent-factory scope=%s event=%s -->", scope, key)
}

// PostOnce posts e on issue or PR number unless one of its recent comments (the forge's last 50)
// by the factory already carries e's marker.
func (n Narrator) PostOnce(ctx context.Context, number int, scope string, e Event) error {
	if n.Login == "" {
		return errors.New("narrate: the factory's login is required to trust a marker")
	}
	marker := Marker(scope, e.Key)
	cs, err := n.Forge.RecentComments(ctx, number)
	if err != nil {
		return fmt.Errorf("narrate: %w", err)
	}
	for _, c := range cs {
		if strings.EqualFold(c.Author, n.Login) && strings.Contains(c.Body, marker) {
			return nil
		}
	}
	if err := n.Forge.Comment(ctx, number, e.Body+"\n\n"+marker); err != nil {
		return fmt.Errorf("narrate: %w", err)
	}
	return nil
}

// Post is PostOnce plus the task's own record, which spares the list call on a replay. The key is
// recorded only once the comment exists or is found; number 0 means nowhere to narrate (R28).
func (n Narrator) Post(ctx context.Context, t *v1alpha1.Task, number int, e Event) error {
	if number == 0 || slices.Contains(t.Status.Narrated, e.Key) {
		return nil
	}
	if err := n.PostOnce(ctx, number, t.Name, e); err != nil {
		return err
	}
	record(t, e.Key)
	return nil
}

// record appends key and trims the oldest keys past MaxNarrated. The list is a display record:
// a trimmed key can only cost a replay its shortcut, never a seq (roomSeq is the ledger).
func record(t *v1alpha1.Task, key string) {
	t.Status.Narrated = append(t.Status.Narrated, key)
	if over := len(t.Status.Narrated) - MaxNarrated; over > 0 {
		t.Status.Narrated = slices.Delete(t.Status.Narrated, 0, over)
	}
}

// roomPoster is the broker's system API as narration uses it (rooms.Client).
type roomPoster interface {
	TaskState(ctx context.Context, room, text string, clientSeq int64) error
}

// Room posts text in the task's room as a task_state message, once per key. The broker keeps one
// message per clientSeq, so the seq is the ledger (ruling SK): a new key takes status.roomSeq + 1,
// and persist writes that to the Task's status before the post. After a restart the key is found
// with its seq, and the replay sends the same seq, which the broker answers without storing.
//
// A refusal keeps its type: errors.Is(err, rooms.ErrNoRoom) is a retry, and
// errors.Is(err, rooms.ErrNotPermitted) is expected until FR-1 enables the factory's
// systemPrincipals entry. Either way the seq stays taken, so the retry reuses it.
func Room(ctx context.Context, r roomPoster, t *v1alpha1.Task, key, text string, persist func(context.Context) error) error {
	switch {
	case t.Status.RoomRef == "":
		return fmt.Errorf("narrate: task %s has no room yet", t.Name)
	case persist == nil:
		return errors.New("narrate: a room message needs its seq persisted first")
	}
	seq, ok := roomSeq(t, key)
	if !ok {
		prevSeq, prevNarrated := t.Status.RoomSeq, slices.Clone(t.Status.Narrated)
		seq = t.Status.RoomSeq + 1
		t.Status.RoomSeq = seq
		record(t, roomPrefix+strconv.FormatInt(seq, 10)+"/"+key)
		if err := persist(ctx); err != nil {
			t.Status.RoomSeq, t.Status.Narrated = prevSeq, prevNarrated
			return fmt.Errorf("narrate: persist room seq %d: %w", seq, err)
		}
	}
	if err := r.TaskState(ctx, t.Status.RoomRef, text, seq); err != nil {
		return fmt.Errorf("narrate: task_state %d in room %s: %w", seq, t.Status.RoomRef, err)
	}
	return nil
}

// roomSeq finds the seq a room message key was given.
func roomSeq(t *v1alpha1.Task, key string) (int64, bool) {
	for _, k := range t.Status.Narrated {
		rest, ok := strings.CutPrefix(k, roomPrefix)
		if !ok {
			continue
		}
		n, k, ok := strings.Cut(rest, "/")
		if seq, err := strconv.ParseInt(n, 10, 64); ok && err == nil && k == key {
			return seq, true
		}
	}
	return 0, false
}

// Tokens is a token count as people read it: 300 k, 1.5 M.
func Tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1f M", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%d k", n/1000)
	}
	return strconv.FormatInt(n, 10)
}

// reasons reads the end, stop and refusal reasons of the factory, SP1 and SP2 (P15) as prose.
func reasons() map[string]string {
	return map[string]string{
		"agent_finished":          "the agent finished its work",
		"agent_error":             "the agent stopped on an error",
		"agent_stuck":             "the agent reported that it was stuck",
		"stuck":                   "the run showed no activity for 10 minutes",
		"deadline":                "the run hit its wall-clock limit",
		"pod_lost":                "the sandbox was lost (spot reclaim or eviction)",
		"revoked":                 "the run was stopped by hand",
		"deleted":                 "the run's claim was deleted",
		"budget-run":              "the run spent its token budget",
		"budget-principal":        "the factory's daily token budget is spent",
		"budget-fleet":            "the agent fleet's daily token budget is spent",
		"budget-task":             "the task spent its token budget",
		"run_lost":                "the run disappeared",
		"no_pr":                   "the agent opened no pull request",
		"no_action":               "the triager found nothing to change",
		"proposal_ready":          "the triager proposed a change for a maintainer to publish",
		"merged":                  "the pull request was merged",
		"pr_closed":               "the pull request was closed",
		"text_too_long":           "the issue text is longer than the factory accepts (14 KiB)",
		"daily_task_cap":          "the factory has reached its daily task cap",
		"kill_switch":             "the factory's kill switch is engaged",
		"stopped_by_label":        "a maintainer applied factory/stop",
		"stopped_by_annotation":   "an operator stopped it",
		"superseded":              "a new factory/ready replaced it",
		"unauthorised_labeller":   "only a maintainer's factory/ready starts a task",
		"edited_after_label":      "the issue was edited after it was labelled",
		"task_active":             "a task for this issue is still running",
		"unsanitisable":           "the issue text could not be made safe for an agent to read; simplify its markup",
		"unauthorised_stopper":    "only a maintainer's factory/stop stops a task",
		"foreign_room":            "a room of the task's name exists and is not the factory's",
		"stale":                   "the pull request had no maintainer activity (a review, a comment or a push) for 14 days",
		"review_rounds_exhausted": "the reviewer still asked for changes after the last review round",
		"no_verdict":              "the last review ended without a verdict the factory can act on, with no review round left",
		"verdict_missing":         "it recorded no verdict",
		"verdict_stale":           "its approval was not for the pull request's current head",
		"room_log_too_long":       "the room's log was too long to read to its end",
		"room_log_unreadable":     "the room's log, which holds the review verdict, could not be read",
		"run_unschedulable":       "the cluster never admitted the task's run within its bound",
		"ci_red":                  "CI stayed red after the fix runs",
		"secret_scan_red":         "the secret scan (Security scanning 🔒, TruffleHog) found a live credential in this pull request: a maintainer revokes it and closes the pull request, and the factory does not retry",
		"ci_pending":              "CI has not finished",
		"main_red":                "main's CI went red after the merge",
		"revert_requested":        "a maintainer asked for a revert",
		"merged_verified":         "merged, and main stayed green for 30 minutes",
		"gate_path":               "it touches a gate path, so an agent can never merge it; a human must re-author the change",
		"class_mismatch":          "the diff is not the class the triage predicted",
		"foreign_trailer":         "its head commit comes from another task's run",
		"class_paused":            "auto-merge of this class is paused after a revert",
		"head_unreported":         "the room never reported its current head as a commit one of the task's runs pushed",
		"auto_merge_cap":          "today's auto-merge cap is reached",
		"policy_pending":          "the policy needs a maintainer's approval",
		"policy_absent":           "policy-bot has not evaluated it",
		"maintainer_approved":     "a maintainer approved it: a human merges it",
		"no_approving_verdict":    "the reviewer did not approve it",
		"not_agent_authored":      "it is not the agents' pull request",
		"not_mergeable":           "GitHub does not allow merging it as it stands",
		"head_moved":              "a new commit landed since the decision, so the merge was refused and it is decided again",
		"shadow_would_arm":        "it would auto-merge, and the merge gate is in shadow until the merge wave",
	}
}

// Reason is r as prose; a reason without one is shown as it is.
func Reason(r string) string {
	if s, ok := reasons()[r]; ok {
		return s
	}
	return r
}

// Started announces a run with what a human needs: its id, branch, budget, where to watch it
// and how to stop it. roomsURL is config.roomsURL, which the config holds to https://<host>.
func Started(t *v1alpha1.Task, s runs.Spec, roomsURL string) Event {
	body := fmt.Sprintf("Agent factory task `%s` started run `%s` (%s) on branch `%s`.\n\n"+
		"- Budget: %s tokens, %d minutes (tier %s)\n"+
		"- Watch: %s/r/%s (tailnet only)\n"+
		"- Stop: apply the label `factory/stop`",
		t.Name, s.RunID, s.Role, s.Branch, Tokens(s.MaxTokens), s.MaxMinutes, t.Spec.Budget.Tier,
		strings.TrimSuffix(roomsURL, "/"), t.Status.RoomRef)
	return Event{Key: "run-" + s.RunID + "-started", Body: body}
}

// ProposalReady narrates a finished triage on the task's issue: the proposal stays in the room
// (internal) until a human publishes it (R38).
func ProposalReady(t *v1alpha1.Task, roomsURL string) Event {
	return Event{Key: "proposal", Body: fmt.Sprintf("Agent factory task `%s`: the triager proposes a change. Read the "+
		"proposal in the room, %s/r/%s (tailnet only). If it is safe to publish, open a new issue with the text you "+
		"approve and label it `factory/ready`; nothing else starts from this finding.",
		t.Name, strings.TrimSuffix(roomsURL, "/"), t.Status.RoomRef)}
}

// Revising says a maintainer's review sent the task back for another run (Δ5), once per round.
func Revising(t *v1alpha1.Task, reviewer string) Event {
	return Event{Key: fmt.Sprintf("revise-%d", len(t.Status.Runs)),
		Body: fmt.Sprintf("Agent factory task `%s` is revising after @%s's review: the next run starts on the same branch, "+
			"with the review in its brief.", t.Name, reviewer)}
}

// remindPrefix starts a reminder's key: remind-<runs>-<quiet spell start, unix seconds>.
const remindPrefix = "remind-"

// Reminder mentions the maintainers when the task's pull request has had no maintainer activity
// for 48 h (§6.3), once per quiet spell: quietSince is the spell's start, so a spell a maintainer
// broke and that began again earns its own reminder before any stale close. An approved pull
// request waits for a merge and is never closed, so its reminder says so.
func Reminder(t *v1alpha1.Task, maintainers []string, quietSince time.Time, approved bool) Event {
	at := make([]string, 0, len(maintainers))
	for _, m := range maintainers {
		at = append(at, "@"+m)
	}
	body := fmt.Sprintf("%s: the pull request of agent factory task `%s` has had no maintainer activity for 48 hours. "+
		"It closes itself after 14 days without any: a review, a comment here or on the pull request, or a push to its branch keeps it open.",
		strings.Join(at, " "), t.Name)
	if approved {
		body = fmt.Sprintf("%s: the pull request of agent factory task `%s` is approved and has waited 48 hours for a merge. "+
			"The factory never closes an approved pull request.", strings.Join(at, " "), t.Name)
	}
	return Event{Key: fmt.Sprintf("%s%d-%d", remindPrefix, len(t.Status.Runs), quietSince.Unix()), Body: body}
}

// RemindedSince is the latest quiet-spell start a reminder of the task's current stay was keyed
// on, posted or still in the outbox. The reconciler never lets a spell start before it: a
// maintainer comment that later leaves GitHub's read window must not restart an older spell.
func RemindedSince(t *v1alpha1.Task) (time.Time, bool) {
	prefix := fmt.Sprintf("%s%d-", remindPrefix, len(t.Status.Runs))
	keys := slices.Clone(t.Status.Narrated)
	for _, o := range t.Status.Outbox {
		keys = append(keys, o.Key)
	}
	var latest int64
	found := false
	for _, k := range keys {
		rest, ok := strings.CutPrefix(k, prefix)
		if !ok {
			continue
		}
		if u, err := strconv.ParseInt(rest, 10, 64); err == nil && (!found || u > latest) {
			latest, found = u, true
		}
	}
	return time.Unix(latest, 0), found
}

// Retrying says a maintainer's /factory retry sent an escalated task back for a fresh run, once
// per retry. after is the reason it had escalated: a retry never gives review rounds back (T5),
// so after the rounds ran out it says what the retry buys.
func Retrying(t *v1alpha1.Task, by, after string) Event {
	body := fmt.Sprintf("Agent factory task `%s` is retrying, as @%s asked.", t.Name, by)
	if after == "review_rounds_exhausted" || after == "no_verdict" {
		body += " Its review rounds stay spent: the retry buys one more revision and one more review, and a further " +
			"`changes` verdict, or a review without a verdict, escalates it again."
	}
	return Event{Key: fmt.Sprintf("retry-%d", t.Status.Retries), Body: body}
}

// PROpened announces the task's pull request.
func PROpened(t *v1alpha1.Task, number int, url, runID string) Event {
	return Event{Key: "pr-opened", Body: fmt.Sprintf("Run `%s` of task `%s` opened #%d: %s", runID, t.Name, number, url)}
}

// Armed announces the merge the gate issued on the decision (§5.1). R52 merged it at the decided
// head, where GitHub checks that head at merge time; the key keeps the word "armed" — the status
// records the same arming.
func Armed(t *v1alpha1.Task) Event {
	return Event{Key: "armed-" + fmt.Sprint(len(t.Status.Runs)), Body: fmt.Sprintf("Agent factory task `%s`: CI is green and policy-bot "+
		"matched the live class `%s`, so it is merged now, at the decided head: GitHub re-checks that head at merge time and refuses "+
		"a moved one. main's CI is watched after the merge.", t.Name, t.Spec.PredictedClass)}
}

// WouldArm is the shadow gate's record (R32): the same decision as Armed, and nothing armed.
func WouldArm(t *v1alpha1.Task, class string) Event {
	return Event{Key: "would-arm-" + fmt.Sprint(len(t.Status.Runs)), Body: fmt.Sprintf("Agent factory task `%s`: "+
		"would auto-merge: `%s`, checks green, verdict approve. The merge gate is in shadow until the "+
		"merge wave, so nothing is armed: a maintainer merges or closes this pull request.", t.Name, class)}
}

// WaitingForHuman says the gate stopped deciding and a maintainer's review is what remains.
func WaitingForHuman(t *v1alpha1.Task, reason string) Event {
	return Event{Key: "human-" + reason + "-" + fmt.Sprint(len(t.Status.Runs)),
		Body: fmt.Sprintf("Agent factory task `%s` waits for a maintainer's review: %s.", t.Name, Reason(reason))}
}

// CIExhausted says the CI fix runs are spent and the named checks are still failing (§6.3).
func CIExhausted(t *v1alpha1.Task, names []string) Event {
	return Event{Key: fmt.Sprintf("ci-exhausted-%d", t.Status.FixRuns),
		Body: fmt.Sprintf("Agent factory task `%s` used its %d CI fix runs; still failing: %s.", t.Name, t.Status.FixRuns, strings.Join(names, ", "))}
}

// RevertOpened announces the merger-authored revert of an auto-merged pull request (§6.4).
func RevertOpened(t *v1alpha1.Task, number int, why string) Event {
	return Event{Key: "revert", Body: fmt.Sprintf("Agent factory task `%s` opened #%d to revert this pull request: %s. "+
		"Auto-merge of the class `%s` goes back to human review until its recent merges pass without a revert (R41).",
		t.Name, number, Reason(why), t.Spec.PredictedClass)}
}

// ClassDemoted goes to the control issue (Task 8.1a): a revert counts toward its class's breaker (R41).
func ClassDemoted(t *v1alpha1.Task, window, maxReverts int) Event {
	return Event{Key: "demoted-" + t.Name, Body: fmt.Sprintf("Agent factory: an auto-merge of `%s` was reverted "+
		"(task `%s`, #%d). It counts toward the class's breaker: `%s` goes to human review while %d or more of its "+
		"last %d merges are reverts, and maintainers' merges of the class count toward that window (R41).",
		t.Spec.PredictedClass, t.Name, t.Status.PullRequest.Number, t.Spec.PredictedClass, maxReverts, window)}
}

// RevertStalled announces the disarm of a revert that did not go green. Its key starts with
// revert-end-, which ends the reconciler's revert watch.
func RevertStalled(t *v1alpha1.Task, number int) Event {
	return Event{Key: "revert-end-stalled", Body: fmt.Sprintf("The revert #%d of agent factory task `%s` did not go green "+
		"in time, so its auto-merge is off: a maintainer merges or closes it.", number, t.Name)}
}

func headlines() map[string]string {
	return map[string]string{
		v1alpha1.PhaseDone: "is done", v1alpha1.PhaseNoOp: "made no change", v1alpha1.PhaseEscalated: "needs a maintainer",
		v1alpha1.PhaseRejected: "was not accepted", v1alpha1.PhaseClosed: "was closed", v1alpha1.PhaseStopped: "was stopped",
		v1alpha1.PhaseReverted: "was reverted",
	}
}

func hints() map[string]string {
	return map[string]string{
		v1alpha1.PhaseEscalated: "Comment `/factory retry` on this issue to run it again (a maintainer only), or push to the branch yourself.",
		v1alpha1.PhaseNoOp:      "If there is work to do, add detail to the issue and re-apply `factory/ready`.",
		v1alpha1.PhaseRejected:  "Fix what is described above and re-apply `factory/ready`.",
	}
}

// Ended announces the task's end, why, and what a human can do next. Its key carries the phase
// and the run count, so a task that ends, is retried and ends again narrates both. An Escalated
// ending mentions the maintainers given: §6.3 puts a named human on every escalation.
func Ended(t *v1alpha1.Task, phase, reason string, maintainers ...string) Event {
	var b strings.Builder
	fmt.Fprintf(&b, "Agent factory task `%s` %s", t.Name, headlines()[phase])
	if why := Reason(reason); why != "" {
		fmt.Fprintf(&b, ": %s", why)
	}
	b.WriteString(".")
	if pr := t.Status.PullRequest; pr != nil && pr.MergedBy != "" && phase == v1alpha1.PhaseDone {
		fmt.Fprintf(&b, " Merged by @%s.", strings.TrimSuffix(pr.MergedBy, "[bot]"))
	}
	if phase == v1alpha1.PhaseEscalated && len(maintainers) > 0 {
		b.WriteString("\n\n@" + strings.Join(maintainers, " @") + ": this task needs a maintainer.")
	}
	if h := hints()[phase]; h != "" {
		b.WriteString("\n\n" + h)
	}
	fmt.Fprintf(&b, "\n\nTokens used: %s.", Tokens(t.Status.Usage.Tokens))
	return Event{Key: fmt.Sprintf("end-%s-%d", strings.ToLower(phase), len(t.Status.Runs)), Body: b.String()}
}

// StopIgnored answers a factory/stop the poller will not act on, once per label.
func StopIgnored(number int, reason string, at time.Time) Event {
	return Event{Key: fmt.Sprintf("stop-ignored-%s-%d", reason, at.Unix()),
		Body: fmt.Sprintf("The agent factory ignored `factory/stop` on #%d: %s.", number, Reason(reason))}
}

// Refused answers a factory/ready the poller will not act on; keyed by the label's time, so each
// label is answered once.
func Refused(number int, reason string, at time.Time) Event {
	return Event{Key: fmt.Sprintf("refused-%s-%d", reason, at.Unix()),
		Body: fmt.Sprintf("The agent factory did not start a task for #%d: %s.", number, Reason(reason))}
}

// NoVerdict says a reviewer or tester run ended without a verdict the factory can act on, and that
// a new review run starts (why is one of verdict_missing, verdict_stale, room_log_too_long). Once
// per run.
func NoVerdict(t *v1alpha1.Task, runID, why string) Event {
	return Event{Key: "noverdict-" + runID, Body: fmt.Sprintf("The review run `%s` of agent factory task `%s` ended without a "+
		"verdict the factory can act on: %s. A new review run starts; each one uses a review round.", runID, t.Name, Reason(why))}
}

// maxBody is the Task CRD's bound on an outbox narration's body.
const maxBody = 4096

// RoundsExhausted goes on the pull request itself (§6.3: "PR comment carrying the last verdict"),
// once per escalation. The summary is an agent's text: sanitised (invisible characters, NFKC,
// fence look-alikes, images and raw HTML, so no marker of ours or the broker's survives), its @
// made fullwidth, and quoted as a code block it cannot close, where GitHub renders no mention,
// link or HTML (ruling TE). Only a public task's summary leaves the room (C7). v's verdict, commit
// and run id are rooms.LastVerdict's, already checked.
func RoundsExhausted(t *v1alpha1.Task, v rooms.Verdict) Event {
	head := fmt.Sprintf("Agent factory task `%s` used its %d review rounds. The last verdict of run `%s` was `%s` on `%s`:\n\n",
		t.Name, t.Status.ReviewRounds, v.RunID, v.Verdict, v.Commit)
	body := "_The summary stays in the room: the task's data class is not public._"
	if t.Spec.DataClass == "public" {
		body = summary(v.Text, maxBody-len(head))
	}
	return Event{Key: fmt.Sprintf("rounds-%d-%d", t.Status.ReviewRounds, len(t.Status.Runs)), Body: head + body}
}

// summary is an agent's text as an inert code block of at most budget bytes, clipped with a mark.
func summary(text string, budget int) string {
	s, _ := sanitize.Text(text)
	s = strings.ReplaceAll(s, "@", "＠")
	const mark = "\n⟦clipped by the factory; the whole summary is in the room⟧"
	for n := len(s); ; n = n * 3 / 4 {
		shown := s
		if n < len(s) {
			shown = strings.ToValidUTF8(s[:n], "") + mark
		}
		if q := github.Quote(shown); len(q) <= budget || n == 0 {
			return q
		}
	}
}
