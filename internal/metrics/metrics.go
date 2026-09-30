// SPDX-License-Identifier: Apache-2.0

// Package metrics is the SP2 §9 metric set, plus the gauges the alerts need,
// on the OpenTelemetry metric API. NewExporter serves a MeterProvider in the
// Prometheus text format; New makes the broker's instruments on any Meter, a
// no-op one in tests.
//
// Names are exposed exactly as written here: the exporter adds no unit or
// counter suffix, so counters carry their own _total and the VMRules query
// these strings byte for byte.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// scope names the instrumentation scope; WithoutScopeInfo keeps it off the series.
const scope = "github.com/Smana/agent-platform"

// Bucket bounds sit on the thresholds that matter: SC-12's 0.5 s fan-out p95,
// and the approval timeouts (15 min alert, 30 min attended, 4 h unattended).
var (
	appendBuckets   = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}
	fanoutBuckets   = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}
	decisionBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 900, 1800, 3600, 14400}
)

// Exporter is a MeterProvider whose metrics Handler serves in the Prometheus
// text format. It exposes rooms_build_info{version} = 1 from the start.
type Exporter struct {
	provider *sdkmetric.MeterProvider
	handler  http.Handler
}

// NewExporter builds a provider on its own registry: no global state, so two
// in one process (tests) never collide.
func NewExporter(version string) (*Exporter, error) {
	reg := prometheus.NewRegistry()
	reader, err := otelprom.New(otelprom.WithRegisterer(reg),
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithoutSuffixes),
		otelprom.WithoutScopeInfo(), otelprom.WithoutTargetInfo())
	if err != nil {
		return nil, fmt.Errorf("metrics: prometheus exporter: %w", err)
	}
	e := &Exporter{provider: sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)),
		handler: promhttp.HandlerFor(reg, promhttp.HandlerOpts{})}
	if _, err := e.Meter().Int64ObservableGauge("rooms_build_info",
		metric.WithDescription("The running build: always 1, labelled with its version."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(1, metric.WithAttributes(attribute.String("version", version)))
			return nil
		})); err != nil {
		return nil, fmt.Errorf("metrics: rooms_build_info: %w", err)
	}
	return e, nil
}

// Meter makes instruments exposed by Handler.
func (e *Exporter) Meter() metric.Meter { return e.provider.Meter(scope) }

// Handler serves GET /metrics.
func (e *Exporter) Handler() http.Handler { return e.handler }

// Shutdown releases the provider.
func (e *Exporter) Shutdown(ctx context.Context) error { return e.provider.Shutdown(ctx) }

// Set is the broker's instruments. The exported ones have no phase-1 caller;
// the phase that feeds each is named beside it. Label values stay bounded:
// never a payload field, and room only on the gauge of rooms with a Running run.
type Set struct {
	// Participants is rooms_participants: live participants (phase 2).
	Participants metric.Int64UpDownCounter
	// Connections is rooms_connections{kind}: open viewer connections (phase 2).
	Connections metric.Int64UpDownCounter
	// FanoutLag is rooms_fanout_lag_seconds: append to delivery on a viewer connection (phase 2).
	FanoutLag metric.Float64Histogram
	// ApprovalsOldest is rooms_approvals_oldest_pending_seconds (phase 5).
	ApprovalsOldest metric.Float64Gauge
	// DecisionSeconds is rooms_approval_decision_seconds: request to decision (phase 5).
	DecisionSeconds metric.Float64Histogram
	// DriverChanges is rooms_driver_changes_total (phase 2).
	DriverChanges metric.Int64Counter
	// Rejected is rooms_rejected_actions_total{reason} (phase 2).
	Rejected metric.Int64Counter
	// Dropped is rooms_connections_dropped_total{reason} (phase 2).
	Dropped metric.Int64Counter
	// VerdictPosts is rooms_verdict_posts_total{result}: posted, not_posted or error (phase 3).
	VerdictPosts metric.Int64Counter

	meter         metric.Meter
	appended      metric.Int64Counter
	appendSeconds metric.Float64Histogram
	appendErrors  metric.Int64Counter
	redactions    metric.Int64Counter
	bridgeStalls  metric.Int64Counter
	bridgeStubs   metric.Int64Counter

	mu    sync.Mutex
	rooms map[string]roomGauge
}

