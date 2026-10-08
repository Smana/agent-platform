// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/narrate"
)

// LabelOrphaned marks a pull request whose task was lost (R51): human-only from then on.
const LabelOrphaned = "factory/orphaned"

// agentBranch is the head a factory run writes (C3), with the task's C2 name in it.
var agentBranch = regexp.MustCompile(`^agent/([a-z2-7]{8})$`)

// orphaned is the one narration an orphaned pull request gets (R51). Its key is the lost task's
// name, so the marker dedups any later scan of the same pull request.
func orphaned(name string) narrate.Event {
	return narrate.Event{Key: name, Body: fmt.Sprintf(
		"The agent factory task `%s` this pull request was opened for was lost in a cluster rebuild. "+
			"It is human-only now: re-label its issue `factory/ready` to start the work again.", name)}
}

// orphans is the orphan scan (R51), run on leader start and every issues poll: an open pull
// request from an agent branch of this repository, carrying the predicted class, with no Task of
// that name, is all that survived a cluster rebuild — the Task, its room and its runs are gone.
// It is labelled factory/orphaned and answered once; nothing is ever adopted from it (R52: the
// branch and the footer are forgeable hints, so the factory starts no work from them).
func (p *IssuePoller) orphans(ctx context.Context) error {
	pulls, err := p.Forge.AgentPulls(ctx)
	if err != nil {
		return fmt.Errorf("list agent pull requests: %w", err)
	}
	var errs []error
	for _, pr := range pulls {
		m := agentBranch.FindStringSubmatch(pr.HeadRef)
		if m == nil || pr.Fork || !classLabel(pr.Labels) {
			continue // not the factory's to judge: not a task's branch, a fork's, or unlabelled
		}
		var t v1alpha1.Task
		err := p.Client.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: m[1]}, &t)
		if err == nil {
			continue // the task lives, whatever its phase
		}
		if !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("get task %s: %w", m[1], err))
			continue
		}
		if !slices.Contains(pr.Labels, LabelOrphaned) {
			if err := p.Forge.AddLabels(ctx, pr.Number, LabelOrphaned); err != nil {
				errs = append(errs, fmt.Errorf("label #%d: %w", pr.Number, err))
				continue // the narration waits for the label it announces
			}
		}
		if err := p.narrator().PostOnce(ctx, pr.Number, "orphan", orphaned(m[1])); err != nil {
			errs = append(errs, fmt.Errorf("narrate #%d: %w", pr.Number, err))
		}
	}
	return errors.Join(errs...)
}

// classLabel: the pull request carries the predicted class the factory stamped on it.
func classLabel(labels []string) bool {
	return slices.ContainsFunc(labels, func(l string) bool { return strings.HasPrefix(l, "factory/class:") })
}
