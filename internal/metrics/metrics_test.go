// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape: %d", rec.Code)
	}
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

// The alerts and the plan's evidence query these names byte for byte: the
// exporter must add no unit or counter suffix, and drop no _total.
func TestExposedNamesAreTheOnesTheAlertsQuery(t *testing.T) {
	ctx := t.Context()
	exp, err := NewExporter("v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = exp.Shutdown(ctx) }()
	now := time.Unix(1_700_000_000, 0)
	s, err := New(exp.Meter(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	s.Appended(ctx, "message", "harness", []string{"jwt"})
	s.AppendTook(ctx, 3*time.Millisecond)
	s.AppendFailed(ctx)
	s.ObserveRoom("3kq7x2ma", "Active", 2, now.Add(-time.Minute))
	s.ObserveRoom("4kq7x2ma", "Closed", 0, now)
	s.Participants.Add(ctx, 1)
	s.Connections.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", "human")))
	s.FanoutLag.Record(ctx, 0.01)
	s.ApprovalsOldest.Record(ctx, 12)
	s.DecisionSeconds.Record(ctx, 5)
	s.DriverChanges.Add(ctx, 1)
	s.Rejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "not_permitted")))
	s.Dropped.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "slow")))
	if err := s.WatchJWKS(map[string]func() time.Time{"https://issuer": func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	s.BridgeStalled(ctx, "cursor_lost")
	s.BridgeStubbed(ctx, "refused")

	body := scrape(t, exp.Handler())
	for _, want := range []string{
		`rooms_build_info{version="v1.2.3"} 1`,
		`rooms{phase="Active"} 1`,
		`rooms{phase="Closed"} 1`,
		`rooms_participants 1`,
		`rooms_connections{kind="human"} 1`,
		`rooms_events_appended_total{origin="harness",type="message"} 1`,
		`rooms_append_seconds_bucket{le="0.005"} 1`,
		`rooms_append_errors_total 1`,
		`rooms_fanout_lag_seconds_bucket{le="0.01"} 1`,
		`rooms_approvals_pending 2`,
		`rooms_approvals_oldest_pending_seconds 12`,
		`rooms_approval_decision_seconds_count 1`,
		`rooms_driver_changes_total 1`,
		`rooms_redactions_total{rule="jwt"} 1`,
		`rooms_rejected_actions_total{reason="not_permitted"} 1`,
		`rooms_connections_dropped_total{reason="slow"} 1`,
		`rooms_last_event_timestamp_seconds{room="3kq7x2ma"} 1.69999994e+09`,
		`rooms_authn_jwks_last_refresh_timestamp_seconds{issuer="https://issuer"} 1.7e+09`,
		`rooms_bridge_harness_stalls_total{reason="cursor_lost"} 1`,
		`rooms_bridge_items_stubbed_total{reason="refused"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	for _, unwanted := range []string{`room="4kq7x2ma"`, "otel_scope", "target_info", "_total_total", "_seconds_seconds"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("exposes %s", unwanted)
		}
	}
	if t.Failed() {
		t.Log(body)
	}
}

func TestRoomGauges(t *testing.T) {
	ctx := t.Context()
	now := time.Unix(1_700_000_000, 0)
	for _, c := range []struct {
		name    string
		observe func(s *Set)
		advance time.Duration
		want    []string
		absent  []string
	}{
		{"a room counts once, in its latest phase", func(s *Set) {
			s.ObserveRoom("3kq7x2ma", "Open", 0, now)
			s.ObserveRoom("3kq7x2ma", "Active", 0, now)
		}, 0, []string{`rooms{phase="Active"} 1`}, []string{`rooms{phase="Open"}`}},
		{"only an Active room has a last-event series", func(s *Set) {
			s.ObserveRoom("3kq7x2ma", "Active", 0, now)
			s.ObserveRoom("3kq7x2ma", "Idle", 0, now)
		}, 0, []string{`rooms{phase="Idle"} 1`}, []string{"rooms_last_event_timestamp_seconds{"}},
		{"pending approvals sum over rooms", func(s *Set) {
			s.ObserveRoom("3kq7x2ma", "AwaitingHuman", 2, now)
			s.ObserveRoom("4kq7x2ma", "AwaitingHuman", 3, now)
		}, 0, []string{`rooms_approvals_pending 5`, `rooms{phase="AwaitingHuman"} 2`}, nil},
		{"a room no longer reconciled (deleted, or another leader) drops out", func(s *Set) {
			s.ObserveRoom("3kq7x2ma", "Active", 1, now)
		}, roomTTL + time.Second, []string{`rooms_approvals_pending 0`}, []string{`rooms{`, `room="3kq7x2ma"`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			exp, err := NewExporter("test")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = exp.Shutdown(ctx) }()
			at := now
			s, err := New(exp.Meter(), func() time.Time { return at })
			if err != nil {
				t.Fatal(err)
			}
			c.observe(s)
			at = now.Add(c.advance)
			body := scrape(t, exp.Handler())
			for _, w := range c.want {
				if !strings.Contains(body, w) {
					t.Errorf("missing %s", w)
				}
			}
			for _, a := range c.absent {
				if strings.Contains(body, a) {
					t.Errorf("exposes %s", a)
				}
			}
			if t.Failed() {
				t.Log(body)
			}
		})
	}
}

func TestNoopMeterWorks(t *testing.T) {
	s, err := New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Appended(t.Context(), "message", "broker", nil)
	s.AppendTook(t.Context(), time.Millisecond)
	s.AppendFailed(t.Context())
	s.ObserveRoom("3kq7x2ma", "Active", 0, time.Now())
}
