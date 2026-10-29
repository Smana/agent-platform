// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/factory/forge"
	"github.com/Smana/maku/internal/factory/narrate"
	"github.com/Smana/maku/internal/factory/sanitize"
)

// LabelPublish asks the factory to file an investigate task's proposal as a new issue (RunLore
// agent-to-agent RA9).
const LabelPublish = "factory/publish"

// publishes reads factory/publish on the stub issues of investigate tasks: a maintainer's label
// files the proposal once, sanitised, unlabelled; anything else is answered and consumed. One
// issue's failure does not keep the others waiting.
func (p *IssuePoller) publishes(ctx context.Context) error {
	items, err := p.Forge.Labeled(ctx, LabelPublish)
	if err != nil || len(items) == 0 {
		return err
	}
	var errs []error
	for _, it := range items {
		if it.PullRequest {
			continue
		}
		if err := p.publishOne(ctx, it.Number); err != nil {
			errs = append(errs, fmt.Errorf("publish #%d: %w", it.Number, err))
		}
	}
	return errors.Join(errs...)
}

// refusePublish answers the label once, keyed by its time, then consumes it. Nothing is created.
func (p *IssuePoller) refusePublish(ctx context.Context, n int, last forge.LabelEvent, reason string) error {
	return p.refuseWith(ctx, n, narrate.PublishRefused(n, reason, last.At))
}

// refuseWith posts a refusal's event once, then consumes the label.
func (p *IssuePoller) refuseWith(ctx context.Context, n int, e narrate.Event) error {
	if err := p.narrator().PostOnce(ctx, n, fmt.Sprintf("issue-%d", n), e); err != nil {
		return err
	}
	return p.Forge.RemoveLabel(ctx, n, LabelPublish)
}

func (p *IssuePoller) publishOne(ctx context.Context, n int) error {
	evs, ok, err := p.labelEvents(ctx, n, LabelPublish)
	if err != nil || !ok {
		return err
	}
	// Only the last label event's actor counts, as for factory/ready: a maintainer's label an
	// outsider re-applied is the outsider's.
	_, last, ok := Generation(evs, p.repo().IsMaintainer)
	if !ok {
		return p.refusePublish(ctx, n, last, "unauthorised_labeller")
	}
	switch w, err := p.writer(ctx, last.Actor); {
	case err != nil:
		return err // no answer is no refusal: the label waits for the next poll
	case !w:
		return p.refusePublish(ctx, n, last, "unauthorised_labeller")
	}
	t, err := p.investigateOf(ctx, n)
	if err != nil {
		return err
	}
	switch {
	case t == nil:
		return p.refusePublish(ctx, n, last, "no_proposal")
	case t.Status.Published != nil: // once per task: the label points at what was filed
		if err := p.narrator().PostOnce(ctx, n, fmt.Sprintf("issue-%d", n), narrate.Published(n, t.Status.Published.Issue, t.Status.Published.By)); err != nil {
			return err
		}
		return p.Forge.RemoveLabel(ctx, n, LabelPublish)
	case t.Status.Phase == v1alpha1.PhaseRejected: // never admitted, so no run made a proposal: say why (task 2d5ch27g)
		return p.refuseWith(ctx, n, narrate.PublishRejected(n, t.Name, t.Status.Reason, last.At))
	case !v1alpha1.TerminalPhase(t.Status.Phase):
		return p.refusePublish(ctx, n, last, "task_not_ended")
	case t.Status.Phase != v1alpha1.PhaseDone: // a stopped, closed or empty task's handoff is no proposal
		return p.refusePublish(ctx, n, last, "no_proposal")
	}
	// Filing is made idempotent by a marker label on the new issue, which grants nothing: a crash
	// between CreateIssue and the record, or a record that failed, leaves an issue the retry finds
	// and reuses, so a re-label never files a duplicate.
	marker := PublishedFromLabel(t.Name)
	found, err := p.Forge.IssuesLabelled(ctx, marker)
	if err != nil {
		return fmt.Errorf("look for an earlier attempt: %w", err)
	}
	filed := forge.Issue{}
	for _, i := range found {
		if !forge.SameLogin(i.Author, p.Cfg.FactoryLogin) {
			continue // a label anyone can apply is no record: only the factory's own issue counts
		}
		if filed.Number == 0 || i.Number < filed.Number {
			filed = i
		}
	}
	if filed.Number == 0 {
		title, body, reason, err := p.draftOf(ctx, t)
		switch {
		case err != nil:
			return err
		case reason != "":
			return p.refusePublish(ctx, n, last, reason)
		}
		// No factory/ready: the maintainer reads the issue and starts it.
		filed, err = p.Forge.CreateIssue(ctx, title,
			body+fmt.Sprintf("\n\n---\nPublished by @%s from agent factory task `%s`.", last.Actor, t.Name), []string{marker})
		if err != nil {
			return fmt.Errorf("create the issue: %w", err)
		}
	}
	iss := filed
	// The label stays only while the issue is unrecorded: past this point a failure is logged with
	// the issue's number and the label still goes, so a retry files no second issue.
	by := strings.ToLower(last.Actor)
	if err := p.recordPublished(ctx, t, iss.Number, by); err != nil {
		p.log().Error("the published issue is not recorded on its task", "task", t.Name, "issue", iss.Number, "error", err)
	}
	if err := p.narrator().PostOnce(ctx, n, fmt.Sprintf("issue-%d", n), narrate.Published(n, iss.Number, by)); err != nil {
		return err
	}
	return p.Forge.RemoveLabel(ctx, n, LabelPublish)
}

