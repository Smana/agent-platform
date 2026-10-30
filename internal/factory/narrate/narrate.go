// SPDX-License-Identifier: Apache-2.0

// Package narrate is the factory talking where people already are (Δ6): one issue or PR comment
// per event, posted by the factory App and never twice (R22), and the task's task_state messages
// in its room, once per clientSeq (ruling SK). A narration carries the factory's own words and
// ids only, never text a user wrote.
package narrate

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/finding"
	"github.com/Smana/maku/internal/factory/forge"
	"github.com/Smana/maku/internal/factory/rooms"
	"github.com/Smana/maku/internal/factory/runs"
	"github.com/Smana/maku/internal/factory/sanitize"
	"github.com/Smana/maku/internal/factory/taskid"
	"github.com/Smana/maku/internal/github"
	summ "github.com/Smana/maku/internal/summary"
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
		"agent_finished":            "the agent finished its work",
		"agent_error":               "the agent stopped on an error",
		"agent_stuck":               "the agent reported that it was stuck",
		"judge_impossible":          "the completion judge found the task cannot be done as asked",
		"loop_unsupported":          "the cluster does not support the loop policy or repository checks yet (the run claim was refused), so no run started",
		"stalled":                   "the run made no commit within its step limit, a judge continuation added none within its time box, or two continuations in a row added none",
		"not_met":                   "the completion judge still found the acceptance checks unmet after the last continuation, or a continuation did not finish within its time box",
		"budget_calls":              "the scoper reached its tool-call limit before handing off",
		"needs_author":              "the scoper needs an answer from the issue's author before any code is written",
		"contract_invalid":          "the tester's branch was not the tester's alone, or its contract commit was malformed",
		"contract_disputed":         "the implementer says the tester's tests are wrong; a maintainer decides",
		"contract_too_large":        "the tester's contract names more files or test ids than a task records, or one of them is too long",
		"contract_check_unset":      "the task has a test contract but the repository no longer names merge.contractCheck",
		"contract_modified":         "the pull request changes a test the tester wrote as the task's contract",
		"contract_unread":           "the factory has not yet read which files changed since the test contract",
		"contract_push_unrecorded":  "GitHub has not yet recorded the tester's push",
		"split_proposed":            "the scoper proposes to split the task into parts, and a maintainer decides",
		"budget-family":             "the cap of the task and its parts has no room left for another part's run",
		"children_merged":           "every part of the split merged",
		"child_dropped":             "a part of the split was closed without merging, or its merge was reverted",
		"parent_not_split":          "its parent task is no longer waiting on its parts",
		"parent_missing":            "its parent task no longer exists",
		"escalation_expired":        "it waited past the escalation time limit for a maintainer",
		"stuck":                     "the run showed no activity for 10 minutes",
		"deadline":                  "the run hit its wall-clock limit",
		"pod_lost":                  "the sandbox was lost (spot reclaim or eviction)",
		"Disrupted":                 "the sandbox's node was reclaimed or drained",
		"PodLost":                   "the sandbox was lost (deleted, or its node disappeared)",
		"PodFailed":                 "the sandbox failed on its own",
		"resumes_exhausted":         "the sandbox was lost again and the automatic resumes are used up",
		"resume_budget":             "the sandbox was lost and the task's token budget cannot cover another run",
		"revoked":                   "the run was stopped by hand",
		"deleted":                   "the run's claim was deleted",
		"budget-run":                "the run spent its token budget",
		"budget-principal":          "the factory's daily budget in dollars is spent",
		"budget-person":             "the daily budget in dollars of the person its next run is charged to is spent",
		"principal_unknown":         "the API principal that requested it is no longer in the factory's configuration",
		"rooms_crd_outdated":        "the cluster's Rooms CRD is older than the factory and refuses a system: member; an operator must refresh the CRD",
		"budget-fleet":              "the agent fleet's daily token budget is spent",
		"budget-task":               "the task's token budget has no room left for another full run",
		"run_lost":                  "the run disappeared",
		"no_pr":                     "the agent opened no pull request",
		"no_action":                 "the triager found nothing to change",
		"no_proposal":               "the triager's run made no proposal to publish",
		"task_not_ended":            "the task is still running, and its proposal is final only once it ends",
		"invalid_draft":             "the proposal does not follow the issue template (a title `<area>: <what is wrong>`, then the sections What is wrong, What it should be and Acceptance check)",
		"redacted":                  "the proposal holds something that looks like a secret, so it stays in the room",
		"proposal_ready":            "the triager proposed a change for a maintainer to publish (factory/publish)",
		"merged":                    "the pull request was merged",
		"pr_closed":                 "the pull request was closed",
		"text_too_long":             "the issue text is longer than the factory accepts (14 KiB)",
		"daily_task_cap":            "the factory has reached its daily task cap",
		"kill_switch":               "the factory's kill switch is engaged",
		"stopped_by_label":          "a maintainer applied factory/stop",
		"stopped_by_annotation":     "an operator stopped it",
		"superseded":                "a new factory/ready replaced it",
		"family_dropped":            "the family was dropped because another part was dropped or reverted",
		"unauthorised_labeller":     "only a maintainer's factory/ready starts a task",
		"edited_after_label":        "the issue was edited after it was labelled",
		"task_active":               "a task for this issue is still running",
		"unsanitisable":             "the issue text could not be made safe for an agent to read; simplify its markup",
		"unauthorised_stopper":      "only a maintainer's factory/stop stops a task",
		"foreign_room":              "a room of the task's name exists and is not the factory's",
		"stale":                     "the pull request had no maintainer activity (a review, a comment or a push) for 14 days",
		"review_rounds_exhausted":   "the reviewer still asked for changes after the last review round",
		"no_verdict":                "the last review ended without a verdict the factory can act on, with no review round left",
		"verdict_missing":           "it recorded no verdict",
		"merge_gate_off":            "no merge class is live or in shadow, so the factory does not gate this pull request",
		"verdict_stale":             "its approval was not for the pull request's current head",
		"room_log_too_long":         "the room's log was too long to read to its end",
		"room_log_unreadable":       "the room's log, which holds the review verdict, could not be read",
		"run_unschedulable":         "the cluster never admitted the task's run within its bound",
		"ci_red":                    "CI stayed red after the fix runs",
		"secret_scan_red":           "the secret scan (Security scanning 🔒, TruffleHog) found a live credential in this pull request: a maintainer revokes it and closes the pull request, and the factory does not retry",
		"ci_cancelled":              "a required check was cancelled and re-running it did not settle it",
		"changes_requested":         "a reviewer requested changes, and the factory does not merge over them",
		"fork_pull_request":         "it comes from a fork, which the factory never adopts",
		"push_unrecorded":           "GitHub has not recorded the push of its head, so who made it cannot be shown",
		"adoption_unread":           "who authored its commits and pushed its branch could not be read",
		"reentered_new_head":        "its author pushed a new head, which is decided from the start",
		"adopted_head_moved":        "its author pushed a new head after the shadow decision, so it is decided again",
		"adopt_ref_invalid":         "the adopted pull request's reference could not be read, so it was not adopted",
		"not_adopted_author":        "it is not authored by a login this class adopts",
		"branch_not_allowed":        "its branch is not one this class adopts",
		"release_age_pending":       "the release age check has not passed yet",
		"release_age_red":           "the release age check failed, or was not posted by the login this class adopts",
		"ci_pending":                "CI has not finished",
		"main_red":                  "main's CI went red after the merge",
		"revert_requested":          "a maintainer asked for a revert",
		"merged_verified":           "merged, and main stayed green for 30 minutes",
		"gate_path":                 "it touches a gate path, so an agent can never merge it; a human must re-author the change",
		"steering_path":             "it changes a steering note, which only a maintainer on the default branch may do",
		"class_mismatch":            "the diff is not the class the triage predicted",
		"class_unarmed":             "the factory does not merge this class: a maintainer reviews and merges it",
		"file_not_allowed":          "it changes a file outside its class's allowed files",
		"file_protected":            "it changes a file its class protects",
		"file_added_or_deleted":     "it changes a file in a way other than editing its content (an add, delete, rename, copy or mode change), which its class does not allow",
		"patch_too_large":           "it changes more files or lines than its class allows",
		"holdout_red":               "a holdout check its class requires did not succeed (failed, cancelled, neutral, skipped, or not posted by GitHub Actions), or its checks could not all be read",
		"oracle_finding_open":       "a link its task was opened to fix is still a link target in the head, or the file holding it was not changed",
		"oracle_unrequested":        "it moves a link target the task's findings did not ask to move, or moves one anywhere but the exact snapshot its finding gave",
		"holdout_pending":           "a holdout check its class requires has not passed yet",
		"ci_wait_expired":           "a check it waits on never reported within merge.awaitingCIFor",
		"foreign_trailer":           "its head commit comes from another task's run",
		"class_paused":              "auto-merge of this class is paused after a revert, a follow-up fix or a regression in its recent merges",
		"ledger_truncated":          "the merge ledger read is incomplete (a full page of 100 labelled pull requests), so the factory does not arm",
		"head_unreported":           "the room never reported its current head as a commit one of the task's runs pushed",
		"auto_merge_cap":            "today's auto-merge cap is reached",
		"not_owned":                 "this repository's .github/agent-factory.yaml does not name this factory",
		"policy_pending":            "the policy needs a maintainer's approval",
		"policy_absent":             "policy-bot has not evaluated it",
		"maintainer_approved":       "a maintainer approved it",
		"no_approving_verdict":      "the reviewer did not approve it",
		"not_agent_authored":        "it is not the agents' pull request",
		"not_mergeable":             "GitHub does not allow merging it as it stands",
		"head_moved":                "a new commit landed since the decision, so the merge was refused and it is decided again",
		"shadow_would_arm":          "it would auto-merge, and the merge gate is in shadow until the merge wave",
		"human_commit_in_pr":        "it holds a commit no run of the task reported, so the factory neither fixes nor merges on top of it",
		"closes_parent":             "its description closes the parent issue (Fixes, Closes or Resolves), which closes only with the last part: edit the description, then merge it yourself",
		"approval_stale":            "a maintainer's approval is of an older head: approve the current one",
		"reentered_approved":        "a maintainer approved the head it was handed off at, so the gate decides it again",
		"reentered_day_turned":      "the UTC day turned, so the cap is counted again",
		"reentered_unpaused":        "the class's pause lifted, so the gate decides it again",
		"reentered_policy":          "policy-bot now passes the head it was handed off at",
		"reentered_head_reported":   "the room now reports its head as a commit of the task's run",
		"rereview_verdict_stale":    "the factory moved its head after the approval, so it is reviewed again",
		"waiting_human_driver":      "a person drives its room; the hold ends when they hand the driver back or their lease lapses",
		"waiting_room_busy":         "another run in its room is still going; the hold ends when that run ends",
		"waiting_run_slot":          "the factory runs its tasks one at a time and another one has the slot",
		"waiting_active_tasks":      "the factory's cap on active tasks is reached",
		"waiting_review_wip":        "the reviewers are at their limit of pull requests in review",
		"waiting_daily_budget":      "the factory's daily budget in dollars is spent",
		"waiting_room_log":          "the room's log is not open on the broker yet",
		"waiting_broker_permission": "the broker does not let the factory write to rooms yet",
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
// (internal) until a maintainer publishes it (R38). factory/publish has the factory file the
// sanitised proposal as a new issue; starting that one is a second label (RunLore RA9).
func ProposalReady(t *v1alpha1.Task, roomsURL string) Event {
	return Event{Key: "proposal", Body: fmt.Sprintf("Agent factory task `%s`: the triager proposes a change. Read the "+
		"proposal in the room, %s/r/%s (tailnet only). If it is safe to publish, label this issue `factory/publish`: "+
		"the factory files the proposal as a new issue, which you read before you label it `factory/ready`. "+
		"Nothing else starts from this finding.",
		t.Name, strings.TrimSuffix(roomsURL, "/"), t.Status.RoomRef)}
}

