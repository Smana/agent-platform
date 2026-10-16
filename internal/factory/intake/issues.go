// SPDX-License-Identifier: Apache-2.0

// Package intake turns triggers into Tasks (§1). GitHub never redelivers a failed webhook, so the
// factory polls for labels and exposes no public endpoint: one list call a minute for
// factory/ready, one for factory/stop, and a few calls per labelled issue. The RunLore intake is
// the one push endpoint, tailnet-only behind a bearer token (FR-9, FA-8).
package intake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/fmetrics"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/killswitch"
	"github.com/Smana/agent-platform/internal/factory/narrate"
	"github.com/Smana/agent-platform/internal/factory/sanitize"
	"github.com/Smana/agent-platform/internal/factory/taskid"
)

// LabelStop asks the factory to stop the task of the issue or PR carrying it (§6.1).
const LabelStop = "factory/stop"

// LabelRevert asks the factory to revert the auto-merged task whose PR carries it (§6.4).
const LabelRevert = "factory/revert"

// maxSnapshot bounds the stored text for etcd (the Task CRD's maxLength); admission refuses
// anything above caps.maxTextBytes anyway (R6).
const maxSnapshot = 65536

// issueForge is the part of the forge the poller uses, as the factory App.
type issueForge interface {
	Labeled(ctx context.Context, label string) ([]forge.Item, error)
	LabelEvents(ctx context.Context, number int, label string) ([]forge.LabelEvent, error)
	Issue(ctx context.Context, number int) (forge.Issue, error)
	RemoveLabel(ctx context.Context, number int, label string) error
	RecentComments(ctx context.Context, number int) ([]forge.Comment, error)
	Comment(ctx context.Context, number int, body string) error
	AgentPulls(ctx context.Context) ([]forge.AgentPull, error)
	AddLabels(ctx context.Context, number int, labels ...string) error
}

// errorCounter counts a failed poll, and a label left for the next one (fmetrics.Set).
type errorCounter interface {
	IntakeError(ctx context.Context, source string)
	LabelEventsTruncated(ctx context.Context, label string)
}

// IssuePoller is a leader-only manager.Runnable: it reads factory/ready and factory/stop labels
// and creates or stops Tasks. Stopped reports the kill switch: intake pauses while it holds, stop
// labels do not.
type IssuePoller struct {
	Forge     issueForge
	Client    client.Client
	Namespace string
	Cfg       *config.Config
	// Repository names the config entry this poller serves, and Forge must be bound to it (v2
	// D1). With one entry it may be left empty.
	Repository string
	// CanWrite is v2 D5's GitHub half: whether login holds write or higher on this poller's
	// repository. A maintainers entry drifts from GitHub silently; this does not. nil starts
	// nothing: a label is never trusted on the list alone.
	CanWrite func(ctx context.Context, login string) (bool, error)
	Stopped  func(context.Context) bool
	Errors   errorCounter
	Log      *slog.Logger
	// Ticker starts the period; nil means a time.Ticker.
	Ticker func(d time.Duration) (c <-chan time.Time, stop func())
	// Sanitize is the snapshot's sanitiser; nil means sanitize.Text. A test forces a withheld text.
	Sanitize func(string) (string, sanitize.Report)
}

func (p *IssuePoller) repo() *config.Repository { return p.Cfg.RepositoryFor(p.Repository) }

// controlIssue is the kill switch's issue when it lives on this poller's repository, else 0.
func (p *IssuePoller) controlIssue() int {
	if ctl := p.Cfg.ControlFor(); ctl.Issue > 0 && strings.EqualFold(ctl.Repository, p.repo().Name) {
		return ctl.Issue
	}
	return 0
}

// owns: the task is on this poller's repository. One entry owns every task, as before the list.
func (p *IssuePoller) owns(t *v1alpha1.Task) bool {
	return len(p.Cfg.Repos()) == 1 || strings.EqualFold(t.Spec.Repository, p.repo().Name)
}

// NeedLeaderElection is true: two pollers would race each label.
func (p *IssuePoller) NeedLeaderElection() bool { return true }

func (p *IssuePoller) log() *slog.Logger {
	if p.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return p.Log
}

