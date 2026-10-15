// SPDX-License-Identifier: Apache-2.0

// Package fmetrics is every §7 metric of agent-factory, as OpenTelemetry instruments on the meter
// internal/metrics exports (ruling SE): the names start agent_factory_ and are exposed exactly as
// written. Each is registered now, so later phases only record. The task and kill-switch gauges
// are read from the cache at scrape time, by the leader only: two replicas would double them.
package fmetrics

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/killswitch"
)

const scope = "github.com/Smana/agent-platform/internal/factory"

// collectTimeout bounds the cache reads of one scrape.
const collectTimeout = 5 * time.Second

// other replaces a label value outside its vocabulary: label values stay bounded (AGENTS.md).
const other = "other"

// The closed vocabularies of the labels callers could otherwise fill with anything. The others
// (tier, template, source, class, classifier, principal) come from the Task CRD's enums or the
// factory's config.
func outcomes() []string { return []string{"auto_merged", "human_merged", "closed", "reverted"} }
func interventions() []string {
	return []string{"stop", "retry", "request_changes", "steer", "takeover", "approve"}
}
func revocations() []string {
	return []string{"budget-run", "budget-task", "budget-principal", "budget-fleet", "manual",
		"budget-task-shadow", "budget-principal-shadow"}
}
func revocationLimits() []string {
	return []string{"run-usd", "run-tokens", "task-usd", "task-tokens", "gateway-ceiling", "fleet", "principal-day"}
}
func budgetFallbacks() []string { return []string{"unpriced", "untyped"} }
func budgetScopes() []string    { return []string{"run", "task"} }
func tiers() []string           { return []string{"light", "standard", "frontier"} }
func intakeSources() []string   { return []string{"issue", "runlore", "schedule", "api"} }
func resumeReasons() []string   { return []string{"Disrupted", "PodLost"} }
func roomCapReasons() []string  { return []string{"human_driver", "facts_due"} }

func oneOf(v string, set []string) string {
	if slices.Contains(set, v) {
		return v
	}
	return other
}

// Bucket bounds sit on the thresholds that matter: SC-1's 30-minute p50 to a PR, and the tier
// budgets (0.6, 3 and 8 M tokens per task).
var (
	timeToPRBuckets   = []float64{60, 300, 600, 900, 1800, 3600, 7200}
	taskTokensBuckets = []float64{50_000, 100_000, 200_000, 400_000, 600_000, 1_000_000, 2_000_000, 3_000_000, 5_000_000, 8_000_000, 12_000_000}
	// The default USD run caps (0.51, 2.55, 6.80) and the v2 cohort's $0.58–0.76 runs.
	runCostBuckets = []float64{0.1, 0.25, 0.51, 0.75, 1, 1.5, 2, 2.55, 3.5, 5, 6.8, 10}
)

// Set is the factory's instruments. Record through its methods, which fix each label set.
type Set struct {
	timeToPR        metric.Float64Histogram
	prOutcomes      metric.Int64Counter
	taskTokens      metric.Int64Histogram
	budgetRemaining metric.Int64Gauge
	budgetCap       metric.Int64Gauge
	interventions   metric.Int64Counter
	classMismatch   metric.Int64Counter
	tierFit         metric.Int64Counter
	intakeErrors    metric.Int64Counter
	eventsTruncated metric.Int64Counter
	revocations     metric.Int64Counter
	githubRemaining metric.Int64Gauge
	githubUp        metric.Int64Gauge
	traceAbandoned  metric.Int64Counter
	resumes         metric.Int64Counter
	roomsCapped     metric.Int64Counter
	usageErrors     metric.Int64Counter
	meterSuccess    metric.Int64Gauge
	overshoot       metric.Int64Counter
	runCost         metric.Float64Histogram
	runBudgetUSD    metric.Float64Gauge
	budgetFallbacks metric.Int64Counter
}

