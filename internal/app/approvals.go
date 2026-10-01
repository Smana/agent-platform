// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/wire"
)

const (
	// approvalsEvery paces the approval sweep: an expiry lands within 30 s of its deadline.
	approvalsEvery = 30 * time.Second
	// policyTimeout bounds a Room read for its approval policy, from the cache.
	policyTimeout = 5 * time.Second
)

// approvalLog is what the approval sweep closes and measures.
type approvalLog interface {
	ExpireDue(ctx context.Context) ([]envelope.Event, error)
	SupersedeAnswered(ctx context.Context) ([]envelope.Event, error)
	OldestPending(ctx context.Context) (time.Duration, int, error)
}

// approvalLoop closes, on the leader every approvalsEvery, the approvals whose
// call has a result or whose deadline passed (§6, 5.2 contracts 3 and 5), and
// sets oldest to the oldest pending one's age in seconds
// (rooms_approvals_oldest_pending_seconds). A replica that stops leading sets it
// to 0. tick paces it; nil means a time.Ticker.
func approvalLoop(l approvalLog, oldest func(context.Context, float64), log *slog.Logger,
	tick func(time.Duration) (<-chan time.Time, func()),
) *leaderLoop {
	var leading atomic.Bool
	return &leaderLoop{every: approvalsEvery, active: &leading, ticker: tick,
		run:  func(ctx context.Context) { sweepApprovals(ctx, l, leading.Load, oldest, log) },
		done: func() { oldest(context.Background(), 0) }}
}

// sweepApprovals runs one sweep while this replica leads (ruling SY). Superseded
// first: a call that has its result is answered, whatever its deadline. Every
// close is keyed on its approval, so a sweep a new leader repeats writes nothing
// twice.
func sweepApprovals(ctx context.Context, l approvalLog, leading func() bool, oldest func(context.Context, float64), log *slog.Logger) {
	if _, err := l.SupersedeAnswered(ctx); err != nil {
		log.Warn("supersede answered approvals", "err", err)
	}
	if !leading() {
		return
	}
	if _, err := l.ExpireDue(ctx); err != nil {
		log.Warn("expire approvals", "err", err)
	}
	if !leading() {
		return
	}
	age, _, err := l.OldestPending(ctx)
	if err != nil {
		log.Warn("oldest pending approval", "err", err)
		return
	}
	oldest(ctx, age.Seconds())
}

// roomPolicy is a room's approval policy from its Room, for the bridge's hello
// and each approval's deadline. An unreadable Room is attended: the shorter wait.
func roomPolicy(rooms client.Reader, ns string) func(string) wire.ApprovalPolicy {
	return func(room string) wire.ApprovalPolicy {
		ctx, cancel := context.WithTimeout(context.Background(), policyTimeout)
		defer cancel()
		var r v1alpha1.Room
		if err := rooms.Get(ctx, client.ObjectKey{Namespace: ns, Name: room}, &r); err != nil {
			return wire.ApprovalPolicy{Profile: "attended"}
		}
		a := r.Spec.Approvals
		return wire.ApprovalPolicy{Profile: a.Profile, Overrides: a.Overrides, TTL: a.TTL}
	}
}