// labelEvents reads label's events on issue n. ok is false when the poller should leave the label
// for the next poll: GitHub has not listed its event yet, or the events passed the forge's cap
// and the newest, the one that counts, is unread (ruling SP).
func (p *IssuePoller) labelEvents(ctx context.Context, n int, label string) ([]forge.LabelEvent, bool, error) {
	evs, err := p.Forge.LabelEvents(ctx, n, label)
	if errors.Is(err, forge.ErrEventsTruncated) {
		p.log().Warn("label events truncated: the label waits", "issue", n, "label", label, "events", len(evs))
		if p.Errors != nil {
			p.Errors.LabelEventsTruncated(ctx, label)
		}
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return evs, len(evs) > 0, nil
}

func (p *IssuePoller) narrator() narrate.Narrator {
	return narrate.Narrator{Forge: p.Forge, Login: p.Cfg.FactoryLogin}
}

// Start polls at once, then every poll.issues, until ctx ends. A failed poll is counted and
// logged, and the next one retries.
func (p *IssuePoller) Start(ctx context.Context) error {
	every := p.Cfg.Poll.Issues.Duration
	if every <= 0 {
		return fmt.Errorf("intake: the issue poll period %s is not positive", every)
	}
	tick := p.Ticker
	if tick == nil {
		tick = func(d time.Duration) (<-chan time.Time, func()) { t := time.NewTicker(d); return t.C, t.Stop }
	}
	c, stop := tick(every)
	defer stop()
	ctx = fmetrics.WithRepository(ctx, p.repo().Name) // v2 D10: this poller's errors are its repository's
	for {
		if err := p.Poll(ctx); err != nil {
			if p.Errors != nil {
				p.Errors.IntakeError(ctx, "issue")
			}
			p.log().Warn("issue poll failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-c:
		}
	}
}

// Poll is one pass: the control issue's label first (§6.1), then stop labels, honoured even with
// intake paused, then reverts (§6.4's rollback path is a maintainer's, not new work), then each
// issue carrying the trigger label, then the orphan scan (R51), which also runs on leader start —
// this poll is the first thing Start runs. One issue's failure does not keep the others waiting.
func (p *IssuePoller) Poll(ctx context.Context) error {
	var publishErr error
	engaged := false // reflects the stop label this pass saw or set, so the reader's cache may not hold it yet; it errs safe if the publish fails
	if n := p.controlIssue(); n > 0 {
		iss, err := p.Forge.Issue(ctx, n)
		if err != nil {
			return err
		}
		on := slices.Contains(iss.Labels, LabelStop)
		engaged = on
		// A failed publish must not hold back the stop labels and reverts of this pass.
		publishErr = killswitch.PublishIssue(ctx, p.Client, p.Namespace, on)
	}
	if err := p.stops(ctx); err != nil {
		return errors.Join(publishErr, err)
	}
	if err := p.reverts(ctx); err != nil {
		return errors.Join(publishErr, err)
	}
	if engaged || p.Stopped(ctx) {
		return publishErr
	}
	items, err := p.Forge.Labeled(ctx, p.repo().TriggerLabel)
	if err != nil {
		return fmt.Errorf("intake: %w", err)
	}
	var errs []error
	for _, it := range items {
		if it.PullRequest {
			continue // factory/ready starts work from issues only
		}
		if err := p.one(ctx, it.Number); err != nil {
			errs = append(errs, fmt.Errorf("#%d: %w", it.Number, err))
		}
	}
	if err := p.orphans(ctx); err != nil {
		errs = append(errs, fmt.Errorf("orphans: %w", err))
	}
	return errors.Join(append(errs, publishErr)...)
}

// Generation counts maintainers' labels (R4). ok is false when the latest label is not a
// maintainer's: that is the label currently on the issue, and it starts nothing (§1).
func Generation(evs []forge.LabelEvent, isMaintainer func(string) bool) (int, forge.LabelEvent, bool) {
	if len(evs) == 0 {
		return 0, forge.LabelEvent{}, false
	}
	evs = slices.Clone(evs)
	slices.SortStableFunc(evs, func(a, b forge.LabelEvent) int { return a.At.Compare(b.At) })
	gen := 0
	for _, e := range evs {
		if isMaintainer(e.Actor) {
			gen++
		}
	}
	last := evs[len(evs)-1]
	return gen, last, isMaintainer(last.Actor)
}

// Snapshot is the text the task keeps, sanitised (G2), and the sha256 of the issue as the
// maintainer labelled it, so they can match it (§1). sanitize.Text folds compatibility forms and
// neutralises look-alikes of the brief's fence (Batch A I1) along with images and markup.
func Snapshot(i forge.Issue) (string, string, sanitize.Report) { return snapshot(i, sanitize.Text) }

func snapshot(i forge.Issue, clean func(string) (string, sanitize.Report)) (string, string, sanitize.Report) {
	raw := "# " + i.Title + "\n\n" + i.Body
	sum := sha256.Sum256([]byte(raw))
	text, rep := clean(raw)
	if len(text) > maxSnapshot {
		text = strings.ToValidUTF8(text[:maxSnapshot], "")
	}
	return text, hex.EncodeToString(sum[:]), rep
}

func (p *IssuePoller) one(ctx context.Context, n int) error {
	label := p.repo().TriggerLabel
	evs, ok, err := p.labelEvents(ctx, n, label)
	if err != nil || !ok {
		return err
	}
	gen, last, ok := Generation(evs, p.repo().IsMaintainer)
	if !ok {
		return p.refuse(ctx, n, last, "unauthorised_labeller")
	}
	switch w, err := p.writer(ctx, last.Actor); {
	case err != nil:
		return err // no answer is no refusal: the label waits for the next poll
	case !w:
		return p.refuse(ctx, n, last, "unauthorised_labeller")
	}
	iss, err := p.Forge.Issue(ctx, n)
	if err != nil {
		return err
	}
	if iss.LastEditedAt.After(last.At) || iss.TitleEditedAt.After(last.At) {
		return p.refuse(ctx, n, last, "edited_after_label") // R5: the body and the title are the snapshot
	}
	clean := p.Sanitize
	if clean == nil {
		clean = sanitize.Text
	}
	text, sum, rep := snapshot(iss, clean)
	if rep.Withheld {
		return p.refuse(ctx, n, last, "unsanitisable") // ruling SM: no task runs on a withheld text
	}
	if rep.Changed() {
		p.log().Info("issue text sanitised", "issue", n, "report", rep.String())
	}
	key := taskid.IssueKey(p.repo().Name, n, gen)
	name := taskid.Name(key)
	open, err := p.openTasks(ctx, n)
	if err != nil {
		return err
	}
	// R4: any other task still moving refuses the label; escalated ones are superseded, but only
	// when nothing else is running, so a label never ends one task beside a live one.
	var superseded []*v1alpha1.Task
	for _, t := range open {
		switch {
		case t.Name == name:
		case t.Status.Phase == v1alpha1.PhaseEscalated:
			superseded = append(superseded, t)
		default:
			return p.refuse(ctx, n, last, "task_active")
		}
	}
	for _, t := range superseded {
		if err := p.annotateStop(ctx, t, "superseded"); err != nil {
			return err
		}
	}
	t := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.Namespace, Labels: map[string]string{v1alpha1.LabelIssue: strconv.Itoa(n)}},
		Spec: v1alpha1.TaskSpec{
			Source: v1alpha1.Source{Kind: "issue", Ref: fmt.Sprintf("%s#%d", p.repo().Name, n), Key: key,
				RequestedBy: "github:" + last.Actor, Trust: "untrusted", ContentSHA256: sum},
			Repository: p.repo().Name, Issue: n, IssueAuthor: iss.Author, Text: text, DataClass: p.Cfg.Defaults.DataClass,
		},
	}
	if err := p.Client.Create(ctx, t); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create task %s: %w", name, err) // AlreadyExists is the dedup (S2)
	}
	return p.Forge.RemoveLabel(ctx, n, label)
}

