// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/taskid"
)

// scheduleLookback bounds how far LastSlot walks back: a factory that was down for a day must
// not replay a week of slots, and a weekly schedule still names its last firing.
const scheduleLookback = 8 * 24 * time.Hour

// scheduleEvery is how often the scheduler weighs every configured cron. Slots are minute
// shaped; a minute catches each one at its start, as the issue poller's minute does its labels.
const scheduleEvery = time.Minute

// LastSlot is the latest time ≤ now the cron expression names, looking back 8 days.
func LastSlot(expr string, now time.Time) (time.Time, bool) {
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}, false
	}
	var last time.Time
	for t := sched.Next(now.Add(-scheduleLookback)); !t.After(now); t = sched.Next(t) {
		last = t
	}
	return last, !last.IsZero()
}

// scheduleForge is the part of the forge the probe reads as the factory App: the open pull
// requests, whose checks it then hands to the merger.
type scheduleForge interface {
	OpenPullRequests(ctx context.Context) ([]forge.PRSummary, error)
}

// scheduleMerger reads a pull request's checks through the merger App (R16): the renovate-red
// probe is a merger-side read, so checks:read never joins the factory App's scope.
type scheduleMerger interface {
	PullRequestChecks(ctx context.Context, number int) (forge.Checks, error)
}

// Scheduler is a leader-only manager.Runnable that turns config's schedules into Tasks (§1):
// a slot fires once — the task's name is the slot's key, so AlreadyExists is the dedup, even
// across a failover — and only a slot of the last hour fires. Stopped reports the kill switch:
// intake pauses while it holds, schedules included.
type Scheduler struct {
	Forge     scheduleForge
	Merger    scheduleMerger
	Client    client.Client
	Namespace string
	Cfg       *config.Config
	Stopped   func(context.Context) bool
	Now       func() time.Time
	Errors    errorCounter
	Log       *slog.Logger
}

// NeedLeaderElection is true: two schedulers would race every slot.
func (s *Scheduler) NeedLeaderElection() bool { return true }

func (s *Scheduler) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

// Start ticks at once, then every minute, until ctx ends. A failed tick is counted and logged,
// and the next one retries.
func (s *Scheduler) Start(ctx context.Context) error {
	t := time.NewTicker(scheduleEvery)
	defer t.Stop()
	for {
		if err := s.Tick(ctx); err != nil {
			if s.Errors != nil {
				s.Errors.IntakeError(ctx, "schedule")
			}
			s.log().Warn("schedule tick failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick creates one Task per schedule whose last slot is within the hour; a probe that finds
// nothing starts nothing (§1).
func (s *Scheduler) Tick(ctx context.Context) error {
	if s.Stopped(ctx) {
		return nil
	}
	var errs []error
	for _, e := range s.Cfg.Schedules {
		now := s.Now().UTC()
		slot, ok := LastSlot(e.Cron, now)
		if !ok || now.Sub(slot) > time.Hour {
			continue
		}
		key := taskid.ScheduleKey(e.Name, slot)
		text := e.Text
		if e.Probe != "" {
			found, detail, err := s.probe(ctx, e.Probe)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", e.Name, err))
				continue
			}
			if !found {
				continue // "if it finds nothing, no task is created" (§1)
			}
			text += "\n\n" + detail
		}
		dataClass := e.DataClass
		if dataClass == "" {
			dataClass = s.Cfg.Defaults.DataClass
		}
		sum := sha256.Sum256([]byte(text))
		t := &v1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: taskid.Name(key), Namespace: s.Namespace},
			Spec: v1alpha1.TaskSpec{
				Source: v1alpha1.Source{Kind: "schedule", Ref: e.Name, Key: key, RequestedBy: "system:scheduler",
					Trust: "trusted", ContentSHA256: hex.EncodeToString(sum[:])},
				Repository: s.Cfg.Repository, Text: text, DataClass: dataClass, PredictedClass: e.Class,
			},
		}
		if err := s.Client.Create(ctx, t); err != nil && !apierrors.IsAlreadyExists(err) {
			errs = append(errs, err) // AlreadyExists is the dedup (S2)
		}
	}
	return errors.Join(errs...)
}

// probe: renovate-red lists open Renovate PRs whose checks have been red for more than 24 h —
// bumps the factory can land itself when Renovate's own branch went stale.
func (s *Scheduler) probe(ctx context.Context, name string) (bool, string, error) {
	if name != "renovate-red" {
		return false, "", fmt.Errorf("intake: unknown probe %q", name)
	}
	prs, err := s.Forge.OpenPullRequests(ctx)
	if err != nil {
		return false, "", err
	}
	var red []string
	for _, p := range prs {
		if p.Author != "renovate[bot]" || s.Now().Sub(p.Created) < 24*time.Hour {
			continue
		}
		c, err := s.Merger.PullRequestChecks(ctx, p.Number)
		if err != nil {
			return false, "", err
		}
		for _, x := range c.Runs {
			if x.State == "FAILURE" {
				red = append(red, fmt.Sprintf("#%d (%s)", p.Number, x.Name))
				break
			}
		}
		if len(red) == 5 {
			break
		}
	}
	if len(red) == 0 {
		return false, "", nil
	}
	return true, "Red Renovate PRs: " + strings.Join(red, ", ") +
		". Open one agent PR with the bump and the fix; never push to renovate/**.", nil
}
