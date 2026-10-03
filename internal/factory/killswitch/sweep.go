// SPDX-License-Identifier: Apache-2.0

package killswitch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Smana/agent-platform/internal/factory/runs"
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
	Every     time.Duration
	OnSweep   func(n int)
	Log       *slog.Logger
}

// NeedLeaderElection is true: only the leader sweeps; two would race the same deletes.
func (s *Sweeper) NeedLeaderElection() bool { return true }

// Start sweeps at once, then every Every, until ctx ends. A failed sweep is logged and the
// next one retries.
func (s *Sweeper) Start(ctx context.Context) error {
	t := time.NewTicker(s.Every)
	defer t.Stop()
	for {
		if n, err := s.Sweep(ctx); err != nil {
			s.Log.Warn("stop sweep failed", "err", err)
		} else if n > 0 && s.OnSweep != nil {
			s.OnSweep(n)
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
// mid-sweep is done, not an error.
func (s *Sweeper) Sweep(ctx context.Context) (int, error) {
	on, err := Engaged(ctx, s.Reader, s.Namespace)
	if err != nil || !on {
		return 0, err
	}
	all, err := s.Runs.List(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, r := range all {
		if runs.Terminal(r.Phase) {
			continue
		}
		if err := s.Runs.Annotate(ctx, r.ID, map[string]string{runs.AnnRevoked: "manual"}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
			continue
		}
		if err := s.Runs.Delete(ctx, r.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}