// New makes the instruments on meter (nil: a no-op meter) and, when tasks is set, the leader-only
// gauges agent_factory_tasks{phase,source,predicted_class,tier} and
// agent_factory_kill_switch_engaged, read from tasks in namespace ns.
func New(meter metric.Meter, tasks client.Reader, ns string, leader func() bool) (*Set, error) {
	if meter == nil {
		meter = noop.NewMeterProvider().Meter(scope)
	}
	s := &Set{}
	var errs []error
	check := func(err error) { errs = append(errs, err) }
	var err error
	s.timeToPR, err = meter.Float64Histogram("agent_factory_time_to_pr_seconds",
		metric.WithDescription("From intake to the PR opening (§7, SC-1)."), metric.WithExplicitBucketBoundaries(timeToPRBuckets...))
	check(err)
	s.prOutcomes, err = meter.Int64Counter("agent_factory_pr_outcomes_total",
		metric.WithDescription("PR outcomes by class: auto_merged, human_merged, closed, reverted (§7, SC-11)."))
	check(err)
	s.taskTokens, err = meter.Int64Histogram("agent_factory_task_tokens",
		metric.WithDescription("Tokens per finished task (§7)."), metric.WithExplicitBucketBoundaries(taskTokensBuckets...))
	check(err)
	s.budgetRemaining, err = meter.Int64Gauge("agent_factory_budget_remaining_tokens",
		metric.WithDescription("Tokens left today per principal (§6.2)."))
	check(err)
	s.budgetCap, err = meter.Int64Gauge("agent_factory_budget_cap_tokens",
		metric.WithDescription("The principal's daily token cap (§6.2), the dashboard's denominator."))
	check(err)
	s.interventions, err = meter.Int64Counter("agent_factory_human_interventions_total",
		metric.WithDescription("How dark the factory really is (§7)."))
	check(err)
	s.classMismatch, err = meter.Int64Counter("agent_factory_class_mismatch_total",
		metric.WithDescription("Predicted class versus the class policy-bot matched (§2)."))
	check(err)
	s.tierFit, err = meter.Int64Counter("agent_factory_tier_fit_total",
		metric.WithDescription("After-the-fact tier fit per classifier (§7, SC-10)."))
	check(err)
	s.intakeErrors, err = meter.Int64Counter("agent_factory_intake_errors_total",
		metric.WithDescription("Failed intake polls or requests."))
	check(err)
	s.eventsTruncated, err = meter.Int64Counter("agent_factory_label_events_truncated_total",
		metric.WithDescription("Label polls that met the forge's event cap and left the label for the next poll."))
	check(err)
	s.revocations, err = meter.Int64Counter("agent_factory_run_revocations_total",
		metric.WithDescription("Runs the factory revoked, by reason."))
	check(err)
	s.githubRemaining, err = meter.Int64Gauge("agent_factory_github_rate_remaining",
		metric.WithDescription("The factory App's remaining REST rate limit."))
	check(err)
	s.githubUp, err = meter.Int64Gauge("agent_factory_github_up",
		metric.WithDescription("1 while a GitHub call succeeded in the last 3 minutes, else 0. Readiness ignores GitHub, so this is the outage signal (FactoryGitHubUnreachable)."))
	check(err)
	s.traceAbandoned, err = meter.Int64Counter("agent_factory_trace_export_abandoned_total",
		metric.WithDescription("Task spans given up after the collector refused them for a day (ruling ST2)."))
	check(err)
	s.resumes, err = meter.Int64Counter("agent_factory_resumes_total",
		metric.WithDescription("Runs the factory resumed on its own after their pod was disrupted or lost, by the AgentRun's reason."))
	check(err)
	s.roomsCapped, err = meter.Int64Counter("agent_factory_rooms_closed_at_cap_total",
		metric.WithDescription("Finished tasks' rooms closed a day after the end, by the wait cut short: a driver still holding the room, or facts the broker never took (#92)."))
	check(err)
	// #54: the meter's health. Only the leader meters, so only the leader sets the timestamp:
	// an alert reads its max across pods.
	s.usageErrors, err = meter.Int64Counter("agent_factory_meter_usage_read_errors_total",
		metric.WithDescription("Meter ticks whose usage read failed: no run or task cap was checked on them."))
	check(err)
	s.meterSuccess, err = meter.Int64Gauge("agent_factory_meter_last_success_timestamp_seconds",
		metric.WithDescription("When the meter last read usage and settled it in the day's ledger (Unix seconds)."))
	check(err)
	s.overshoot, err = meter.Int64Counter("agent_factory_run_overshoot_tokens_total",
		metric.WithDescription("Tokens runs spent past their own cap before the meter revoked them."))
	check(err)
	s.runCost, err = meter.Float64Histogram("agent_factory_run_cost_usd",
		metric.WithDescription("A priced run's USD cost when it ended, cache reads at their own rate (OD-6)."),
		metric.WithExplicitBucketBoundaries(runCostBuckets...))
	check(err)
	s.runBudgetUSD, err = meter.Float64Gauge("agent_factory_run_budget_usd",
		metric.WithDescription("Each tier's USD run cap; absent for a tier capped in tokens."))
	check(err)
	s.budgetFallbacks, err = meter.Int64Counter("agent_factory_run_budget_fallbacks_total",
		metric.WithDescription("Runs and tasks of a USD-capped tier held to their token cap instead: an unpriced model, or a meter.query without gen_ai_token_type."))
	check(err)
	if tasks != nil {
		check(registerCollected(meter, tasks, ns, leader))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("fmetrics: %w", err)
	}
	return s, nil
}