// Published answers a factory/publish that filed the proposal, or one whose proposal was already
// filed: the issue's number, and the next step.
func Published(_, issue int, by string) Event {
	return Event{Key: fmt.Sprintf("published-%d", issue),
		Body: fmt.Sprintf("Published as #%d by @%s. Read it, then label it `factory/ready` to start the fix.", issue, by)}
}

// publishReasons are the prose of a factory/publish refusal that differs from Reason's, which
// speaks of factory/ready.
var publishReasons = map[string]string{
	"unauthorised_labeller": "only a maintainer's factory/publish files a proposal",
}

// PublishRefused answers a factory/publish the poller will not act on, keyed by the label's time,
// so each label is answered once. Nothing was created.
func PublishRefused(n int, reason string, at time.Time) Event {
	why, ok := publishReasons[reason]
	if !ok {
		why = Reason(reason)
	}
	return Event{Key: fmt.Sprintf("publish-refused-%s-%d", reason, at.Unix()),
		Body: fmt.Sprintf("The agent factory did not publish #%d's proposal: %s.", n, why)}
}

// PublishRejected answers a factory/publish on an issue whose task was rejected at admission: no run
// ran, so there is no proposal, and the rejection's reason is what the maintainer needs.
func PublishRejected(n int, task, reason string, at time.Time) Event {
	return Event{Key: fmt.Sprintf("publish-refused-rejected-%s-%d", reason, at.Unix()),
		Body: fmt.Sprintf("The agent factory did not publish #%d's proposal: its task `%s` was not accepted, so no run made one: %s.",
			n, task, Reason(reason))}
}