// writer is v2 D5's GitHub check on login; without CanWrite nobody is one.
func (p *IssuePoller) writer(ctx context.Context, login string) (bool, error) {
	if p.CanWrite == nil {
		return false, nil
	}
	ok, err := p.CanWrite(ctx, login)
	if err != nil {
		return false, fmt.Errorf("write check of %s: %w", login, err)
	}
	return ok, nil
}

// refuse answers the label once, keyed by its time, then consumes it (R4).
func (p *IssuePoller) refuse(ctx context.Context, n int, last forge.LabelEvent, reason string) error {
	if err := p.narrator().PostOnce(ctx, n, fmt.Sprintf("issue-%d", n), narrate.Refused(n, reason, last.At)); err != nil {
		return err
	}
	return p.Forge.RemoveLabel(ctx, n, p.repo().TriggerLabel)
}

// openTasks are this repository's issue n's tasks that have not ended.
func (p *IssuePoller) openTasks(ctx context.Context, n int) ([]*v1alpha1.Task, error) {
	var l v1alpha1.TaskList
	if err := p.Client.List(ctx, &l, client.InNamespace(p.Namespace), client.MatchingLabels{v1alpha1.LabelIssue: strconv.Itoa(n)}); err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	var open []*v1alpha1.Task
	for i := range l.Items {
		// The issue label is a number, and two repositories share numbers: only this one's count.
		if !v1alpha1.TerminalPhase(l.Items[i].Status.Phase) && p.owns(&l.Items[i]) {
			open = append(open, &l.Items[i])
		}
	}
	return open, nil
}

