// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/bridgeapi"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/metrics"
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

// brokerAPILog is what :8443 and its approval sweep read and write; the metered
// store has all of it.
type brokerAPILog interface {
	bridgeapi.Log
	bridgeapi.Approvals
	approvalLog
}

// bridgeAPI is :8443 over the broker's parts, with phase 5's approvals: each
// room's policy read from its Room, requests recorded in the log, and the
// approval sweep added to the leader through add (review 5.3 M3).
func bridgeAPI(l brokerAPILog, red bridgeapi.Redactor, runs, systems bridgeapi.Authenticator, watch bridgeapi.Liveness,
	rooms client.Reader, ns string, add func(manager.Runnable) error, m *metrics.Set, log *slog.Logger,
) (*bridgeapi.Server, error) {
	oldest := func(ctx context.Context, secs float64) { m.ApprovalsOldest.Record(ctx, secs) }
	if err := add(approvalLoop(l, oldest, log, nil)); err != nil {
		return nil, fmt.Errorf("approval sweep: %w", err)
	}
	return &bridgeapi.Server{Log: l, Redactor: red, Runs: runs, Systems: systems, Watch: watch, Logger: log,
		RoomPolicy: roomPolicy(rooms, ns), Approvals: l}, nil
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
