// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Smana/agent-platform/internal/config"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/humanapi"
	"github.com/Smana/agent-platform/internal/metrics"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/runrequest"
	"github.com/Smana/agent-platform/internal/store"
)

const (
	// leaseEvery paces the driver lease sweep: a lapse is 2 or 15 minutes (§2).
	leaseEvery     = 30 * time.Second
	factoryTimeout = 15 * time.Second
)

// humanActor serves humans' acts on :8080 over the broker's parts: the same
// groups as the listener, the redactor every append goes through (§4), and
// rejections counted in rooms_rejected_actions_total{reason}.
func humanActor(h config.HumanConfig, roomLog humanapi.ActLog, red humanapi.Redactor, runs humanapi.Runs,
	rooms client.Client, requester runrequest.Requester, m *metrics.Set,
) *humanapi.Actor {
	return &humanapi.Actor{Log: roomLog, Groups: policy.Groups{Admin: h.Groups.Admin, Member: h.Groups.Member},
		Runs: runs, Redactor: red, Rooms: rooms, Requester: requester,
		OnReject: func(ctx context.Context, reason string) {
			m.Rejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
		}}
}

// runRequester is SP3's factory at factoryURL, or before SP3 (unset) the
// manifest renderer (ruling P14).
func runRequester(factoryURL string) runrequest.Requester {
	if factoryURL == "" {
		return runrequest.Manifest{}
	}
	return runrequest.Factory{URL: factoryURL, HC: httpx.New(factoryTimeout, nil)}
}

// leaseLog is what the lease sweep reads and moves.
type leaseLog interface {
	LapsedDrivers(ctx context.Context) ([]store.RoomState, error)
	ExpireDriver(ctx context.Context, roomID string, expect int64, to string, d envelope.Draft) (envelope.Event, error)
}

// leaseLoop hands a lapsed human driver's token back to the room's system holder
// (§2), on the leader every leaseEvery. tick paces it; nil means a time.Ticker.
func leaseLoop(l leaseLog, log *slog.Logger, tick func(time.Duration) (<-chan time.Time, func())) *leaderLoop {
	var leading atomic.Bool
	return &leaderLoop{every: leaseEvery, active: &leading, ticker: tick,
		run: func(ctx context.Context) { sweepLeases(ctx, l, leading.Load, log) }}
}

// sweepLeases expires every lapsed holder's lease while this replica leads
// (ruling SY). Each change is keyed on its epoch, so a sweep that a new leader
// repeats writes nothing twice, and the store re-checks the lapse under the
// room's row lock.
func sweepLeases(ctx context.Context, l leaseLog, leading func() bool, log *slog.Logger) {
	lapsed, err := l.LapsedDrivers(ctx)
	if err != nil {
		log.Error("driver lease sweep", "err", err)
		return
	}
	for _, r := range lapsed {
		if !leading() {
			return
		}
		_, err := l.ExpireDriver(ctx, r.ID, r.DriverEpoch, r.FallbackDriver, envelope.Draft{
			Actor:  envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"},
			Origin: envelope.OriginBroker, OriginClient: "broker:lease:" + r.ID, OriginSeq: r.DriverEpoch + 1})
		// Not lapsed any more, moved meanwhile or sealed: the sweep's read is stale.
		if err != nil && !errors.Is(err, store.ErrNotLapsed) && !errors.Is(err, store.ErrStaleEpoch) && !errors.Is(err, store.ErrSealed) {
			log.Warn("driver lease", "room", r.ID, "err", err)
		}
	}
}