// annotateStop asks the reconciler to stop t: a merge patch of the one annotation.
func (p *IssuePoller) annotateStop(ctx context.Context, t *v1alpha1.Task, why string) error {
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{v1alpha1.AnnotationStop: why}}})
	if err != nil {
		return fmt.Errorf("stop patch: %w", err)
	}
	obj := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: t.Name, Namespace: t.Namespace}}
	if err := p.Client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, body)); err != nil {
		return fmt.Errorf("stop task %s: %w", t.Name, err)
	}
	return nil
}

// stops maps a maintainer's factory/stop on an issue or a PR to its running task (≤ 60 s, §6.1),
// then consumes the label. A maintainer's stop with no running task stays, so a task started later
// on that issue stops at once. Anyone else's is removed and answered once (ruling SP): left on,
// it would stop that later task on their say.
func (p *IssuePoller) stops(ctx context.Context) error {
	items, err := p.Forge.Labeled(ctx, LabelStop)
	if err != nil {
		return fmt.Errorf("intake: %w", err)
	}
	if len(items) == 0 {
		return nil
	}
	var l v1alpha1.TaskList
	if err := p.Client.List(ctx, &l, client.InNamespace(p.Namespace)); err != nil {
		return fmt.Errorf("list tasks: %w", err)
	}
	for _, it := range items {
		if it.Number == p.controlIssue() {
			continue // the control issue keeps its factory/stop: removing it is how a human resumes (§6.1)
		}
		evs, ok, err := p.labelEvents(ctx, it.Number, LabelStop)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if _, last, byMaintainer := Generation(evs, p.repo().IsMaintainer); !byMaintainer {
			if err := p.narrator().PostOnce(ctx, it.Number, fmt.Sprintf("issue-%d", it.Number),
				narrate.StopIgnored(it.Number, "unauthorised_stopper", last.At)); err != nil {
				return err
			}
			if err := p.Forge.RemoveLabel(ctx, it.Number, LabelStop); err != nil {
				return err
			}
			continue
		}
		hit := false
		for i := range l.Items {
			t := &l.Items[i]
			if v1alpha1.TerminalPhase(t.Status.Phase) || !p.owns(t) {
				continue
			}
			if t.Spec.Issue == it.Number || (t.Status.PullRequest != nil && t.Status.PullRequest.Number == it.Number) {
				if err := p.annotateStop(ctx, t, "label"); err != nil {
					return err
				}
				hit = true
			}
		}
		if hit {
			if err := p.Forge.RemoveLabel(ctx, it.Number, LabelStop); err != nil {
				return err
			}
		}
	}
	return nil
}

// reverts maps a maintainer's factory/revert on an auto-merged PR to its task (§6.4). Merged
// PRs are closed, so this lists labelled items in every state.
func (p *IssuePoller) reverts(ctx context.Context) error {
	items, err := p.Forge.Labeled(ctx, LabelRevert)
	if err != nil || len(items) == 0 {
		return err
	}
	var l v1alpha1.TaskList
	if err := p.Client.List(ctx, &l, client.InNamespace(p.Namespace)); err != nil {
		return fmt.Errorf("list tasks: %w", err)
	}
	for _, it := range items {
		evs, ok, err := p.labelEvents(ctx, it.Number, LabelRevert)
		if err != nil || !ok {
			continue // GitHub has not listed it yet, or its newest events are unread: it waits
		}
		actor := evs[len(evs)-1].Actor
		if !p.repo().IsMaintainer(actor) {
			continue // labels are intent, and only a maintainer's counts
		}
		if w, err := p.writer(ctx, actor); err != nil || !w {
			continue // a revert opens a pull request: a write, so D5's check holds it too
		}
		for i := range l.Items {
			t := &l.Items[i]
			if ref := t.Status.PullRequest; ref != nil && ref.Number == it.Number && ref.AutoMerged && p.owns(t) {
				body, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{v1alpha1.AnnotationRevert: "label"}}})
				obj := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: t.Name, Namespace: t.Namespace}}
				if err := p.Client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, body)); err != nil {
					return fmt.Errorf("revert task %s: %w", t.Name, err)
				}
			}
		}
		if err := p.Forge.RemoveLabel(ctx, it.Number, LabelRevert); err != nil {
			return err
		}
	}
	return nil
}
