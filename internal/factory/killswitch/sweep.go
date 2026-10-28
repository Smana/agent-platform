// SPDX-License-Identifier: Apache-2.0

package killswitch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Smana/maku/internal/factory/runs"
)

// RunStore is the part of the AgentRun client the sweep uses; runs.Client implements it.
type RunStore interface {
	List(ctx context.Context) ([]runs.Run, error)
	Annotate(ctx context.Context, id string, kv map[string]string) error
	Delete(ctx context.Context, id string) error
}

// Sweeper is the stop's reach over every run (owner, 2026-09-27; R35). The reconciler stops
// tasks; this revokes and deletes every live AgentRun in agents, human-requested ones included,
// since after phase 5 the factory created them all. Only on a definite yes: a transient API error
// never revokes a run.
type Sweeper struct {
	Reader    client.Reader
	Namespace string
	Runs      RunStore
	// Release drops a deleted run's reservation from the day's ledger (#55): a claim deleted
	// while live never reaches the terminal phase the meter releases on. nil: the meter's
	// unclaimed sweep releases it after its grace.
	Release func(ctx context.Context, r runs.Run) error
	Every   time.Duration
	// OnSweep is told of the runs a sweep revoked, each to count under its payer.
	OnSweep func(revoked []runs.Run)
	Log     *slog.Logger
}

// NeedLeaderElection is true: only the leader sweeps; two would race the same deletes.
func (s *Sweeper) NeedLeaderElection() bool { return true }

// Start sweeps at once, then every Every, until ctx ends. A failed sweep is logged and the
// next one retries.
func (s *Sweeper) Start(ctx context.Context) error {
	t := time.NewTicker(s.Every)
	defer t.Stop()
	for {
		if revoked, err := s.Sweep(ctx); err != nil {
			s.Log.Warn("stop sweep failed", "err", err)
		} else if len(revoked) > 0 && s.OnSweep != nil {
			s.OnSweep(revoked)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Sweep revokes and deletes every non-terminal run while the stop is engaged: the annotation
// first, so the composition sees the revoke even if the delete then fails. A run already gone
// mid-sweep is done, not an error. It returns the runs it revoked: a run that already
// carried a revoke, such as one the task reconciler's stop revoked seconds earlier, was counted by
// whoever wrote it (F30).
func (s *Sweeper) Sweep(ctx context.Context) ([]runs.Run, error) {
	on, err := Engaged(ctx, s.Reader, s.Namespace)
	if err != nil || !on {
		return nil, err
	}
	all, err := s.Runs.List(ctx)
	if err != nil {
		return nil, err
	}
	var revoked []runs.Run
	var errs []error
	for _, r := range all {
		if runs.Terminal(r.Phase) {
			continue
		}
		err := s.Runs.Annotate(ctx, r.ID, map[string]string{runs.AnnRevoked: "manual"})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
			continue
		}
		if err == nil && r.Revoked == "" {
			revoked = append(revoked, r)
		}
		if err := s.Runs.Delete(ctx, r.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		if s.Release != nil {
			errs = append(errs, s.Release(ctx, r))
		}
	}
	return revoked, errors.Join(errs...)
}