// NeedsAuthor is the bounce (scoper design SC8, "The bounce, on the issue"): one comment naming the
// issue's author and the maintainers. A public task's question and first assumption are an agent's
// text, quoted as an inert code block (ruling TE); an internal task's stay in the room (C7).
func NeedsAuthor(t *v1alpha1.Task, question, assumption string, maintainers []string, roomsURL string) Event {
	var b strings.Builder
	who := "the issue's author"
	if a := t.Spec.IssueAuthor; a != "" {
		who = "@" + a
	}
	fmt.Fprintf(&b, "Agent factory task `%s` needs an answer from %s before it writes any code.", t.Name, who)
	if t.Spec.DataClass == "public" {
		b.WriteString(" The scoper's question, and the assumption it would make otherwise:\n\n")
		b.WriteString(summary(question+"\nAssumption: "+assumption, maxBody/2))
	} else {
		fmt.Fprintf(&b, " The scoper's question stays in [the room](%s/r/%s): the task's data class is not public.", strings.TrimSuffix(roomsURL, "/"), t.Status.RoomRef)
	}
	b.WriteString("\n\n**To answer**, edit this issue's title or body: the factory reads the issue as it was labelled, " +
		"never its comments. Then a maintainer re-applies `factory/ready`. **To proceed on the assumption**, a maintainer " +
		"comments `/factory retry`. Without either, the task stops when its escalation expires.")
	if len(maintainers) > 0 {
		b.WriteString("\n\n@" + strings.Join(maintainers, " @") + ": this task waits on its author.")
	}
	return Event{Key: fmt.Sprintf("needs-author-%d", len(t.Status.Runs)), Body: b.String()}
}