// PublishedFromLabel marks the issue filed from task name's proposal: the retry's way to find it.
func PublishedFromLabel(name string) string { return "factory/published-from:" + name }

// investigateOf is the newest investigate task of this repository's issue n, nil when there is none.
func (p *IssuePoller) investigateOf(ctx context.Context, n int) (*v1alpha1.Task, error) {
	var l v1alpha1.TaskList
	if err := p.Client.List(ctx, &l, client.InNamespace(p.Namespace), client.MatchingLabels{v1alpha1.LabelIssue: strconv.Itoa(n)}); err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	var found *v1alpha1.Task
	for i := range l.Items {
		t := &l.Items[i]
		// A task rejected at admission was never triaged, so it has no template yet: it is still the
		// issue's task, and its rejection is the refusal's reason.
		rejected := t.Spec.Template == "" && t.Status.Phase == v1alpha1.PhaseRejected
		if (t.Spec.Template != "investigate" && !rejected) || t.Spec.Issue != n || !p.owns(t) {
			continue
		}
		if found == nil || t.CreationTimestamp.After(found.CreationTimestamp.Time) {
			found = t
		}
	}
	return found, nil
}

// draftOf is the public issue made from t's proposal: its first line the title, the rest the body,
// held to the issue template, made inert and scanned for secrets. reason is a refusal's code.
func (p *IssuePoller) draftOf(ctx context.Context, t *v1alpha1.Task) (title, body, reason string, err error) {
	if p.Proposal == nil {
		return "", "", "", errors.New("intake: no proposal reader")
	}
	proposal, err := p.Proposal(ctx, t)
	switch {
	case errors.Is(err, ErrNoProposal):
		return "", "", "no_proposal", nil
	case err != nil:
		return "", "", "", err
	}
	first, rest, _ := strings.Cut(proposal, "\n")
	d, err := sanitize.CheckDraft(strings.TrimLeft(first, "# \t"), rest)
	if err != nil {
		return "", "", "invalid_draft", nil
	}
	title, tr := sanitize.Public(d.Title)
	body, br := sanitize.Public(d.Body)
	if tr.Withheld || br.Withheld {
		return "", "", "invalid_draft", nil
	}
	if p.Redact == nil {
		return "", "", "redacted", nil // no scan, no publication
	}
	if _, rules := p.Redact.String(title + "\n" + body); len(rules) > 0 {
		return "", "", "redacted", nil
	}
	return title, body, "", nil
}

// ErrNoProposal is what a Proposal reader answers when the task has none.
var ErrNoProposal = errors.New("intake: the task has no proposal")

// recordPublished writes status.published and the published-issue label on t: the record
// originOf reads, the factory's own state, never issue text (R9).
func (p *IssuePoller) recordPublished(ctx context.Context, t *v1alpha1.Task, issue int, by string) error {
	status, _ := json.Marshal(map[string]any{"status": map[string]any{"published": v1alpha1.Published{
		Issue: issue, By: by, At: metav1.NewTime(p.now())}}})
	label, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{
		v1alpha1.LabelPublishedIssue: strconv.Itoa(issue)}}})
	obj := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: t.Name, Namespace: t.Namespace}}
	return errors.Join(
		retryConflict(func() error {
			return p.Client.Status().Patch(ctx, obj.DeepCopy(), client.RawPatch(types.MergePatchType, status))
		}),
		retryConflict(func() error { return p.Client.Patch(ctx, obj.DeepCopy(), client.RawPatch(types.MergePatchType, label)) }))
}

// retryConflict tries do three times while the API server answers a conflict.
func retryConflict(do func() error) error {
	var err error
	for range 3 {
		if err = do(); !apierrors.IsConflict(err) {
			return err
		}
	}
	return err
}

// originOf is the investigate task whose published issue is n (RA9): the task of this repository
// labelled published-issue n whose own status.published names n. Nothing in the issue's text is
// read, so no text can forge an origin. "" when no task qualifies.
func (p *IssuePoller) originOf(ctx context.Context, n int) (string, error) {
	var l v1alpha1.TaskList
	if err := p.Client.List(ctx, &l, client.InNamespace(p.Namespace), client.MatchingLabels{v1alpha1.LabelPublishedIssue: strconv.Itoa(n)}); err != nil {
		return "", fmt.Errorf("list tasks: %w", err)
	}
	i := slices.IndexFunc(l.Items, func(t v1alpha1.Task) bool {
		return p.owns(&t) && t.Status.Published != nil && t.Status.Published.Issue == n
	})
	if i < 0 {
		return "", nil
	}
	return l.Items[i].Name, nil
}

// now is the poller's clock.
func (p *IssuePoller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}
