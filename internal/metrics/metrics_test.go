// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
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
	exp, err := NewExporter(BrokerBuildInfo, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = exp.Shutdown(ctx) }()
	now := time.Unix(1_700_000_000, 0)
	s, err := New(exp.Meter())
	if err != nil {
		t.Fatal(err)
	}
	s.Appended(ctx, "message", "harness", []string{"jwt"})
	s.AppendTook(ctx, 3*time.Millisecond)
	s.AppendFailed(ctx)
	s.ObserveRoom("3kq7x2ma", "Active", true, 2, now.Add(-time.Minute))
	s.ObserveRoom("4kq7x2ma", "Closed", false, 0, now)
	s.Participants.Add(ctx, 1)
	s.Connections.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", "human")))
	s.FanoutLag.Record(ctx, 0.01)
	s.ApprovalsOldest.Record(ctx, 12)
	s.DecisionSeconds.Record(ctx, 5)
	s.DriverChanges.Add(ctx, 1)
	s.Rejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "not_permitted")))
	s.Dropped.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "slow")))
	s.VerdictPosts.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "posted")))
	s.MCPCalls.Add(ctx, 1, metric.WithAttributes(attribute.String("tool", "room_summary"), attribute.String("outcome", "ok")))
	if err := s.WatchJWKS(map[string]func() time.Time{"https://issuer": func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	s.WebhookDelivery(ctx, "slack-app", "delivered")
	s.WebhookDisabled(ctx, "slack-app", true)
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
		`rooms_verdict_posts_total{result="posted"} 1`,
		`rooms_human_mcp_calls_total{outcome="ok",tool="room_summary"} 1`,
		`rooms_last_event_timestamp_seconds{room="3kq7x2ma"} 1.69999994e+09`,
		`rooms_authn_jwks_last_refresh_timestamp_seconds{issuer="https://issuer"} 1.7e+09`,
		`rooms_bridge_harness_stalls_total{reason="cursor_lost"} 1`,
		`rooms_bridge_items_stubbed_total{reason="refused"} 1`,
		`rooms_webhook_deliveries_total{outcome="delivered",subscriber="slack-app"} 1`,
		`rooms_webhook_disabled{subscriber="slack-app"} 1`,
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

// The fan-out listener's state, read at each scrape: 1 while LISTEN is in place.
func TestFanoutListenerUp(t *testing.T) {
	exp, err := NewExporter(BrokerBuildInfo, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = exp.Shutdown(t.Context()) }()
	s, err := New(exp.Meter())
	if err != nil {
		t.Fatal(err)
	}
	var up atomic.Bool
	if err := s.WatchFanout(up.Load); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"rooms_fanout_listener_up 0\n", "rooms_fanout_listener_up 1\n"} {
		if body := scrape(t, exp.Handler()); !strings.Contains(body, want) {
			t.Fatalf("missing %q in\n%s", want, body)
		}
		up.Store(true)
	}
}

func TestRoomGauges(t *testing.T) {
	ctx := t.Context()
	now := time.Unix(1_700_000_000, 0)
	for _, c := range []struct {
		name    string
		observe func(s *Set)
		want    []string
		absent  []string
	}{
		{"a room counts once, in its latest phase", func(s *Set) {
			s.ObserveRoom("3kq7x2ma", "Open", false, 0, now)
			s.ObserveRoom("3kq7x2ma", "Active", true, 0, now)
		}, []string{`rooms{phase="Active"} 1`}, []string{`rooms{phase="Open"}`}},
		{"a room without a Running run has no activity series", func(s *Set) {
			s.ObserveRoom("3kq7x2ma", "Active", true, 0, now)
			s.ObserveRoom("3kq7x2ma", "Idle", false, 0, now)
		}, []string{`rooms{phase="Idle"} 1`}, []string{"rooms_last_event_timestamp_seconds{"}},
		// S1 review I-3: 30 silent minutes flip a running room to AwaitingHuman,
		// which is exactly when RoomStalled must still see its series.
		{"a stalled room keeps its activity series through the flip to AwaitingHuman", func(s *Set) {
			s.ObserveRoom("3kq7x2ma", "Active", true, 0, now.Add(-31*time.Minute))
			s.ObserveRoom("3kq7x2ma", "AwaitingHuman", true, 0, now.Add(-31*time.Minute))
		}, []string{`rooms{phase="AwaitingHuman"} 1`, `rooms_last_event_timestamp_seconds{room="3kq7x2ma"} 1.69999814e+09`}, nil},
		{"pending approvals sum over rooms", func(s *Set) {
			s.ObserveRoom("3kq7x2ma", "AwaitingHuman", false, 2, now)
			s.ObserveRoom("4kq7x2ma", "AwaitingHuman", false, 3, now)
		}, []string{`rooms_approvals_pending 5`, `rooms{phase="AwaitingHuman"} 2`}, []string{"rooms_last_event_timestamp_seconds{"}},
		{"a room keeps its last values while it is not reconciled (a database outage)", func(s *Set) {
			s.ObserveRoom("3kq7x2ma", "Active", true, 1, now)
		}, []string{`rooms{phase="Active"} 1`, `rooms_last_event_timestamp_seconds{room="3kq7x2ma"}`, `rooms_approvals_pending 1`}, nil},
		{"a forgotten room (its CR is gone) drops out", func(s *Set) {
			s.ObserveRoom("3kq7x2ma", "Active", true, 1, now)
			s.ObserveRoom("4kq7x2ma", "Idle", false, 0, now)
			s.ForgetRoom("3kq7x2ma")
		}, []string{`rooms_approvals_pending 0`, `rooms{phase="Idle"} 1`}, []string{`rooms{phase="Active"}`, `room="3kq7x2ma"`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			exp, err := NewExporter(BrokerBuildInfo, "test")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = exp.Shutdown(ctx) }()
			s, err := New(exp.Meter())
			if err != nil {
				t.Fatal(err)
			}
			c.observe(s)
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

func TestEachBinaryHasItsOwnBuildInfo(t *testing.T) {
	exp, err := NewExporter(FactoryBuildInfo, "v0.7.0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = exp.Shutdown(t.Context()) }()
	if body := scrape(t, exp.Handler()); !strings.Contains(body, "agent_factory_build_info{version=\"v0.7.0\"} 1\n") ||
		strings.Contains(body, "rooms_build_info") {
		t.Fatal(body)
	}
	if _, err := NewExporter("build_info", "v0.7.0"); err == nil {
		t.Fatal("a gauge outside both prefixes is refused")
	}
}

func TestNoopMeterWorks(t *testing.T) {
	s, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Appended(t.Context(), "message", "broker", nil)
	s.AppendTook(t.Context(), time.Millisecond)
	s.AppendFailed(t.Context())
	s.ObserveRoom("3kq7x2ma", "Active", true, 0, time.Now())
	s.ForgetRoom("3kq7x2ma")
}

// rooms_run_info{run, model, effort} = 1 for each live run whose harness reported an effort. The
// model is the claim's spec.model, read from the watch at collection, never the harness's; the
// effort is the harness's, an enum. A per-live-run series, its map capped.
func TestRunInfo(t *testing.T) {
	ctx := t.Context()
	series := func(run, model, effort string) string {
		return `rooms_run_info{effort="` + effort + `",model="` + model + `",run="` + run + `"} 1`
	}
	for _, c := range []struct {
		name    string
		runs    map[string]watchedRun // what the watch says; absent means live with no model
		observe func(s *Set)
		want    []string
		absent  []string
		count   int // series of rooms_run_info, when set
	}{
		{"the claim's model and the harness's effort", map[string]watchedRun{"aaaaaaaa": {"agent-default", true}}, func(s *Set) {
			s.RunInfo(ctx, "aaaaaaaa", "high")
		}, []string{series("aaaaaaaa", "agent-default", "high")}, nil, 1},
		{"the latest effort wins", map[string]watchedRun{"aaaaaaaa": {"agent-default", true}}, func(s *Set) {
			s.RunInfo(ctx, "aaaaaaaa", "high")
			s.RunInfo(ctx, "aaaaaaaa", "low")
		}, []string{series("aaaaaaaa", "agent-default", "low")}, []string{`effort="high"`}, 1},
		{"a claim without a model", nil, func(s *Set) {
			s.RunInfo(ctx, "aaaaaaaa", "max")
		}, []string{series("aaaaaaaa", "", "max")}, nil, 1},
		{"a run that is no longer live drops out", map[string]watchedRun{"aaaaaaaa": {"agent-default", false}}, func(s *Set) {
			s.RunInfo(ctx, "aaaaaaaa", "high")
			s.RunInfo(ctx, "bbbbbbbb", "low")
		}, []string{series("bbbbbbbb", "", "low")}, []string{`run="aaaaaaaa"`}, 1},
		{"at the cap, dead runs make room", deadRuns(maxRunInfo), func(s *Set) {
			for i := range maxRunInfo {
				s.RunInfo(ctx, fmt.Sprintf("r%07d", i), "high")
			}
			s.RunInfo(ctx, "newrun00", "low") // every held run has ended since: pruned, not dropped
		}, []string{series("newrun00", "", "low")}, []string{"rooms_run_info_dropped_total "}, 1},
		{"at the cap with every run live, a new one is dropped and counted", nil, func(s *Set) {
			for i := range maxRunInfo + 5 {
				s.RunInfo(ctx, fmt.Sprintf("r%07d", i), "high")
			}
			s.RunInfo(ctx, "r0000000", "low") // a known run still updates
		}, []string{series("r0000000", "", "low"), "rooms_run_info_dropped_total 5"},
			[]string{`run="r` + fmt.Sprintf("%07d", maxRunInfo) + `"`}, maxRunInfo},
	} {
		t.Run(c.name, func(t *testing.T) {
			exp, err := NewExporter(BrokerBuildInfo, "test")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = exp.Shutdown(ctx) }()
			s, err := New(exp.Meter())
			if err != nil {
				t.Fatal(err)
			}
			s.TrackRuns(func(run string) (string, bool) {
				w, ok := c.runs[run]
				if !ok {
					return "", true
				}
				return w.model, w.live
			})
			c.observe(s)
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
			if n := strings.Count(body, "\nrooms_run_info{"); c.count > 0 && n != c.count {
				t.Errorf("%d rooms_run_info series, want %d", n, c.count)
			}
			if t.Failed() && len(body) < 4096 {
				t.Log(body)
			}
		})
	}
}

// watchedRun is what a test's watch says of a run.
type watchedRun struct {
	model string
	live  bool
}

// deadRuns is a watch in which the first n runs of TestRunInfo's naming have ended.
func deadRuns(n int) map[string]watchedRun {
	out := make(map[string]watchedRun, n)
	for i := range n {
		out[fmt.Sprintf("r%07d", i)] = watchedRun{}
	}
	return out
}

// series maps each sample line of a scrape, "name{labels}", to its value.
func series(body string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(body, "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		i := strings.LastIndexByte(l, ' ')
		out[l[:i]] = l[i+1:]
	}
	return out
}

func TestMakuName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"rooms", "maku_rooms"},
		{"rooms_events_appended_total", "maku_rooms_events_appended_total"},
		{"rooms_build_info", "maku_rooms_build_info"},
		{"agent_factory_tasks", "maku_factory_tasks"},
		{"agent_factory_build_info", "maku_factory_build_info"},
		{"factory_slack_build_info", ""},
		{"roomsx", ""},
		{"go_goroutines", ""},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := MakuName(tc.in); got != tc.want {
				t.Fatalf("MakuName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// v0.30.0 serves every rooms series twice, the second under maku_ (the rebrand): dashboards switch
// names while both carry the same values. Off, only the old names.
func TestMakuNamesMirrorEveryRoomsSeries(t *testing.T) {
	for _, tc := range []struct {
		name string
		on   bool
	}{{"on serves both names with equal values", true}, {"off serves the old names only", false}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			exp, err := NewExporter(BrokerBuildInfo, "v0.30.0", WithMakuNames(tc.on))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = exp.Shutdown(ctx) }()
			s, err := New(exp.Meter())
			if err != nil {
				t.Fatal(err)
			}
			s.Appended(ctx, "message", "harness", []string{"jwt"})
			s.AppendTook(ctx, 3*time.Millisecond)
			s.ObserveRoom("3kq7x2ma", "Active", true, 2, time.Unix(1_700_000_000, 0))
			s.BridgeStalled(ctx, "cursor_lost")
			got := series(scrape(t, exp.Handler()))
			n := 0
			for k, v := range got {
				if !strings.HasPrefix(k, "rooms_") && !strings.HasPrefix(k, "rooms{") {
					continue
				}
				n++
				m, ok := got["maku_"+k]
				switch {
				case tc.on && (!ok || m != v):
					t.Errorf("%s = %s, maku_%s = %q (present %v)", k, v, k, m, ok)
				case !tc.on && ok:
					t.Errorf("maku_%s served with the switch off", k)
				}
			}
			if n < 6 {
				t.Fatalf("scraped %d rooms series, want at least 6", n)
			}
			if _, ok := got[`maku_rooms_build_info{version="v0.30.0"}`]; ok != tc.on {
				t.Errorf("maku_rooms_build_info present = %v, want %v", ok, tc.on)
			}
		})
	}
}

// A family whose maku_ name is already a real family keeps only the real one: never two families
// of one name in a scrape, which Prometheus would reject.
func TestMakuGathererSkipsATwinThatIsAlreadyReal(t *testing.T) {
	fam := func(name string, v float64) *dto.MetricFamily {
		typ := dto.MetricType_GAUGE
		return &dto.MetricFamily{Name: &name, Type: &typ, Metric: []*dto.Metric{{Gauge: &dto.Gauge{Value: &v}}}}
	}
	g := makuGatherer{reg: prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
		return []*dto.MetricFamily{fam("rooms_x", 1), fam("maku_rooms_x", 2), fam("rooms_y", 3)}, nil
	})}
	mfs, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]float64{}
	for _, mf := range mfs {
		got[mf.GetName()] = append(got[mf.GetName()], mf.GetMetric()[0].GetGauge().GetValue())
	}
	for name, want := range map[string]float64{"rooms_x": 1, "maku_rooms_x": 2, "rooms_y": 3, "maku_rooms_y": 3} {
		if v := got[name]; len(v) != 1 || v[0] != want {
			t.Errorf("%s = %v, want [%v]", name, v, want)
		}
	}
}
