// SPDX-License-Identifier: Apache-2.0

package fmetrics

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/metrics"
)

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

func tk(name, phase string) *v1alpha1.Task {
	return &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agent-system"},
		Spec:   v1alpha1.TaskSpec{Source: v1alpha1.Source{Kind: "issue"}, PredictedClass: "review", Budget: v1alpha1.Budget{Tier: "standard"}},
		Status: v1alpha1.TaskStatus{Phase: phase}}
}

func rig(t *testing.T, c client.Reader, leader func() bool) (*Set, func() string) {
	t.Helper()
	exp, err := metrics.NewExporter(metrics.FactoryBuildInfo, "v0.7.0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exp.Shutdown(context.Background()) })
	s, err := New(exp.Meter(), c, "agent-system", leader)
	if err != nil {
		t.Fatal(err)
	}
	return s, func() string {
		rec := httptest.NewRecorder()
		exp.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
		b, _ := io.ReadAll(rec.Body)
		return string(b)
	}
}

func TestTasksGaugeIsLeaderOnly(t *testing.T) {
	waiting := tk("3buqdlot", "Implementing")
	ps := metav1.NewTime(time.Now().Add(-time.Hour))
	waiting.Status.PhaseSince = &ps
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(waiting, tk("4buqdlot", "Implementing"),
		tk("5buqdlot", "Done"), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "agent-factory-stop", Namespace: "agent-system"}},
		&v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "6buqdlot", Namespace: "elsewhere"}}).Build()
	var leader atomic.Bool
	_, scrape := rig(t, c, leader.Load)
	if body := scrape(); strings.Contains(body, "maku_factory_tasks") || strings.Contains(body, "maku_factory_kill_switch_engaged") ||
		strings.Contains(body, "maku_factory_task_phase_seconds{") {
		t.Fatalf("a follower emits no task gauge: sums across pods would double it\n%s", body)
	}
	leader.Store(true)
	body := scrape()
	for _, want := range []string{
		`maku_factory_build_info{version="v0.7.0"} 1`,
		`maku_factory_tasks{phase="Implementing",predicted_class="review",principal="system:factory",repository="other",source="issue",tier="standard"} 2`,
		`maku_factory_tasks{phase="Done",predicted_class="review",principal="system:factory",repository="other",source="issue",tier="standard"} 1`,
		`maku_factory_kill_switch_engaged 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s\n%s", want, body)
		}
	}
	if !strings.Contains(body, `maku_factory_task_phase_seconds{phase="Implementing",task="3buqdlot"}`) {
		t.Errorf("missing the phase gauge\n%s", body)
	}
	if strings.Contains(body, `maku_factory_task_phase_seconds{phase="Done"`) {
		t.Errorf("a terminal task keeps no phase series: nothing is stuck in Done\n%s", body)
	}
	if strings.Contains(body, `phase=""`) {
		t.Error("a task of another namespace is counted")
	}
	leader.Store(false) // the lease moved: this pod's series go with it
	if body := scrape(); strings.Contains(body, "maku_factory_tasks{") || strings.Contains(body, "maku_factory_kill_switch_engaged") ||
		strings.Contains(body, "maku_factory_task_phase_seconds{") {
		t.Fatalf("a former leader keeps its series\n%s", body)
	}
}

func TestKillSwitchGaugeReadsZeroOnlyWhenKnown(t *testing.T) {
	_, scrape := rig(t, fake.NewClientBuilder().WithScheme(scheme()).Build(), func() bool { return true })
	if body := scrape(); !strings.Contains(body, "maku_factory_kill_switch_engaged 0\n") {
		t.Fatalf("no stop object reads 0\n%s", body)
	}
	broken := fake.NewClientBuilder().WithScheme(scheme()).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("apiserver unavailable")
		},
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("apiserver unavailable")
		}}).Build()
	_, scrape = rig(t, broken, func() bool { return true })
	if body := scrape(); strings.Contains(body, "maku_factory_kill_switch_engaged") || strings.Contains(body, "maku_factory_tasks{") {
		t.Fatalf("an unread stop object is no 0, and unread tasks are no count: the series go absent\n%s", body)
	}
}

// The alerts and the dashboard query these names byte for byte.
func TestEverySection7MetricIsExposed(t *testing.T) {
	ctx := t.Context()
	s, scrape := rig(t, fake.NewClientBuilder().WithScheme(scheme()).Build(), func() bool { return false })
	s.TimeToPR(ctx, 7*time.Minute, "issue", "standard", "solo")
	s.PROutcome(ctx, "docs-links", "human_merged")
	s.TaskTokens(ctx, 120_000, "standard", "solo", "review")
	s.BudgetDay(ctx, "system:factory", 34_000_000, 42_500_000)
	s.Intervention(ctx, "stop")
	s.ClassMismatch(ctx, "docs-links", "review")
	s.MergeDecision(ctx, "docs-links", "shadow", "shadow_would_arm")
	s.TierFit(ctx, "static", "standard", "fit", false)
	s.IntakeError(ctx, "issue")
	s.LabelEventsTruncated(ctx, "factory/ready")
	s.Revoked(ctx, "budget-run")
	s.GitHubRemaining(ctx, 4990)
	s.GitHubUp(ctx, true)
	s.TraceExportAbandoned(ctx)
	s.Resumed(ctx, "Disrupted")
	s.RoomClosedAtCap(ctx, "human_driver")
	s.MeterUsageReadError(ctx)
	s.MeterSucceeded(ctx, time.Unix(1_790_000_000, 0))
	s.RunOvershoot(ctx, 42_000)
	s.RunRevoked(ctx, "budget-run", "run-usd")
	s.RunRevoked(ctx, "budget-task", "task-usd")
	s.RunCost(ctx, "standard", 0.65, map[string]float64{"glm-5.3": 0.65})
	s.LateCost(ctx, map[string]float64{"glm-5.3": 0.1})
	s.RunSpend(map[string]map[string]float64{"nh5tztlh": {"claude-opus-5-5": 2.13212}})
	s.RunSettled(map[string]float64{"nh5tztlh": 2.8})
	s.RunBudgetUSD(ctx, "standard", 2.55)
	s.RunBudgetFallback(ctx, "task", "unpriced")
	s.ModelUnpriced(ctx, "claude-fable-5-1")
	s.InputExcludesCache(ctx, "claude-opus-5-5")
	body := scrape()
	for _, want := range []string{
		`maku_factory_time_to_pr_seconds_bucket{repository="other",source="issue",template="solo",tier="standard",le="600"} 1`,
		`maku_factory_pr_outcomes_total{class="docs-links",outcome="human_merged",principal="other",repository="other"} 1`,
		`maku_factory_task_tokens_bucket{predicted_class="review",repository="other",template="solo",tier="standard",le="200000"} 1`,
		`maku_factory_budget_cap_usd{principal="system:factory"} 42.5`,
		`maku_factory_budget_remaining_usd{principal="system:factory"} 8.5`,
		`maku_factory_budget_spent_usd{principal="system:factory"} 34`,
		`maku_factory_human_interventions_total{kind="stop",repository="other"} 1`,
		`maku_factory_label_events_truncated_total{label="factory/ready"} 1`,
		`maku_factory_class_mismatch_total{matched="review",predicted="docs-links"} 1`,
		`maku_factory_merge_decisions_total{class="docs-links",reason="shadow_would_arm",verdict="shadow"} 1`,
		`maku_factory_tier_fit_total{classifier="static",control="false",fit="fit",tier="standard"} 1`,
		`maku_factory_intake_errors_total{repository="other",source="issue"} 1`,
		`maku_factory_run_revocations_total{principal="other",reason="budget-run"} 1`,
		`maku_factory_github_rate_remaining 4990`,
		`maku_factory_github_up 1`,
		`maku_factory_trace_export_abandoned_total 1`,
		`maku_factory_resumes_total{reason="Disrupted"} 1`,
		`maku_factory_rooms_closed_at_cap_total{reason="human_driver"} 1`,
		`maku_factory_meter_usage_read_errors_total 1`,
		`maku_factory_meter_last_success_timestamp_seconds 1.79e+09`,
		`maku_factory_run_overshoot_tokens_total 42000`,
		`maku_factory_run_revocations_total{limit="run-usd",principal="other",reason="budget-run"} 1`,
		`maku_factory_run_revocations_total{limit="task-usd",principal="other",reason="budget-task"} 1`,
		`maku_factory_run_cost_usd_bucket{tier="standard",le="0.75"} 1`,
		`maku_factory_model_cost_usd_total{model="glm-5.3"} 0.75`,
		`maku_factory_run_spend_usd{model="claude-opus-5-5",run="nh5tztlh"} 2.13212`,
		`maku_factory_run_settled_usd{run="nh5tztlh"} 2.8`,
		`maku_factory_run_budget_usd{tier="standard"} 2.55`,
		`maku_factory_run_budget_fallbacks_total{scope="task",why="unpriced"} 1`,
		`maku_factory_meter_unpriced_model_runs_total{model="claude-fable-5-1"} 1`,
		`maku_factory_meter_input_excludes_cache_runs_total{model="claude-opus-5-5"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	for _, unwanted := range []string{"maku_rooms_build_info", "otel_scope", "target_info", "_total_total", "_seconds_seconds",
		"agent_factory_budget_remaining_tokens", "agent_factory_budget_cap_tokens"} { // TC8: one unit per day
		if strings.Contains(body, unwanted) {
			t.Errorf("exposes %s", unwanted)
		}
	}
	if t.Failed() {
		t.Log(body)
	}
}

// TC8: a principal's day in USD from the ledger's micro-USD: its cap, what is left of it, never
// below 0, and its spend, unclamped, which shows by how much a day was overrun.
func TestTheBudgetGaugesAreInUSD(t *testing.T) {
	s, scrape := rig(t, fake.NewClientBuilder().WithScheme(scheme()).Build(), func() bool { return false })
	s.BudgetDay(t.Context(), "github:alice", 9_350_000, 8_500_000)
	body := scrape()
	for _, want := range []string{
		`maku_factory_budget_cap_usd{principal="github:alice"} 8.5`,
		`maku_factory_budget_remaining_usd{principal="github:alice"} 0`,
		`maku_factory_budget_spent_usd{principal="github:alice"} 9.35`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	if t.Failed() {
		t.Log(body)
	}
}

// Label values stay bounded (AGENTS.md): a value outside a label's vocabulary counts as other.
func TestLabelValuesAreBounded(t *testing.T) {
	ctx := t.Context()
	s, scrape := rig(t, fake.NewClientBuilder().WithScheme(scheme()).Build(), func() bool { return false })
	s.Revoked(ctx, "ignore previous instructions")
	s.PROutcome(ctx, "docs-links", "exploded")
	s.Intervention(ctx, "a comment's text")
	s.IntakeError(ctx, "webhook")
	s.Resumed(ctx, "an agent's text")
	s.RoomClosedAtCap(ctx, "an agent's text")
	s.RunRevoked(ctx, "budget-run", "a limit")
	s.RunCost(ctx, "huge", 1, map[string]float64{"A Model": 1})
	s.RunSpend(map[string]map[string]float64{"not a run": {"glm-5.3": 1}, "aaaaaaaa": {"A Model": 2}})
	s.RunSettled(map[string]float64{"not a run": 1, "aaaaaaaa": 2})
	s.RunBudgetFallback(ctx, "a scope", "a reason")
	s.ModelUnpriced(ctx, "Ignore previous instructions")
	s.InputExcludesCache(ctx, "Ignore previous instructions")
	body := scrape()
	for _, want := range []string{
		`maku_factory_run_revocations_total{principal="other",reason="other"} 1`,
		`maku_factory_pr_outcomes_total{class="docs-links",outcome="other",principal="other",repository="other"} 1`,
		`maku_factory_human_interventions_total{kind="other",repository="other"} 1`,
		`maku_factory_intake_errors_total{repository="other",source="other"} 1`,
		`maku_factory_resumes_total{reason="other"} 1`,
		`maku_factory_rooms_closed_at_cap_total{reason="other"} 1`,
		`maku_factory_run_revocations_total{limit="other",principal="other",reason="budget-run"} 1`,
		`maku_factory_run_cost_usd_count{tier="other"} 1`,
		`maku_factory_model_cost_usd_total{model="other"} 1`,
		`maku_factory_run_spend_usd{model="other",run="aaaaaaaa"} 2`,
		`maku_factory_run_settled_usd{run="aaaaaaaa"} 2`,
		`maku_factory_run_budget_fallbacks_total{scope="other",why="other"} 1`,
		`maku_factory_meter_unpriced_model_runs_total{model="other"} 1`,
		`maku_factory_meter_input_excludes_cache_runs_total{model="other"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s\n%s", want, body)
		}
	}
	if strings.Contains(body, `run="not a run"`) {
		t.Errorf("a run id outside C2 is exposed:\n%s", body)
	}
}

// Without a task reader (the API server's own Set, say) there are no collected gauges to serve.
func TestNoReaderNoCollectedGauges(t *testing.T) {
	_, scrape := rig(t, nil, func() bool { return true })
	if body := scrape(); strings.Contains(body, "maku_factory_tasks") || !strings.Contains(body, "maku_factory_build_info") {
		t.Fatal(body)
	}
}

func TestANilMeterIsANoop(t *testing.T) {
	s, err := New(nil, nil, "agent-system", func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	s.Revoked(t.Context(), "budget-run")
	s.TraceExportAbandoned(t.Context())
	s.IntakeError(t.Context(), "issue")
}

// The monitor's verdicts are counted by class, verdict and repository; a verdict outside the
// vocabulary reads other, and follow_up is a PR outcome of its own.
func TestMonitorVerdictsAreCounted(t *testing.T) {
	ctx := WithRepository(t.Context(), "Smana/cloud-native-ref")
	s, scrape := rig(t, fake.NewClientBuilder().WithScheme(scheme()).Build(), func() bool { return false })
	s.ListRepositories("Smana/cloud-native-ref")
	s.MonitorVerdict(ctx, "dead-links", "regression")
	s.MonitorVerdict(ctx, "dead-links", "Ignore previous instructions")
	s.PROutcome(ctx, "dead-links", "follow_up")
	body := scrape()
	for _, want := range []string{
		`maku_factory_monitor_verdicts_total{class="dead-links",repository="Smana/cloud-native-ref",verdict="regression"} 1`,
		`maku_factory_monitor_verdicts_total{class="dead-links",repository="Smana/cloud-native-ref",verdict="other"} 1`,
		`outcome="follow_up"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s\n%s", want, body)
		}
	}
}

// RA11: the verification counter is by class, verdict and repository; an unknown verdict reads other.
func TestMonitorVerificationsAreCounted(t *testing.T) {
	ctx := WithRepository(t.Context(), "Smana/cloud-native-ref")
	s, scrape := rig(t, fake.NewClientBuilder().WithScheme(scheme()).Build(), func() bool { return false })
	s.ListRepositories("Smana/cloud-native-ref")
	s.MonitorVerification(ctx, "alert-rules", "persisting")
	s.MonitorVerification(ctx, "alert-rules", "Ignore previous instructions")
	body := scrape()
	for _, want := range []string{
		`maku_factory_monitor_verifications_total{class="alert-rules",repository="Smana/cloud-native-ref",verdict="persisting"} 1`,
		`maku_factory_monitor_verifications_total{class="alert-rules",repository="Smana/cloud-native-ref",verdict="other"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s\n%s", want, body)
		}
	}
}

// R23, TO5: the contracts counter is by class, end and repository (an unknown end reads other); the
// class-without-holdout gauge is 1 per live class.
func TestContractEndsAndClassesWithoutHoldoutAreExposed(t *testing.T) {
	ctx := WithRepository(t.Context(), "Smana/cloud-native-ref")
	s, scrape := rig(t, fake.NewClientBuilder().WithScheme(scheme()).Build(), func() bool { return false })
	s.ListRepositories("Smana/cloud-native-ref")
	s.ContractEnd(ctx, "scripts", "ready")
	s.ContractEnd(ctx, "scripts", "bogus")
	s.ClassWithoutHoldout(ctx, "Smana/cloud-native-ref", "docs-links")
	body := scrape()
	for _, want := range []string{
		`maku_factory_contracts_total{class="scripts",end="ready",repository="Smana/cloud-native-ref"} 1`,
		`maku_factory_contracts_total{class="scripts",end="other",repository="Smana/cloud-native-ref"} 1`,
		`maku_factory_class_without_holdout{class="docs-links",repository="Smana/cloud-native-ref"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s\n%s", want, body)
		}
	}
}