type roomGauge struct {
	phase   string
	running bool
	pending int
	last    time.Time
}

// New makes the instruments on meter; nil means a no-op meter.
func New(meter metric.Meter) (*Set, error) {
	if meter == nil {
		meter = noop.NewMeterProvider().Meter(scope)
	}
	s := &Set{meter: meter, rooms: map[string]roomGauge{}}
	var errs []error
	check := func(err error) { errs = append(errs, err) }
	var err error
	s.Participants, err = meter.Int64UpDownCounter("rooms_participants", metric.WithDescription("Live participants."))
	check(err)
	s.Connections, err = meter.Int64UpDownCounter("rooms_connections", metric.WithDescription("Open connections, by kind."))
	check(err)
	s.appended, err = meter.Int64Counter("rooms_events_appended_total", metric.WithDescription("Durable events appended, by type and origin."))
	check(err)
	s.appendSeconds, err = meter.Float64Histogram("rooms_append_seconds", metric.WithDescription("Append latency, failures included."),
		metric.WithExplicitBucketBoundaries(appendBuckets...))
	check(err)
	s.appendErrors, err = meter.Int64Counter("rooms_append_errors_total", metric.WithDescription("Appends that failed on the database."))
	check(err)
	s.FanoutLag, err = meter.Float64Histogram("rooms_fanout_lag_seconds", metric.WithDescription("Append to delivery on a viewer connection."),
		metric.WithExplicitBucketBoundaries(fanoutBuckets...))
	check(err)
	s.ApprovalsOldest, err = meter.Float64Gauge("rooms_approvals_oldest_pending_seconds", metric.WithDescription("Age of the oldest undecided approval."))
	check(err)
	s.DecisionSeconds, err = meter.Float64Histogram("rooms_approval_decision_seconds", metric.WithDescription("Approval request to decision."),
		metric.WithExplicitBucketBoundaries(decisionBuckets...))
	check(err)
	s.DriverChanges, err = meter.Int64Counter("rooms_driver_changes_total", metric.WithDescription("Driver token changes."))
	check(err)
	s.redactions, err = meter.Int64Counter("rooms_redactions_total", metric.WithDescription("Secrets redacted, by rule."))
	check(err)
	s.Rejected, err = meter.Int64Counter("rooms_rejected_actions_total", metric.WithDescription("Actions refused, by reason."))
	check(err)
	s.Dropped, err = meter.Int64Counter("rooms_connections_dropped_total", metric.WithDescription("Connections the broker closed, by reason."))
	check(err)
	s.VerdictPosts, err = meter.Int64Counter("rooms_verdict_posts_total", metric.WithDescription("Verdict comments, by result."))
	check(err)
	// Ruling AP: counted by the broker from the events bridges append, since
	// nothing dials into a sandbox to scrape a bridge.
	s.bridgeStalls, err = meter.Int64Counter("rooms_bridge_harness_stalls_total",
		metric.WithDescription("Stalls on a harness log that bridges told their rooms, by reason; each bridge's cursor held."))
	check(err)
	s.bridgeStubs, err = meter.Int64Counter("rooms_bridge_items_stubbed_total",
		metric.WithDescription("Harness items kept as a stub in their slot, by reason."))
	check(err)
	check(s.registerRoomGauges())
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	return s, nil
}

// registerRoomGauges exposes rooms{phase}, rooms_approvals_pending and
// rooms_last_event_timestamp_seconds{room} from what ObserveRoom last recorded.
func (s *Set) registerRoomGauges() error {
	phases, err := s.meter.Int64ObservableGauge("rooms", metric.WithDescription("Rooms, by phase."))
	if err != nil {
		return err
	}
	pending, err := s.meter.Int64ObservableGauge("rooms_approvals_pending", metric.WithDescription("Undecided approvals."))
	if err != nil {
		return err
	}
	last, err := s.meter.Float64ObservableGauge("rooms_last_event_timestamp_seconds",
		metric.WithDescription("Last durable event of each room with a Running run, in Unix seconds."))
	if err != nil {
		return err
	}
	_, err = s.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		byPhase, total := map[string]int64{}, int64(0)
		for id, r := range s.rooms {
			byPhase[r.phase]++
			total += int64(r.pending)
			if r.running {
				o.ObserveFloat64(last, float64(r.last.UnixMilli())/1e3, metric.WithAttributes(attribute.String("room", id)))
			}
		}
		for p, n := range byPhase {
			o.ObserveInt64(phases, n, metric.WithAttributes(attribute.String("phase", p)))
		}
		o.ObserveInt64(pending, total)
		return nil
	}, phases, pending, last)
	return err
}

