// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/fmetrics"
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

// ScheduleForge is the part of the forge the probe reads as the factory App: the open pull
// requests, whose checks it then hands to the merger.
type ScheduleForge interface {
	OpenPullRequests(ctx context.Context) ([]forge.PRSummary, error)
}

// ScheduleMerger reads a pull request's checks through the merger App (R16): the renovate-red
// probe is a merger-side read, so checks:read never joins the factory App's scope.
type ScheduleMerger interface {
	PullRequestChecks(ctx context.Context, number int) (forge.Checks, error)
	DeadLinksMerger
}

// Scheduler is a leader-only manager.Runnable that turns config's schedules into Tasks (§1):
// a slot fires once — the task's name is the slot's key, so AlreadyExists is the dedup, even
// across a failover — and only a slot of the last hour fires. Stopped reports the kill switch:
// intake pauses while it holds, schedules included.
type Scheduler struct {
	Forge  ScheduleForge
	Merger ScheduleMerger
	// Clients is v2 D8: a repository's factory App and merger connections. Set, each entry's
	// probe reads its own repository, and one with no connection probes nothing; nil, Forge and
	// Merger serve every entry (one repository).
	Clients   func(repository string) (ScheduleForge, ScheduleMerger, bool)
	Client    client.Client
	Namespace string
	Cfg       *config.Config
	// Owned is v2 D7's fence: an entry not owned by this factory fires nothing; nil owns nothing.
	Owned   func(repository string) bool
	Stopped func(context.Context) bool
	Now     func() time.Time
	Errors  errorCounter
	Log     *slog.Logger

	seenMu sync.Mutex
	seen   map[string][2]int64 // schedule -> the detector run pair last evaluated, outcome final
	// pending is a pair whose findings await their Task; settle saves it once the Task exists.
	pending map[string][2]int64
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
	for _, rc := range s.Cfg.Repos() {
		if s.Owned == nil || !s.Owned(rc.Name) {
			continue // another factory's, or nobody's (v2 D7)
		}
		errs = append(errs, s.tick(ctx, &rc)...)
	}
	return errors.Join(errs...)
}

// tick is one repository's schedules, each stamped with that repository (v2 D1).
func (s *Scheduler) tick(ctx context.Context, rc *config.Repository) []error {
	var errs []error
	for _, e := range rc.Schedules {
		now := s.Now().UTC()
		slot, ok := LastSlot(e.Cron, now)
		if !ok || now.Sub(slot) > time.Hour {
			continue
		}
		key := taskid.ScheduleKey(e.Name, slot)
		text := e.Text
		var findings []v1alpha1.Finding
		if e.Probe != "" {
			found, detail, fs, err := s.probe(ctx, rc, e)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", e.Name, err))
				continue
			}
			if !found {
				continue // "if it finds nothing, no task is created" (§1)
			}
			text, findings = text+"\n\n"+detail, fs
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
				Repository: rc.Name, Text: text, DataClass: dataClass, PredictedClass: e.Class, Findings: findings,
			},
		}
		if err := s.Client.Create(ctx, t); err != nil && !apierrors.IsAlreadyExists(err) {
			errs = append(errs, err) // AlreadyExists is the dedup (S2)
		} else if len(findings) > 0 {
			s.settle(rc.Name + "/" + e.Name)
		}
	}
	return errs
}

// remember saves a run pair as weighed.
func (s *Scheduler) remember(key string, pair [2]int64) {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	if s.seen == nil {
		s.seen = map[string][2]int64{}
	}
	s.seen[key] = pair
}

// settle saves the pair whose findings became a Task (created, or already there).
func (s *Scheduler) settle(key string) {
	s.seenMu.Lock()
	pair, ok := s.pending[key]
	delete(s.pending, key)
	s.seenMu.Unlock()
	if ok {
		s.remember(key, pair)
	}
}

// probe runs the schedule's probe: whether it found work, the text it adds, and the findings an
// oracle class's task carries.
func (s *Scheduler) probe(ctx context.Context, rc *config.Repository, e config.Schedule) (bool, string, []v1alpha1.Finding, error) {
	switch e.Probe {
	case "renovate-red":
		found, detail, err := s.renovateRed(ctx, rc, e.ProbeAuthor)
		return found, detail, nil, err
	case "dead-links":
		return s.deadLinksProbe(ctx, rc, e)
	}
	return false, "", nil, fmt.Errorf("intake: unknown probe %q", e.Probe)
}