// SplitProposed is the split's one comment on the parent issue (splitter design SL4): the plan, and
// how to approve it, run the task whole, or scope it again. A public task's why and each part's task
// are an agent's text, quoted as inert code blocks within the outbox's bound (ruling TE); an
// internal task's stay in the room (C7).
func SplitProposed(t *v1alpha1.Task, why string, parts, maintainers []string, roomsURL string) Event {
	room := fmt.Sprintf("%s/r/%s", strings.TrimSuffix(roomsURL, "/"), t.Status.RoomRef)
	head := fmt.Sprintf("Agent factory task `%s` proposes to split this issue into %d parts, each its own sub-issue and "+
		"pull request, opened one at a time: a part starts once the one before it has merged.", t.Name, len(parts))
	var tail strings.Builder
	s := t.Spec.Split
	fmt.Fprintf(&tail, "\n\n**To approve**, a maintainer comments `/factory split`: the approver is charged for every part, "+
		"each part runs at tier %s, and the parts together stay within %s. **To run it whole**, a maintainer comments "+
		"`/factory retry`. **To scope it again**, edit this issue and re-apply `factory/ready`. Without any of these, the task "+
		"stops when its escalation expires. The whole plan is in [the room](%s).", s.ChildTier, USD(s.FamilyMicroUSD), room)
	if len(maintainers) > 0 {
		tail.WriteString("\n\n@" + strings.Join(maintainers, " @") + ": this split waits on a maintainer.")
	}
	var b strings.Builder
	b.WriteString(head)
	if t.Spec.DataClass != "public" {
		b.WriteString(" Its parts stay in the room: the task's data class is not public.")
	} else {
		each := (maxBody - len(head) - tail.Len() - 32*(len(parts)+1)) / (len(parts) + 1)
		b.WriteString("\n\nWhy:\n\n" + summary(why, each))
		for i, p := range parts {
			fmt.Fprintf(&b, "\n\nPart %d:\n\n%s", i+1, summary(p, each))
		}
	}
	b.WriteString(tail.String())
	return Event{Key: fmt.Sprintf("split-proposed-%d", len(t.Status.Runs)), Body: b.String()}
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

// Resuming says the factory resumes a run its sandbox lost, on its own, n of limit (disruption
// design §4). A reviewer's new run uses no review round.
func Resuming(t *v1alpha1.Task, runID, role string, n, limit int) Event {
	body := fmt.Sprintf("Agent factory task `%s`: run `%s` stopped because %s; resuming automatically (%d/%d).",
		t.Name, runID, Reason("pod_lost"), n, limit)
	if role == "reviewer" {
		body += " The new review run uses no review round."
	}
	return Event{Key: fmt.Sprintf("resume-%d", n), Body: body}
}

// safePath is a contract file the narration may name: the tester wrote the trailer, so anything
// but a plain path is withheld (a narration carries the factory's own words and ids only).
var safePath = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)