// ObserveRoom records a reconciled room: its phase, whether a run of it is
// Running, its undecided approvals and its last durable event. The last event is
// exposed for a room with a Running run whatever its phase: 30 silent minutes
// turn such a room AwaitingHuman, which is exactly when RoomStalled reads it
// (S1 review I-3). The values hold until the next successful reconcile,
// so a database outage keeps them rather than emptying the gauges (review M1).
// Only the leader reconciles, and a replica that loses the lease exits, so a
// follower never serves stale values.
func (s *Set) ObserveRoom(room, phase string, running bool, pendingApprovals int, lastEventAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rooms[room] = roomGauge{phase: phase, running: running, pending: pendingApprovals, last: lastEventAt}
}

// ForgetRoom takes a room whose CR is gone out of the gauges.
func (s *Set) ForgetRoom(room string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rooms, room)
}

// AppendTook records one append's latency, whatever its outcome.
func (s *Set) AppendTook(ctx context.Context, d time.Duration) {
	s.appendSeconds.Record(ctx, d.Seconds())
}

// Appended counts a new durable event and the redaction rules it matched.
func (s *Set) Appended(ctx context.Context, typ, origin string, rules []string) {
	s.appended.Add(ctx, 1, metric.WithAttributes(attribute.String("type", typ), attribute.String("origin", origin)))
	for _, r := range rules {
		s.redactions.Add(ctx, 1, metric.WithAttributes(attribute.String("rule", r)))
	}
}

// AppendFailed counts an append the database failed.
func (s *Set) AppendFailed(ctx context.Context) { s.appendErrors.Add(ctx, 1) }

// BridgeStalled counts a stall a bridge told its room; reason is one of a fixed set.
func (s *Set) BridgeStalled(ctx context.Context, reason string) {
	s.bridgeStalls.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// BridgeStubbed counts a harness item stored as a stub; reason is one of a fixed set.
func (s *Set) BridgeStubbed(ctx context.Context, reason string) {
	s.bridgeStubs.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// WatchFanout exposes rooms_fanout_listener_up from up (fanout.Hub.Healthy): 1
// while the hub LISTENs. It never gates readiness: while it is 0 the hub polls
// every second, so viewers are still served (review M3).
func (s *Set) WatchFanout(up func() bool) error {
	_, err := s.meter.Int64ObservableGauge("rooms_fanout_listener_up",
		metric.WithDescription("1 while the fan-out hub LISTENs; 0 while it polls every second instead."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			var v int64
			if up() {
				v = 1
			}
			o.Observe(v)
			return nil
		}))
	if err != nil {
		return fmt.Errorf("metrics: rooms_fanout_listener_up: %w", err)
	}
	return nil
}

// WatchJWKS exposes rooms_authn_jwks_last_refresh_timestamp_seconds{issuer}: when
// each issuer's keys were last fetched. Held keys stop verifying 24 h after it
// (Ruling AF), so an alert well before that catches an unreachable issuer. An
// issuer never fetched has no series.
func (s *Set) WatchJWKS(lastRefresh map[string]func() time.Time) error {
	_, err := s.meter.Float64ObservableGauge("rooms_authn_jwks_last_refresh_timestamp_seconds",
		metric.WithDescription("When each token issuer's JWKS was last fetched, in Unix seconds."),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			for issuer, at := range lastRefresh {
				if t := at(); !t.IsZero() {
					o.Observe(float64(t.UnixMilli())/1e3, metric.WithAttributes(attribute.String("issuer", issuer)))
				}
			}
			return nil
		}))
	if err != nil {
		return fmt.Errorf("metrics: rooms_authn_jwks_last_refresh_timestamp_seconds: %w", err)
	}
	return nil
}
