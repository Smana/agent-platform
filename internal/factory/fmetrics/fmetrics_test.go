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

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/metrics"
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
	if body := scrape(); strings.Contains(body, "agent_factory_tasks") || strings.Contains(body, "agent_factory_kill_switch_engaged") ||
		strings.Contains(body, "agent_factory_task_phase_seconds{") {
		t.Fatalf("a follower emits no task gauge: sums across pods would double it\n%s", body)
	}
	leader.Store(true)
	body := scrape()
	for _, want := range []string{
		`agent_factory_build_info{version="v0.7.0"} 1`,
		`agent_factory_tasks{phase="Implementing",predicted_class="review",source="issue",tier="standard"} 2`,
		`agent_factory_tasks{phase="Done",predicted_class="review",source="issue",tier="standard"} 1`,
		`agent_factory_kill_switch_engaged 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s\n%s", want, body)
		}
	}
	if !strings.Contains(body, `agent_factory_task_phase_seconds{phase="Implementing",task="3buqdlot"}`) {
		t.Errorf("missing the phase gauge\n%s", body)
	}
	if strings.Contains(body, `agent_factory_task_phase_seconds{phase="Done"`) {
		t.Errorf("a terminal task keeps no phase series: nothing is stuck in Done\n%s", body)
	}
	if strings.Contains(body, `phase=""`) {
		t.Error("a task of another namespace is counted")
	}
	leader.Store(false) // the lease moved: this pod's series go with it
	if body := scrape(); strings.Contains(body, "agent_factory_tasks{") || strings.Contains(body, "agent_factory_kill_switch_engaged") ||
		strings.Contains(body, "agent_factory_task_phase_seconds{") {
		t.Fatalf("a former leader keeps its series\n%s", body)
	}
}

func TestKillSwitchGaugeReadsZeroOnlyWhenKnown(t *testing.T) {
	_, scrape := rig(t, fake.NewClientBuilder().WithScheme(scheme()).Build(), func() bool { return true })
	if body := scrape(); !strings.Contains(body, "agent_factory_kill_switch_engaged 0\n") {
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
	if body := scrape(); strings.Contains(body, "agent_factory_kill_switch_engaged") || strings.Contains(body, "agent_factory_tasks{") {
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
	s.BudgetRemaining(ctx, "system:factory", 24_000_000)
	s.BudgetCap(ctx, "system:factory", 25_000_000)
	s.Intervention(ctx, "stop")
	s.ClassMismatch(ctx, "docs-links", "review")
	s.TierFit(ctx, "static", "standard", "fit", false)
	s.IntakeError(ctx, "issue")
	s.LabelEventsTruncated(ctx, "factory/ready")
	s.Revoked(ctx, "budget-run")
	s.GitHubRemaining(ctx, 4990)
	s.TraceExportAbandoned(ctx)
	body := scrape()
	for _, want := range []string{
		`agent_factory_time_to_pr_seconds_bucket{source="issue",template="solo",tier="standard",le="600"} 1`,
		`agent_factory_pr_outcomes_total{class="docs-links",outcome="human_merged"} 1`,
		`agent_factory_task_tokens_bucket{predicted_class="review",template="solo",tier="standard",le="200000"} 1`,
		`agent_factory_budget_remaining_tokens{principal="system:factory"} 2.4e+07`,
		`agent_factory_budget_cap_tokens{principal="system:factory"} 2.5e+07`,
		`agent_factory_human_interventions_total{kind="stop"} 1`,
		`agent_factory_label_events_truncated_total{label="factory/ready"} 1`,
		`agent_factory_class_mismatch_total{matched="review",predicted="docs-links"} 1`,
		`agent_factory_tier_fit_total{classifier="static",control="false",fit="fit",tier="standard"} 1`,
		`agent_factory_intake_errors_total{source="issue"} 1`,
		`agent_factory_run_revocations_total{reason="budget-run"} 1`,
		`agent_factory_github_rate_remaining 4990`,
		`agent_factory_trace_export_abandoned_total 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	for _, unwanted := range []string{"rooms_build_info", "otel_scope", "target_info", "_total_total", "_seconds_seconds"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("exposes %s", unwanted)
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
	body := scrape()
	for _, want := range []string{
		`agent_factory_run_revocations_total{reason="other"} 1`,
		`agent_factory_pr_outcomes_total{class="docs-links",outcome="other"} 1`,
		`agent_factory_human_interventions_total{kind="other"} 1`,
		`agent_factory_intake_errors_total{source="other"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s\n%s", want, body)
		}
	}
}

// Without a task reader (the API server's own Set, say) there are no collected gauges to serve.
func TestNoReaderNoCollectedGauges(t *testing.T) {
	_, scrape := rig(t, nil, func() bool { return true })
	if body := scrape(); strings.Contains(body, "agent_factory_tasks") || !strings.Contains(body, "agent_factory_build_info") {
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