// ContractReady says the tester left a contract (test-first tester F3-R5): how many tests, and the
// files that hold them, at most five, each a plain path.
func ContractReady(t *v1alpha1.Task, c *v1alpha1.ContractStatus) Event {
	var files []string
	for _, f := range c.Files {
		switch {
		case len(files) == 5:
		case safePath.MatchString(f):
			files = append(files, "`"+f+"`")
		default:
			files = append(files, "⟦a path withheld⟧")
		}
	}
	more := ""
	if len(c.Files) > len(files) {
		more = fmt.Sprintf(" and %d more", len(c.Files)-len(files))
	}
	return Event{Key: "contract", Body: fmt.Sprintf("Agent factory task `%s`. The tester wrote a contract of %d tests: %s%s. "+
		"The implementer works to it, and the pull request may not change it.", t.Name, len(c.IDs), strings.Join(files, ", "), more)}
}

// NoContract says the tester left none, by its end's category only, and that the implementer
// works without one (TO3).
func NoContract(t *v1alpha1.Task, end string) Event {
	return Event{Key: "contract", Body: fmt.Sprintf("Agent factory task `%s`. The tester left no contract (%s): the implementer works without one.", t.Name, oneWord(end))}
}

// oneWord is an end's category when it is the word it should be, "unreported" otherwise.
func oneWord(s string) string {
	if regexp.MustCompile(`^[a-z_]{1,40}$`).MatchString(s) {
		return s
	}
	return "unreported"
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

// WouldArm is the shadow gate's record (R32): the same decision as Armed, and nothing armed. Its
// key, and so its marker, carries the head it judged (v2 D2): a maintainer's later merge at that
// head is a merge of what the gate would have merged, and a new head is a new decision.
func WouldArm(t *v1alpha1.Task, class, head string) Event {
	return Event{Key: fmt.Sprintf("would-arm-%d head=%s", len(t.Status.Runs), head), Body: fmt.Sprintf("Agent factory task `%s`: "+
		"would auto-merge: `%s`, checks green, verdict approve. The merge gate is in shadow until the "+
		"merge wave, so nothing is armed: a maintainer merges or closes this pull request.", t.Name, class)}
}

// WouldArmAdopted is WouldArm for an adopted pull request (R46): the same key, so the same marker
// the shadow tally reads, and no claim of a verdict, since no agent reviewed Renovate's change. It
// names a file fence, sizes and a release age only when the class declares them.
func WouldArmAdopted(t *v1alpha1.Task, cl config.Class, class, head string) Event {
	e := WouldArm(t, class, head)
	what := "the diff a semver bump"
	if len(cl.AllowedFiles) > 0 || len(cl.ProtectedFiles) > 0 || cl.ModifyOnly || cl.MaxPatchFiles > 0 || cl.MaxModifiedLines > 0 {
		what += " within the class's files and sizes"
	}
	age := ""
	if cl.ReleaseAgeCheck != "" {
		age = ", released long enough ago for the class's release-age check"
	}
	e.Body = fmt.Sprintf("Agent factory: would auto-merge: `%s`, checks green, %s%s. The class is in shadow: nothing is merged by the factory.",
		class, what, age)
	return e
}

// WaitingForHuman says the gate stopped deciding and a maintainer's review is what remains.
func WaitingForHuman(t *v1alpha1.Task, reason string) Event {
	body := fmt.Sprintf("Agent factory task `%s` waits for a maintainer's review: %s.", t.Name, Reason(reason))
	if pr := t.Status.PullRequest; pr != nil && pr.Number > 0 {
		// The ruleset blocks a plain merge, whoever approved (#51).
		body += "\n\nTo merge: " + summ.MergeHow(t.Spec.Repository, pr.Number) + "."
	}
	return Event{Key: "human-" + reason + "-" + fmt.Sprint(len(t.Status.Runs)), Body: body}
}

// WaitingForApproval is WaitingForHuman on a repository with approveToMerge (human path AM3): the
// one action is a review, and the merger does the merge. The by-hand reasons keep WaitingForHuman.
func WaitingForApproval(t *v1alpha1.Task, reason string) Event {
	body := fmt.Sprintf("Agent factory task `%s` waits for a maintainer's review: %s.", t.Name, Reason(reason))
	if pr := t.Status.PullRequest; pr != nil && pr.Number > 0 {
		body += "\n\nTo merge: approve this pull request (a GitHub review by a maintainer). The merger merges the approved head within a minute, or says why not."
	}
	return Event{Key: "human-" + reason + "-" + fmt.Sprint(len(t.Status.Runs)), Body: body}
}

// ArmedByApproval is Armed for a merge a maintainer's approval decided (AM1).
func ArmedByApproval(t *v1alpha1.Task, login string) Event {
	return Event{Key: "armed-" + fmt.Sprint(len(t.Status.Runs)), Body: fmt.Sprintf("Agent factory task `%s`: CI is green, policy-bot "+
		"passed and @%s approved this head, so it is merged now, at the approved head: GitHub re-checks that head at merge time and refuses "+
		"a moved one. It counts as a human merge: no automatic revert, the monitor watches it.", t.Name, login)}
}

// ChecksNeverReported says the wait in AwaitingCI outlasted merge.awaitingCIFor (#182), naming
// the checks that have not reported as completed; waited is the caller's measure, on its own
// clock. Nothing was merged.
func ChecksNeverReported(t *v1alpha1.Task, waited time.Duration, missing []string) Event {
	var head string
	if pr := t.Status.PullRequest; pr != nil {
		head = pr.HeadSHA
	}
	waited = waited.Round(time.Minute)
	return Event{Key: "ci-wait-expired-" + head, Body: fmt.Sprintf("Agent factory task `%s` waited %s for checks that had not reported as completed: %s. "+
		"Nothing was merged; a maintainer decides.", t.Name, waited, strings.Join(missing, ", "))}
}

// BackToGate says a transient hand-off returned to the merge gate (N1), and why. n is the task's
// count of returns, so each one is posted once.
func BackToGate(t *v1alpha1.Task, reason string, n int32) Event {
	return Event{Key: fmt.Sprintf("gate-%s-%d", reason, n),
		Body: fmt.Sprintf("Agent factory task `%s` goes back to the merge gate: %s.", t.Name, Reason(reason))}
}

// Held says the task's room is held — a person drives it, or another run is live in it — so its
// next run waits (N3). Its key is the wait's:
// the task's run count and retries, so a hold that changes shape — the other run ends and a
// person keeps the driver — is said once, and the next wait for a run says it again.
func Held(t *v1alpha1.Task, reason, roomsURL string) Event {
	return Event{Key: fmt.Sprintf("hold-%d-%d", len(t.Status.Runs), t.Status.Retries),
		Body: fmt.Sprintf("Agent factory task `%s` waits: %s. Its next run starts once the room is free: %s/r/%s (tailnet only).",
			t.Name, Reason(reason), strings.TrimSuffix(roomsURL, "/"), t.Status.RoomRef)}
}

// CIFixing says why the next run starts: CI is red on the head, naming each check and its
// conclusion ("name: state"). Once per fix run, as Revising is once per round.
func CIFixing(t *v1alpha1.Task, pr int, head string, checks []string, n, limit int) Event {
	return Event{Key: fmt.Sprintf("ci-fixing-%d", n),
		Body: fmt.Sprintf("Agent factory task `%s`: CI is red on #%d at `%s` (%s); a run starts to fix it (%d/%d).",
			t.Name, pr, head, strings.Join(checks, ", "), n, limit)}
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

// MonitorVerdict is the post-merge monitor's reading of pull request n (MC7), with the signals it
// read. control gives the copy on the control issue its own key (M2).
func MonitorVerdict(t *v1alpha1.Task, n int, verdict string, why []string, control, demotes bool) Event {
	key := fmt.Sprintf("monitor-%d verdict=%s", n, verdict)
	if control {
		key = fmt.Sprintf("monitor-%d control verdict=%s", n, verdict)
	}
	body := fmt.Sprintf("Agent factory monitor of #%d (`%s`): **%s**. %s", n, t.Spec.PredictedClass, verdict, strings.Join(why, " · "))
	if verdict == "regression" {
		if demotes {
			body += "\n\nThe class is demoted at its next decision (breaker.maxFollowUps); nothing is reverted (MC7)."
		} else {
			body += "\n\nbreaker.maxFollowUps is 0: this regression is recorded but not counted toward the breaker; nothing is reverted (MC7)."
		}
	}
	return Event{Key: key, Body: body}
}

// FollowUpMarker is the hidden line that lets the factory find the follow-up issue it filed for
// task's fix, so a lost write or a timed-out create never files a second one.
func FollowUpMarker(task string) string {
	return fmt.Sprintf("<!-- agent-factory follow-up=%s -->", task)
}

// FollowUpIssue is the stub issue RunLore's persisting verdict files (RA12, R13): the merged pull
// request, the alert and the origin room. It carries no evidence: the issue is public.
func FollowUpIssue(task string, pr int, alert, originRoom, roomsURL string) (title, body string) {
	a := finding.LabelSafe(alert)
	body = fmt.Sprintf("RunLore reports that the fix in #%d did not hold: alert `%s` is still firing after the merge.\n\n"+
		"The finding and its evidence stay in the room of the task that proposed it: %s/r/%s (tailnet only).",
		pr, a, strings.TrimSuffix(roomsURL, "/"), originRoom) + "\n\n" + FollowUpMarker(task)
	return fmt.Sprintf("RunLore: the fix in #%d did not hold (%s)", pr, a), body
}

// FollowUpFound says pull request later changed lines pull request m introduced, within the
// monitor's window: it is counted as a follow-up fix (MC8). control: the control issue's copy.
func FollowUpFound(t *v1alpha1.Task, m, later int, control bool) Event {
	key := fmt.Sprintf("follow-up-%d", later)
	if control {
		key += " control"
	}
	return Event{Key: key, Body: fmt.Sprintf("#%d changed lines #%d introduced within the monitor's window (`%s`): counted as a follow-up fix (MC8).",
		later, m, t.Spec.PredictedClass)}
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
	why := Reason(reason)
	if p := t.Payer(); reason == "budget-person" && strings.HasPrefix(p, "system:") {
		why = strings.Replace(why, "the person", "the principal", 1) // a sponsored principal pays, not a person
	}
	if why != "" {
		fmt.Fprintf(&b, ": %s", why)
	}
	if p := t.Payer(); reason == "budget-person" && p != "" {
		fmt.Fprintf(&b, ", `%s`; it resets at 00:00 UTC", p)
	}
	b.WriteString(".")
	if pr := t.Status.PullRequest; pr != nil && pr.MergedBy != "" && phase == v1alpha1.PhaseDone {
		fmt.Fprintf(&b, " Merged by @%s.", strings.TrimSuffix(pr.MergedBy, "[bot]"))
	}
	if phase == v1alpha1.PhaseEscalated && len(maintainers) > 0 {
		b.WriteString("\n\n@" + strings.Join(maintainers, " @") + ": this task needs a maintainer.")
	}
	if h := hints()[phase]; h != "" && reason != "budget-family" {
		b.WriteString("\n\n" + h)
	}
	if v1alpha1.TerminalPhase(phase) {
		b.WriteString(usageLine + ": " + Tokens(t.Status.Usage.Tokens) + ".")
	} else { // a retry can still spend: this is the total so far
		b.WriteString(usageLine + " so far: " + Tokens(t.Status.Usage.Tokens) + ".")
	}
	return Event{Key: fmt.Sprintf(closingPrefix+"%s-%d", strings.ToLower(phase), len(t.Status.Runs)), Body: b.String()}
}

const (
	closingPrefix = "end-"
	usageLine     = "\n\nTokens used"
)

// Closing reports whether key is an ending's: its token total is only final once the usage settles.
func Closing(key string) bool { return strings.HasPrefix(key, closingPrefix) }

// Retotal rewrites an ending's "Tokens used" line with the settled total (#42).
func Retotal(body string, tokens int64, incomplete bool) string {
	if i := strings.LastIndex(body, usageLine); i >= 0 {
		if incomplete { // a run the meter never read: the total is a floor
			return body[:i] + usageLine + ": at least " + Tokens(tokens) + " (a run's usage was not read)."
		}
		return body[:i] + usageLine + ": " + Tokens(tokens) + "."
	}
	return body
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

// PersonOverBudget refuses a label whose labeller's day is spent (v2 per-person caps D4): no Task,
// the person, their cap and the reset named, so re-labelling after it retries.
func PersonOverBudget(number int, person string, capMicroUSD int64, at time.Time) Event {
	return Event{Key: fmt.Sprintf("refused-person_budget_exhausted-%d", at.Unix()),
		Body: fmt.Sprintf("The agent factory did not start a task for #%d: the daily budget of `%s` (%s) is spent. "+
			"It resets at 00:00 UTC; re-apply the label after that to start it.", number, person, USD(capMicroUSD))}
}

// ApproverOverBudget refuses a `/factory split` whose approver's day is spent (splitter design
// SL11): the split keeps waiting, and the approver comments again after the reset. Re-labelling
// would supersede the proposal, so it is not offered.
func ApproverOverBudget(number int, person string, capMicroUSD int64, at time.Time) Event {
	return Event{Key: fmt.Sprintf("refused-split_approver_budget-%d", at.Unix()),
		Body: fmt.Sprintf("The agent factory did not split #%d: the daily budget of `%s` (%s) is spent, and every part is charged to the person who approves. "+
			"It resets at 00:00 UTC; comment `/factory split` again after that. The proposal stays open.", number, person, USD(capMicroUSD))}
}

// SplitMarker is the hidden line that finds part k's issue again (splitter design SL8), as Marker
// finds a comment.
func SplitMarker(parent string, k int) string {
	return fmt.Sprintf("<!-- agent-factory split=%s/%d -->", parent, k)
}

// ChildIssue is part k's issue (SL8): the factory's own title, and a body that quotes the part's
// scope as an inert block for a public task, or links the room for an internal one, with the marker.
func ChildIssue(t *v1alpha1.Task, k int32, sc *envelope.Scope, roomsURL string) (title, body string) {
	s := t.Status.Split
	title = fmt.Sprintf("Part %d of %d of #%d", k, s.Of, t.Spec.Issue)
	var b strings.Builder
	fmt.Fprintf(&b, "Part %d of %d of #%d, opened by agent factory task `%s` once %s approved its split with `/factory split`. "+
		"Its task runs on branch `agent/%s`, through the merge gate of #%d's class; the next part opens once this one merges.",
		k, s.Of, t.Spec.Issue, t.Name, strings.TrimPrefix(s.ApprovedBy, "github:"), taskid.Name(taskid.SplitKey(t.Name, int(k))), t.Spec.Issue)
	if t.Spec.DataClass != "public" {
		fmt.Fprintf(&b, "\n\nThe part's scope is in [the room](%s/r/%s): the task's data class is not public.",
			strings.TrimSuffix(roomsURL, "/"), t.Status.RoomRef)
	} else {
		var q strings.Builder
		fmt.Fprintf(&q, "Task: %s\nTouches: %s\n", sc.Task, strings.Join(sc.Touches, ", "))
		for i, p := range sc.Plan {
			fmt.Fprintf(&q, "%d. %s\n", i+1, p)
		}
		for _, a := range sc.Acceptance {
			fmt.Fprintf(&q, "Acceptance: %s: proves %s\n", a.Check, a.Proves)
		}
		b.WriteString("\n\nThe part, as the scoper wrote it:\n\n" + summary(q.String(), 3*maxBody/2))
	}
	b.WriteString("\n\nApply `factory/stop` here to stop this part, or on #" + strconv.Itoa(t.Spec.Issue) + " to stop every part.\n\n" +
		SplitMarker(t.Name, int(k)))
	return title, b.String()
}

// SplitPart says on the parent issue that part k opened (SL7), once.
func SplitPart(t *v1alpha1.Task, k int32, issue int) Event {
	return Event{Key: fmt.Sprintf("split-part-%d", k), Body: fmt.Sprintf("Agent factory task `%s`: part %d of %d is #%d.",
		t.Name, k, t.Status.Split.Of, issue)}
}

// Part is one part of a split as its parent's end lists it.
type Part struct {
	Index   int32
	Issue   int
	PR      string
	Outcome string
}

// SplitEnded is the parent's ending (SL12): Ended, with its parts and their pull requests before the
// token line, so the settle's retotal still finds it (#42).
func SplitEnded(t *v1alpha1.Task, phase, reason string, parts []Part, maintainers ...string) Event {
	e := Ended(t, phase, reason, maintainers...)
	var b strings.Builder
	b.WriteString("\n\nParts:")
	for _, p := range parts {
		fmt.Fprintf(&b, "\n- part %d, #%d: %s", p.Index, p.Issue, cmp.Or(p.Outcome, "open"))
		if p.PR != "" {
			fmt.Fprintf(&b, " (%s)", p.PR)
		}
	}
	if i := strings.LastIndex(e.Body, usageLine); i >= 0 {
		e.Body = e.Body[:i] + b.String() + e.Body[i:]
	}
	return e
}

// PartReverted says on a closed parent's issue that one of its parts was reverted (SP6): whether the
// issue is solved is a person's call.
func PartReverted(t *v1alpha1.Task, revert int, parentClosed bool) Event {
	p := t.Spec.Parent
	tail := "This issue stays closed: reopen it if the work is not done."
	if !parentClosed {
		tail = "The parent issue is waiting on its parts: its family no longer counts this part as landed."
	}
	return Event{Key: fmt.Sprintf("part-reverted-%d", revert), Body: fmt.Sprintf("Agent factory task `%s`, part %d of %d of this "+
		"issue (#%d), was reverted by #%d. %s", t.Name, p.Index, p.Of, t.Spec.Issue, revert, tail)}
}

// USD is a micro-USD amount as people read it, to the cent: $8.50.
func USD(micro int64) string { return fmt.Sprintf("$%.2f", float64(micro)/1e6) }

// NoVerdict says a reviewer run ended without a verdict the factory can act on, and that
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