// registerCollected reads the tasks and the stop object at each scrape, on the leader only. A
// failed read observes nothing, so the series go absent rather than read as 0: an unread stop
// object is not a disengaged one.
func registerCollected(meter metric.Meter, r client.Reader, ns string, leader func() bool) error {
	tasks, err := meter.Int64ObservableGauge("agent_factory_tasks", metric.WithDescription("Tasks by state (§7)."))
	if err != nil {
		return err
	}
	stop, err := meter.Int64ObservableGauge("agent_factory_kill_switch_engaged",
		metric.WithDescription("1 while the stop is engaged: the stop object or factory/stop on the control issue (§6.1)."))
	if err != nil {
		return err
	}
	phase, err := meter.Float64ObservableGauge("agent_factory_task_phase_seconds",
		metric.WithDescription("Seconds a task has spent in its phase (TaskStuck)."))
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		if !leader() {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, collectTimeout)
		defer cancel()
		var l v1alpha1.TaskList
		if err := r.List(ctx, &l, client.InNamespace(ns)); err == nil {
			counts := map[[4]string]int64{}
			for _, t := range l.Items {
				counts[[4]string{t.Status.Phase, t.Spec.Source.Kind, t.Spec.PredictedClass, t.Spec.Budget.Tier}]++
			}
			for k, n := range counts {
				o.ObserveInt64(tasks, n, metric.WithAttributes(attribute.String("phase", k[0]), attribute.String("source", k[1]),
					attribute.String("predicted_class", k[2]), attribute.String("tier", k[3])))
			}
			for _, t := range l.Items {
				if !v1alpha1.TerminalPhase(t.Status.Phase) && t.Status.PhaseSince != nil {
					o.ObserveFloat64(phase, time.Since(t.Status.PhaseSince.Time).Seconds(),
						metric.WithAttributes(attribute.String("task", t.Name), attribute.String("phase", t.Status.Phase)))
				}
			}
		}
		if on, err := killswitch.Engaged(ctx, r, ns); err == nil {
			v := int64(0)
			if on {
				v = 1
			}
			o.ObserveInt64(stop, v)
		}
		return nil
	}, tasks, stop, phase)
	return err
}

// TimeToPR records a task's time from intake to its PR opening.
func (s *Set) TimeToPR(ctx context.Context, d time.Duration, source, tier, template string) {
	s.timeToPR.Record(ctx, d.Seconds(), metric.WithAttributes(attribute.String("source", source),
		attribute.String("tier", tier), attribute.String("template", template)))
}

// PROutcome counts a task PR's outcome: auto_merged, human_merged, closed or reverted.
func (s *Set) PROutcome(ctx context.Context, class, outcome string) {
	s.prOutcomes.Add(ctx, 1, metric.WithAttributes(attribute.String("class", class),
		attribute.String("outcome", oneOf(outcome, outcomes()))))
}

// TaskTokens records a finished task's tokens.
func (s *Set) TaskTokens(ctx context.Context, tokens int64, tier, template, predictedClass string) {
	s.taskTokens.Record(ctx, tokens, metric.WithAttributes(attribute.String("tier", tier),
		attribute.String("template", template), attribute.String("predicted_class", predictedClass)))
}

// BudgetRemaining sets the tokens a principal has left today.
func (s *Set) BudgetRemaining(ctx context.Context, principal string, tokens int64) {
	s.budgetRemaining.Record(ctx, tokens, metric.WithAttributes(attribute.String("principal", principal)))
}

// BudgetCap sets the principal's daily token cap, the gauge's dashboard denominator (§6.2).
func (s *Set) BudgetCap(ctx context.Context, principal string, tokens int64) {
	s.budgetCap.Record(ctx, tokens, metric.WithAttributes(attribute.String("principal", principal)))
}

// Intervention counts a human stepping in: stop, retry, request_changes, steer, takeover, approve.
func (s *Set) Intervention(ctx context.Context, kind string) {
	s.interventions.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", oneOf(kind, interventions()))))
}