// conns is the repository's two connections: Clients' (v2 D8), or Forge and Merger.
func (s *Scheduler) conns(rc *config.Repository) (ScheduleForge, ScheduleMerger, error) {
	if s.Clients == nil {
		return s.Forge, s.Merger, nil
	}
	gh, merger, ok := s.Clients(rc.Name)
	if !ok {
		return nil, nil, fmt.Errorf("intake: no GitHub connection for %s (v2 D8)", rc.Name)
	}
	return gh, merger, nil
}

// deadLinksProbe (MC5, MC6): nothing while the class is off (MC14) or while a task of this
// schedule is still open (loops L9); else the findings both newest detector runs agree on. A run
// the probe does not trust (errFindingsRefused) starts nothing: it is logged and counted, not a
// failed tick.
func (s *Scheduler) deadLinksProbe(ctx context.Context, rc *config.Repository, e config.Schedule) (bool, string, []v1alpha1.Finding, error) {
	cl := rc.Classes[e.Class]
	if (!cl.Live && !cl.Shadow) || cl.Oracle == nil {
		return false, "", nil, nil
	}
	var l v1alpha1.TaskList
	if err := s.Client.List(ctx, &l, client.InNamespace(s.Namespace)); err != nil {
		return false, "", nil, err
	}
	if slices.ContainsFunc(l.Items, func(t v1alpha1.Task) bool {
		return t.Spec.Source.Kind == "schedule" && t.Spec.Source.Ref == e.Name && !v1alpha1.TerminalPhase(t.Status.Phase)
	}) {
		return false, "", nil, nil
	}
	_, merger, err := s.conns(rc)
	if err != nil {
		return false, "", nil, err
	}
	two, err := detectorRuns(ctx, merger, rc, cl)
	if err != nil || two == nil {
		return false, "", nil, err
	}
	// The leader is the only evaluator: a run pair already weighed is not read again each minute.
	pair, key := [2]int64{two[0].ID, two[1].ID}, rc.Name+"/"+e.Name
	s.seenMu.Lock()
	same := s.seen[key] == pair
	s.seenMu.Unlock()
	if same {
		return false, "", nil, nil
	}
	fs, err := evaluateRuns(ctx, merger, two, cl, s.log().With("schedule", e.Name))
	// Final outcomes are saved now (refused, or nothing found); findings wait for their Task.
	switch {
	case err == nil && len(fs) > 0:
		s.seenMu.Lock()
		if s.pending == nil {
			s.pending = map[string][2]int64{}
		}
		s.pending[key] = pair
		s.seenMu.Unlock()
	case err == nil || errors.Is(err, errFindingsRefused):
		s.remember(key, pair)
	}
	if errors.Is(err, errFindingsRefused) {
		s.log().Warn("dead-links findings refused", "schedule", e.Name, "err", err)
		if s.Errors != nil {
			s.Errors.IntakeError(fmetrics.WithRepository(ctx, rc.Name), "dead-links")
		}
		return false, "", nil, nil
	}
	if err != nil || len(fs) == 0 {
		return false, "", nil, err
	}
	text, spec := deadLinksTask(fs)
	return true, text, spec, nil
}

// renovateRed lists open Renovate PRs whose checks have been red for more than 24 h — bumps the
// factory can land itself when Renovate's own branch went stale.
func (s *Scheduler) renovateRed(ctx context.Context, rc *config.Repository, author string) (bool, string, error) {
	if !rc.NeedsMerger() {
		return false, "", nil // no merger key is required, and reading checks needs it: nothing to find
	}
	gh, merger, err := s.conns(rc)
	if err != nil {
		return false, "", err
	}
	prs, err := gh.OpenPullRequests(ctx)
	if err != nil {
		return false, "", err
	}
	var red []string
	for _, p := range prs {
		if p.Author != author || s.Now().Sub(p.Created) < 24*time.Hour {
			continue
		}
		c, err := merger.PullRequestChecks(ctx, p.Number)
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