// ClassMismatch counts a task whose predicted class is not the one policy-bot matched.
func (s *Set) ClassMismatch(ctx context.Context, predicted, matched string) {
	s.classMismatch.Add(ctx, 1, metric.WithAttributes(attribute.String("predicted", predicted),
		attribute.String("matched", matched)))
}

// TierFit counts a finished task's tier fit per classifier, OD-14's control group apart.
func (s *Set) TierFit(ctx context.Context, classifier, tier, fit string, control bool) {
	s.tierFit.Add(ctx, 1, metric.WithAttributes(attribute.String("classifier", classifier), attribute.String("tier", tier),
		attribute.String("fit", fit), attribute.String("control", strconv.FormatBool(control))))
}

// IntakeError counts a failed intake poll or request, by source.
func (s *Set) IntakeError(ctx context.Context, source string) {
	s.intakeErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("source", oneOf(source, intakeSources()))))
}

// LabelEventsTruncated counts a label left on its issue because its events passed the forge's cap.
func (s *Set) LabelEventsTruncated(ctx context.Context, label string) {
	s.eventsTruncated.Add(ctx, 1, metric.WithAttributes(attribute.String("label", label)))
}

// Revoked counts a run the factory revoked, by reason: the run meter's OnRevoke.
func (s *Set) Revoked(ctx context.Context, reason string) {
	s.revocations.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", oneOf(reason, revocations()))))
}

// RunRevoked counts a revocation by the run meter, by reason and the limit that fired.
func (s *Set) RunRevoked(ctx context.Context, reason, limit string) {
	s.revocations.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", oneOf(reason, revocations())),
		attribute.String("limit", oneOf(limit, revocationLimits()))))
}

// RunCost records a priced run's USD cost at its end: the run meter's OnCost.
func (s *Set) RunCost(ctx context.Context, tier string, usd float64) {
	s.runCost.Record(ctx, usd, metric.WithAttributes(attribute.String("tier", oneOf(tier, tiers()))))
}

// RunBudgetUSD sets a tier's USD run cap.
func (s *Set) RunBudgetUSD(ctx context.Context, tier string, usd float64) {
	s.runBudgetUSD.Record(ctx, usd, metric.WithAttributes(attribute.String("tier", oneOf(tier, tiers()))))
}

// RunBudgetFallback counts a run (the meter's OnFallback) or a task (the reconciler) held to its
// token cap though its tier caps in USD.
func (s *Set) RunBudgetFallback(ctx context.Context, scope, why string) {
	s.budgetFallbacks.Add(ctx, 1, metric.WithAttributes(attribute.String("scope", oneOf(scope, budgetScopes())),
		attribute.String("why", oneOf(why, budgetFallbacks()))))
}

// Resumed counts a run the factory resumed on its own, by the lost run's AgentRun reason
// (disruption design §6).
func (s *Set) Resumed(ctx context.Context, reason string) {
	s.resumes.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", oneOf(reason, resumeReasons()))))
}

// RoomClosedAtCap counts a finished task's room closed at the cap instead of after its wait (#92).
func (s *Set) RoomClosedAtCap(ctx context.Context, reason string) {
	s.roomsCapped.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", oneOf(reason, roomCapReasons()))))
}

// TraceExportAbandoned counts a task span given up unexported (ruling ST2).
func (s *Set) TraceExportAbandoned(ctx context.Context) { s.traceAbandoned.Add(ctx, 1) }

// GitHubUp sets agent_factory_github_up: whether GitHub answered recently (forge.Healthy).
func (s *Set) GitHubUp(ctx context.Context, up bool) {
	v := int64(0)
	if up {
		v = 1
	}
	s.githubUp.Record(ctx, v)
}

// GitHubRemaining sets the factory App's remaining REST rate limit.
func (s *Set) GitHubRemaining(ctx context.Context, remaining int64) {
	s.githubRemaining.Record(ctx, remaining)
}

// MeterUsageReadError counts a meter tick whose usage read failed (#54).
func (s *Set) MeterUsageReadError(ctx context.Context) { s.usageErrors.Add(ctx, 1) }

// MeterSucceeded sets when the meter last metered (#54).
func (s *Set) MeterSucceeded(ctx context.Context, at time.Time) {
	s.meterSuccess.Record(ctx, at.Unix())
}

// RunOvershoot adds what a run spent past its cap when the meter revoked it (#54): a tick's lag
// in steady state, the whole blind interval's spend after an outage.
func (s *Set) RunOvershoot(ctx context.Context, tokens int64) { s.overshoot.Add(ctx, tokens) }
