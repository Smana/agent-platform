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
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/factory/killswitch"
	"github.com/Smana/maku/internal/factory/runs"
)

const scope = "github.com/Smana/maku/internal/factory"

// collectTimeout bounds the cache reads of one scrape.
const collectTimeout = 5 * time.Second

// other replaces a label value outside its vocabulary: label values stay bounded (AGENTS.md).
const other = "other"

// The closed vocabularies of the labels callers could otherwise fill with anything. The others
// (tier, template, source, class, classifier, principal) come from the Task CRD's enums or the
// factory's config.
func outcomes() []string {
	return []string{"auto_merged", "approved_merged", "human_merged", "closed", "reverted", "follow_up"}
}
func interventions() []string {
	return []string{"stop", "retry", "request_changes", "steer", "takeover", "approve", "split"}
}
func revocations() []string {
	return []string{"budget-run", "budget-task", "budget-principal", "budget-fleet", "manual",
		"budget-task-shadow", "budget-principal-shadow", "budget-person"}
}
func revocationLimits() []string {
	return []string{"run-usd", "run-tokens", "task-usd", "task-tokens", "gateway-ceiling", "fleet", "principal-day"}
}
func budgetFallbacks() []string { return []string{"unpriced", "untyped"} }
func budgetScopes() []string    { return []string{"run", "task"} }
func tiers() []string           { return []string{"light", "standard", "frontier"} }
func intakeSources() []string   { return []string{"issue", "runlore", "schedule", "api", "adopt"} }
func resumeReasons() []string   { return []string{"Disrupted", "PodLost"} }
func roomCapReasons() []string  { return []string{"human_driver", "facts_due"} }
func admissionReasons() []string {
	return []string{"repository_differs_from_the_room", "repository_differs_from_the_branch", "repository_permission", "repository_not_owned",
		"person_budget_exhausted"}
}

// runRE bounds the run label to a C2 run id: the gauge holds only the runs the meter has not
// reported yet, so its cardinality is the live runs' times the models that served them.
var runRE = regexp.MustCompile(`^[a-z2-7]{8}$`)

// modelRE bounds the model label to a backend's pinned model name (glm-5.3, claude-opus-5-5).
var modelRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// principalRE is a ledger key's shape (spend per identity SI5): a sponsored or system principal, a
// person's lowercased GitHub login (v1alpha1.Person), or an API caller's subject with no GitHub
// link. Its values are the people who may start runs and the principals in the config.
var principalRE = regexp.MustCompile(`^(system:[a-z0-9][a-z0-9-]{0,62}|github:[a-z0-9][a-z0-9-]{0,38}|human:[A-Za-z0-9][A-Za-z0-9_-]{0,63})$`)

func charges() []string     { return []string{runs.KindTask, runs.KindRoom} }
func humanPhases() []string { return []string{v1alpha1.PhaseAwaitingHuman, v1alpha1.PhaseEscalated} }

// apiOutcomes are POST /v1/tasks's outcomes (F4): admitted, or a refusal's code. Anything else is
// recorded as other, so the label stays bounded.
func apiOutcomes() []string {
	return []string{"admitted", "kill_switch", "bad_json", "idempotency_key_required", "bad_data_class", "bad_ref",
		"repository_not_allowed", "repository_not_owned", "meter_unpriced", "max_cost_required", "max_cost_over_principal",
		"max_cost_over_class", "not_linked", "link_unverified", "not_a_maintainer", "invalid_draft", "bad_text", "tasks_unavailable",
		"principal_task_cap", "daily_task_cap", "ledger_unavailable", "principal_budget_exhausted", "intake_failed", "duplicate", "idempotency_conflict"}
}

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
	timeToPR             metric.Float64Histogram
	prOutcomes           metric.Int64Counter
	monitorVerdicts      metric.Int64Counter
	monitorVerifications metric.Int64Counter
	taskTokens           metric.Int64Histogram
	budgetCap            metric.Float64Gauge
	budgetRemaining      metric.Float64Gauge
	budgetSpent          metric.Float64Gauge
	interventions        metric.Int64Counter
	classMismatch        metric.Int64Counter
	mergeDecisions       metric.Int64Counter
	tierFit              metric.Int64Counter
	intakeErrors         metric.Int64Counter
	eventsTruncated      metric.Int64Counter
	revocations          metric.Int64Counter
	githubRemaining      metric.Int64Gauge
	githubUp             metric.Int64Gauge
	traceAbandoned       metric.Int64Counter
	resumes              metric.Int64Counter
	steeringReads        metric.Int64Counter
	scoperRuns           metric.Int64Counter
	splits               metric.Int64Counter
	splitChildren        metric.Int64Counter
	scopeBounces         metric.Int64Counter
	loopRuns             metric.Int64Counter
	contracts            metric.Int64Counter
	noHoldout            metric.Int64Gauge
	roomsCapped          metric.Int64Counter
	usageErrors          metric.Int64Counter
	meterSuccess         metric.Int64Gauge
	overshoot            metric.Int64Counter
	runCost              metric.Float64Histogram
	runBudgetUSD         metric.Float64Gauge
	budgetFallbacks      metric.Int64Counter
	owned                metric.Int64Gauge
	refusals             metric.Int64Counter
	unpricedModels       metric.Int64Counter
	excludesCache        metric.Int64Counter
	modelCost            metric.Float64Counter
	charged              metric.Float64Counter
	prsOpened            metric.Int64Counter
	humanWait            metric.Float64Counter
	maxCost              metric.Float64Counter
	apiIntake            metric.Int64Counter

	spendMu sync.Mutex
	spend   map[string]map[string]float64 // the meter's last live spend, run then model
	settled map[string]float64            // the meter's last ledger totals, by run
	live    []runs.Run                    // the meter's last charge info: the runs live or ended within endSettle

	// repos is the config's repository list, the repository label's only values (v2 D10).
	mu    sync.RWMutex
	repos []string
	// sponsors maps each sponsored principal to its sponsor (SI9), set from the config at start.
	sponsors map[string]string
	// perPerson: a task's payer is its person (budgets.perPerson.enabled), else the fleet.
	perPerson bool
}

// ChargePeople sets whether tasks pay as their person, per-person caps being on: the tasks
// gauge's principal (spend per identity SI1).
func (s *Set) ChargePeople(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.perPerson = on
}

func (s *Set) taskPayer(t *v1alpha1.Task) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return runs.TaskPayer(t, s.perPerson)
}

type principalKey struct{}

// WithPrincipal names the payer the series recorded under ctx belong to (spend per identity SI4):
// the reconciler binds its task's, the API its requester's, the meter and the sweep a run's.
// Recorded without one, a series reads other.
func WithPrincipal(ctx context.Context, principal string) context.Context {
	return context.WithValue(ctx, principalKey{}, principal)
}

// PrincipalOf is the payer ctx binds, "" when none: for a consumer's test double.
func PrincipalOf(ctx context.Context) string {
	p, _ := ctx.Value(principalKey{}).(string)
	return p
}

func boundPrincipal(p string) string {
	if !principalRE.MatchString(p) {
		return other
	}
	return p
}

func principalOf(ctx context.Context) attribute.KeyValue {
	return attribute.String("principal", boundPrincipal(PrincipalOf(ctx)))
}

// Sponsors sets each sponsored principal's sponsor (SI9), from the config at start.
func (s *Set) Sponsors(by map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sponsors = maps.Clone(by)
}

// MaxCost adds an admitted request's ceiling to its principal's total (SI9).
func (s *Set) MaxCost(ctx context.Context, microUSD int64) {
	s.maxCost.Add(ctx, float64(microUSD)/1e6, metric.WithAttributes(principalOf(ctx)))
}

// APIIntake counts one POST /v1/tasks by its principal and outcome.
func (s *Set) APIIntake(ctx context.Context, outcome string) {
	s.apiIntake.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", oneOf(outcome, apiOutcomes())), principalOf(ctx)))
}

// ListRepositories sets the values the repository label may take: the config's list, at most
// config.MaxRepositories. Anything else is recorded as other.
func (s *Set) ListRepositories(names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos = slices.Clone(names)
}

// repository is name as the list spells it, other when the list does not hold it.
func (s *Set) repository(name string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if i := slices.IndexFunc(s.repos, func(r string) bool { return strings.EqualFold(r, name) }); i >= 0 {
		return s.repos[i]
	}
	return other
}

type repositoryKey struct{}

// WithRepository names the repository the series recorded under ctx belong to: the reconciler
// binds its task's, a poller its own (v2 D10). Recorded without one, a series reads other.
func WithRepository(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, repositoryKey{}, name)
}

func (s *Set) repoOf(ctx context.Context) attribute.KeyValue {
	name, _ := ctx.Value(repositoryKey{}).(string)
	return attribute.String("repository", s.repository(name))
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
		metric.WithDescription("PR outcomes by class: auto_merged, approved_merged, human_merged, closed, reverted, follow_up (§7, SC-11, AM1)."))
	check(err)
	s.monitorVerdicts, err = meter.Int64Counter("agent_factory_monitor_verdicts_total",
		metric.WithDescription("Post-merge monitor verdicts (merge gate classes MC7), by class and verdict."))
	check(err)
	s.monitorVerifications, err = meter.Int64Counter("agent_factory_monitor_verifications_total",
		metric.WithDescription("RunLore's verdict on a merged fix, as the monitor read it (RA11), by class and verdict: resolved, persisting or none."))
	check(err)
	s.taskTokens, err = meter.Int64Histogram("agent_factory_task_tokens",
		metric.WithDescription("Tokens per finished task (§7)."), metric.WithExplicitBucketBoundaries(taskTokensBuckets...))
	check(err)
	// TC8: the day ledger's unit, USD, and nothing in tokens beside it.
	s.budgetCap, err = meter.Float64Gauge("agent_factory_budget_cap_usd",
		metric.WithDescription("The principal's daily cap in USD (§6.2), the dashboard's denominator."))
	check(err)
	s.budgetRemaining, err = meter.Float64Gauge("agent_factory_budget_remaining_usd",
		metric.WithDescription("USD left today per principal (§6.2), 0 once the day is spent."))
	check(err)
	s.budgetSpent, err = meter.Float64Gauge("agent_factory_budget_spent_usd",
		metric.WithDescription("USD spent today per principal, unclamped: how far past its cap a day ran."))
	check(err)
	s.interventions, err = meter.Int64Counter("agent_factory_human_interventions_total",
		metric.WithDescription("How dark the factory really is (§7)."))
	check(err)
	s.classMismatch, err = meter.Int64Counter("agent_factory_class_mismatch_total",
		metric.WithDescription("Predicted class versus the class policy-bot matched (§2)."))
	check(err)
	s.mergeDecisions, err = meter.Int64Counter("agent_factory_merge_decisions_total",
		metric.WithDescription("The merge gate's decisions by class, verdict and reason (v2 D2); a wait is none."))
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
	s.steeringReads, err = meter.Int64Counter("agent_factory_steering_reads_total",
		metric.WithDescription("Steering notes read at triage, by class and read: used, absent, invalid or unowned (scoper design SC4, SC12)."))
	check(err)
	s.scoperRuns, err = meter.Int64Counter("agent_factory_scoper_runs_total",
		metric.WithDescription("Scoper runs as they end, by class and outcome: ready, question, assumed, missing, failed, budget or budget_calls (scoper design SC11, SC12; harness roles HR2)."))
	check(err)
	s.scopeBounces, err = meter.Int64Counter("agent_factory_scope_bounces_total",
		metric.WithDescription("Bounced tasks leaving their wait, by class and exit: relabel, retry or expired (scoper design SC8, SC12)."))
	check(err)
	s.splits, err = meter.Int64Counter("agent_factory_splits_total",
		metric.WithDescription("Split handoffs by their outcome, counted once: approved, declined, superseded, expired or ignored (splitter design SL14)."))
	check(err)
	s.splitChildren, err = meter.Int64Counter("agent_factory_split_children_total",
		metric.WithDescription("Parts of a split, counted once when their parent ends: landed, dropped or reverted (splitter design SL14)."))
	check(err)
	s.resumes, err = meter.Int64Counter("agent_factory_resumes_total",
		metric.WithDescription("Runs the factory resumed on its own after their pod was disrupted or lost, by the AgentRun's reason."))
	check(err)
	s.contracts, err = meter.Int64Counter("agent_factory_contracts_total",
		metric.WithDescription("Tester runs as they end, by class and end: ready, not_testable, contract_not_red, contract_outside_tests, contract_invalid or unreported (test-first tester R23)."))
	check(err)
	s.noHoldout, err = meter.Int64Gauge("agent_factory_class_without_holdout",
		metric.WithDescription("1 for a live class that declares no holdoutChecks: refused from the next minor (test-first tester TO5)."))
	check(err)
	s.loopRuns, err = meter.Int64Counter("agent_factory_loop_runs_total",
		metric.WithDescription("Implementer runs under the loop policy, by class and how they ended: judge_impossible, stalled, not_met, finished or other (loops design L2, L3)."))
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
	s.owned, err = meter.Int64Gauge("agent_factory_repository_owned",
		metric.WithDescription("1 while the repository's .github/agent-factory.yaml names this factory (v2 D7), else 0."))
	check(err)
	s.refusals, err = meter.Int64Counter("agent_factory_admission_refusals_total",
		metric.WithDescription("Run requests refused on their repository: bound to another, no write on GitHub, or not this factory's."))
	check(err)
	s.unpricedModels, err = meter.Int64Counter("agent_factory_meter_unpriced_model_runs_total",
		metric.WithDescription("Runs a model served with no meter.prices row, by that model: AgentModelUnpriced (J-AC4)."))
	check(err)
	s.excludesCache, err = meter.Int64Counter("agent_factory_meter_input_excludes_cache_runs_total",
		metric.WithDescription("Runs whose input a model's gateway series reported without its cache tokens, by that model: priced as fresh input whole."))
	check(err)
	s.modelCost, err = meter.Float64Counter("agent_factory_model_cost_usd_total",
		metric.WithDescription("USD the factory's runs spent, by the model that served them, counted when each run's cost is reported."))
	check(err)
	s.charged, err = meter.Float64Counter("agent_factory_charged_usd_total",
		metric.WithDescription("USD the day ledgers charged, by the run's payer and kind, added when the write that settles it succeeds (spend per identity SI2)."))
	check(err)
	s.prsOpened, err = meter.Int64Counter("agent_factory_prs_opened_total",
		metric.WithDescription("Task pull requests opened, by the task's payer."))
	check(err)
	s.humanWait, err = meter.Float64Counter("agent_factory_human_wait_seconds_total",
		metric.WithDescription("Seconds tasks waited for a human, by the task's payer, added when a task leaves AwaitingHuman or Escalated."))
	check(err)
	s.maxCost, err = meter.Float64Counter("agent_factory_max_cost_usd_total",
		metric.WithDescription("USD of max_cost the intake api admitted, by sponsored principal (spend per identity SI9)."))
	check(err)
	s.apiIntake, err = meter.Int64Counter("agent_factory_api_intake_total",
		metric.WithDescription("POST /v1/tasks requests, by sponsored principal and outcome (factory interfaces F4)."))
	check(err)
	sponsorInfo, err := meter.Int64ObservableGauge("agent_factory_principal_sponsor_info",
		metric.WithDescription("1 for each sponsored principal, labelled with the maintainer accountable for it (SI9)."))
	check(err)
	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		s.mu.RLock()
		defer s.mu.RUnlock()
		for p, sp := range s.sponsors {
			o.ObserveInt64(sponsorInfo, 1, metric.WithAttributes(attribute.String("principal", boundPrincipal(p)),
				attribute.String("sponsor", boundPrincipal(sp))))
		}
		return nil
	}, sponsorInfo)
	check(err)
	check(s.registerSpend(meter))
	check(s.registerSettled(meter))
	check(s.registerCharges(meter))
	if tasks != nil {
		check(registerCollected(meter, tasks, ns, leader, s.repository, s.taskPayer))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("fmetrics: %w", err)
	}
	return s, nil
}

// registerCollected reads the tasks and the stop object at each scrape, on the leader only. A
// failed read observes nothing, so the series go absent rather than read as 0: an unread stop
// object is not a disengaged one.
func registerCollected(meter metric.Meter, r client.Reader, ns string, leader func() bool, repo func(string) string,
	payer func(*v1alpha1.Task) string) error {
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
			counts := map[[6]string]int64{}
			for i := range l.Items {
				t := &l.Items[i]
				counts[[6]string{t.Status.Phase, t.Spec.Source.Kind, t.Spec.PredictedClass, t.Spec.Budget.Tier, repo(t.Spec.Repository),
					boundPrincipal(payer(t))}]++
			}
			for k, n := range counts {
				o.ObserveInt64(tasks, n, metric.WithAttributes(attribute.String("phase", k[0]), attribute.String("source", k[1]),
					attribute.String("predicted_class", k[2]), attribute.String("tier", k[3]), attribute.String("repository", k[4]),
					attribute.String("principal", k[5])))
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

// RepositoryOwned records whether the fence names this factory on repository (v2 D7).
func (s *Set) RepositoryOwned(ctx context.Context, repository string, owned bool) {
	v := int64(0)
	if owned {
		v = 1
	}
	s.owned.Record(ctx, v, metric.WithAttributes(attribute.String("repository", s.repository(repository))))
}

// AdmissionRefused counts a run request refused on its repository, the one ctx names. Other
// refusals have their own codes and are not counted here.
func (s *Set) AdmissionRefused(ctx context.Context, reason string) {
	if !slices.Contains(admissionReasons(), reason) {
		return
	}
	s.refusals.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason), s.repoOf(ctx), principalOf(ctx)))
}

// TimeToPR records a task's time from intake to its PR opening.
func (s *Set) TimeToPR(ctx context.Context, d time.Duration, source, tier, template string) {
	s.timeToPR.Record(ctx, d.Seconds(), metric.WithAttributes(attribute.String("source", source),
		attribute.String("tier", tier), attribute.String("template", template), s.repoOf(ctx)))
}

// PROpened counts a task's pull request opening, by its repository and payer: where TimeToPR
// records, without the histogram's buckets times every principal (spend per identity SI4).
func (s *Set) PROpened(ctx context.Context) {
	s.prsOpened.Add(ctx, 1, metric.WithAttributes(s.repoOf(ctx), principalOf(ctx)))
}

// PROutcome counts a task PR's outcome: auto_merged (the merger, on the class's conditions),
// approved_merged (the merger, on a maintainer's approval of the head: AM1), human_merged, closed,
// reverted or follow_up.
func (s *Set) PROutcome(ctx context.Context, class, outcome string) {
	s.prOutcomes.Add(ctx, 1, metric.WithAttributes(attribute.String("class", class),
		attribute.String("outcome", oneOf(outcome, outcomes())), s.repoOf(ctx), principalOf(ctx)))
}

// MonitorVerdict counts a merge's final monitor verdict (MC7): healthy, regression or inconclusive.
func (s *Set) MonitorVerdict(ctx context.Context, class, verdict string) {
	s.monitorVerdicts.Add(ctx, 1, metric.WithAttributes(attribute.String("class", class),
		attribute.String("verdict", oneOf(verdict, []string{"healthy", "regression", "inconclusive"})), s.repoOf(ctx)))
}

// MonitorVerification counts RunLore's verdict on a merged fix as the monitor read it (RA11):
// resolved, persisting, or none when the window closed without one.
func (s *Set) MonitorVerification(ctx context.Context, class, verdict string) {
	s.monitorVerifications.Add(ctx, 1, metric.WithAttributes(attribute.String("class", class),
		attribute.String("verdict", oneOf(verdict, []string{"resolved", "persisting", "none"})), s.repoOf(ctx)))
}

// HumanWait adds the time a task waited in AwaitingHuman or Escalated, as it leaves the phase, to
// its payer's waits (spend per identity SI8). Any other phase is no wait for a human.
func (s *Set) HumanWait(ctx context.Context, phase string, d time.Duration) {
	if !slices.Contains(humanPhases(), phase) || d <= 0 {
		return
	}
	s.humanWait.Add(ctx, d.Seconds(), metric.WithAttributes(attribute.String("phase", phase), principalOf(ctx)))
}

// Charged adds a run's settled micro-USD, in dollars, to its payer's charges by kind: the run
// meter's OnCharge, once the write that settles it succeeded (spend per identity SI2).
func (s *Set) Charged(ctx context.Context, principal, kind string, microUSD int64) {
	s.charged.Add(ctx, float64(microUSD)/1e6, metric.WithAttributes(attribute.String("principal", boundPrincipal(principal)),
		attribute.String("kind", oneOf(kind, charges()))))
}

// TaskTokens records a finished task's tokens.
func (s *Set) TaskTokens(ctx context.Context, tokens int64, tier, template, predictedClass string) {
	s.taskTokens.Record(ctx, tokens, metric.WithAttributes(attribute.String("tier", tier),
		attribute.String("template", template), attribute.String("predicted_class", predictedClass), s.repoOf(ctx)))
}

// BudgetDay sets a principal's day from the day ledger's micro-USD: its cap, what is left of it
// (never below 0) and what it spent (unclamped).
func (s *Set) BudgetDay(ctx context.Context, principal string, spentMicroUSD, capMicroUSD int64) {
	at := metric.WithAttributes(attribute.String("principal", principal))
	s.budgetCap.Record(ctx, float64(capMicroUSD)/1e6, at)
	s.budgetRemaining.Record(ctx, float64(max(0, capMicroUSD-spentMicroUSD))/1e6, at)
	s.budgetSpent.Record(ctx, float64(spentMicroUSD)/1e6, at)
}

// Intervention counts a human stepping in: stop, retry, request_changes, steer, takeover, approve.
func (s *Set) Intervention(ctx context.Context, kind string) {
	s.interventions.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", oneOf(kind, interventions())), s.repoOf(ctx)))
}

// ClassMismatch counts a task whose predicted class is not the one policy-bot matched.
func (s *Set) ClassMismatch(ctx context.Context, predicted, matched string) {
	s.classMismatch.Add(ctx, 1, metric.WithAttributes(attribute.String("predicted", predicted),
		attribute.String("matched", matched)))
}

// MergeDecision counts one decision of the merge gate (§5.1): arm, shadow, human, escalate,
// ci_red or ci_cancelled, with its reason code.
func (s *Set) MergeDecision(ctx context.Context, class, verdict, reason string) {
	s.mergeDecisions.Add(ctx, 1, metric.WithAttributes(attribute.String("class", class),
		attribute.String("verdict", verdict), attribute.String("reason", reason)))
}

// TierFit counts a finished task's tier fit per classifier, OD-14's control group apart.
func (s *Set) TierFit(ctx context.Context, classifier, tier, fit string, control bool) {
	s.tierFit.Add(ctx, 1, metric.WithAttributes(attribute.String("classifier", classifier), attribute.String("tier", tier),
		attribute.String("fit", fit), attribute.String("control", strconv.FormatBool(control))))
}

// IntakeError counts a failed intake poll or request, by source.
func (s *Set) IntakeError(ctx context.Context, source string) {
	s.intakeErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("source", oneOf(source, intakeSources())), s.repoOf(ctx)))
}

// LabelEventsTruncated counts a label left on its issue because its events passed the forge's cap.
func (s *Set) LabelEventsTruncated(ctx context.Context, label string) {
	s.eventsTruncated.Add(ctx, 1, metric.WithAttributes(attribute.String("label", label)))
}

// Revoked counts a run the factory revoked, by reason: the run meter's OnRevoke.
func (s *Set) Revoked(ctx context.Context, reason string) {
	s.revocations.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", oneOf(reason, revocations())), principalOf(ctx)))
}

// PrincipalShadow counts a run admitted past a day's cap while budgets.enforcePrincipal is off
// (R3), naming the ledger key over: system:factory, or the person (per-person caps D8). The
// principal label is bounded by the people who may start runs.
func (s *Set) PrincipalShadow(ctx context.Context, principal string) {
	s.revocations.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "budget-principal-shadow"),
		attribute.String("principal", boundPrincipal(principal))))
}

// RunRevoked counts a revocation by the run meter, by reason and the limit that fired.
func (s *Set) RunRevoked(ctx context.Context, reason, limit string) {
	s.revocations.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", oneOf(reason, revocations())),
		attribute.String("limit", oneOf(limit, revocationLimits())), principalOf(ctx)))
}

// RunCost records a priced run's USD cost at its end, in total by tier and by the model that
// served it: the run meter's OnCost.
func (s *Set) RunCost(ctx context.Context, tier string, usd float64, byModel map[string]float64) {
	s.runCost.Record(ctx, usd, metric.WithAttributes(attribute.String("tier", oneOf(tier, tiers()))))
	s.LateCost(ctx, byModel)
}

// LateCost adds what runs spent after their cost was reported to the by-model counter: the run
// meter's OnLateCost.
func (s *Set) LateCost(ctx context.Context, byModel map[string]float64) {
	for model, v := range byModel {
		s.modelCost.Add(ctx, v, metric.WithAttributes(attribute.String("model", boundModel(model))))
	}
}

// RunSpend replaces the per-run spend gauge's values: the run meter's OnSpend, each tick.
func (s *Set) RunSpend(spend map[string]map[string]float64) {
	s.spendMu.Lock()
	defer s.spendMu.Unlock()
	s.spend = spend
}

// registerSpend exposes agent_factory_run_spend_usd{run,model} from the meter's last spend. Only
// the leader meters, so only its gauge has values; a run leaves it once its cost is reported.
func (s *Set) registerSpend(meter metric.Meter) error {
	g, err := meter.Float64ObservableGauge("agent_factory_run_spend_usd",
		metric.WithDescription("A run's USD spend so far, by the model that served it, until the meter reports its cost (agent_factory_run_cost_usd)."))
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		s.spendMu.Lock()
		defer s.spendMu.Unlock()
		for run, by := range s.spend {
			if !runRE.MatchString(run) {
				continue
			}
			bounded := map[string]float64{} // two models bounded to other add up
			for model, v := range by {
				bounded[boundModel(model)] += v
			}
			for model, v := range bounded {
				o.ObserveFloat64(g, v, metric.WithAttributes(attribute.String("run", run), attribute.String("model", model)))
			}
		}
		return nil
	}, g)
	return err
}

// RunSettled replaces the settled gauge's values: the run meter's OnSettled, each tick.
func (s *Set) RunSettled(settled map[string]float64) {
	s.spendMu.Lock()
	defer s.spendMu.Unlock()
	s.settled = settled
}

// registerSettled exposes agent_factory_run_settled_usd{run}: the sum of the run's settled
// entries in the day ledgers, as the objects the meter read and wrote show it. Only the leader
// meters, so only its gauge has values; a run leaves it an hour after it ends. Every id is a C2
// id, or the run is left out.
func (s *Set) registerSettled(meter metric.Meter) error {
	g, err := meter.Float64ObservableGauge("agent_factory_run_settled_usd",
		metric.WithDescription("A run's USD as the day ledgers settled it: the sum of its settled entries, never the meter's own account. Kept an hour after the run ends."))
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		s.spendMu.Lock()
		defer s.spendMu.Unlock()
		for run, v := range s.settled {
			if runRE.MatchString(run) {
				o.ObserveFloat64(g, v, metric.WithAttributes(attribute.String("run", run)))
			}
		}
		return nil
	}, g)
	return err
}

// RunCharges replaces the charge info's runs: the run meter's OnCharges, each tick that listed them.
func (s *Set) RunCharges(live []runs.Run) {
	s.spendMu.Lock()
	defer s.spendMu.Unlock()
	s.live = live
}

// registerCharges exposes agent_factory_run_charge_info{run,principal,task,room} = 1 from the
// meter's last hand-over (spend per identity SI3): the leader's live runs and those ended within
// endSettle, so a panel joins a run's gateway series to its payer. task is absent for a room's
// run, room for a run without one. Every id is a C2 id, or the run is left out.
func (s *Set) registerCharges(meter metric.Meter) error {
	g, err := meter.Int64ObservableGauge("agent_factory_run_charge_info",
		metric.WithDescription("1 for each run the meter charges now or charged within its settle window, naming its payer, its task and its room."))
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		s.spendMu.Lock()
		defer s.spendMu.Unlock()
		for _, r := range s.live {
			if !runRE.MatchString(r.ID) {
				continue
			}
			at := []attribute.KeyValue{attribute.String("run", r.ID), attribute.String("principal", boundPrincipal(r.Payer()))}
			if runRE.MatchString(r.TaskID) {
				at = append(at, attribute.String("task", r.TaskID))
			}
			if runRE.MatchString(r.RoomRef) {
				at = append(at, attribute.String("room", r.RoomRef))
			}
			o.ObserveInt64(g, 1, metric.WithAttributes(at...))
		}
		return nil
	}, g)
	return err
}

func boundModel(model string) string {
	if !modelRE.MatchString(model) {
		return other
	}
	return model
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

// ModelUnpriced counts a run a model served without a price: the run meter's OnUnpriced. The
// model is the gateway's gen_ai_request_model, a name a backend pins; any other shape is other.
func (s *Set) ModelUnpriced(ctx context.Context, model string) {
	s.unpricedModels.Add(ctx, 1, metric.WithAttributes(attribute.String("model", boundModel(model))))
}

// InputExcludesCache counts a run whose input a model reported without its cache tokens: the run
// meter's OnInputExcludesCache. The model is bounded as ModelUnpriced's.
func (s *Set) InputExcludesCache(ctx context.Context, model string) {
	s.excludesCache.Add(ctx, 1, metric.WithAttributes(attribute.String("model", boundModel(model))))
}

// Resumed counts a run the factory resumed on its own, by the lost run's AgentRun reason
// (disruption design §6).
func (s *Set) Resumed(ctx context.Context, reason string) {
	s.resumes.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", oneOf(reason, resumeReasons()))))
}

// LoopRun counts an implementer run under the loop policy once it ends, by its task's class and end.
func (s *Set) LoopRun(ctx context.Context, class, end string) {
	s.loopRuns.Add(ctx, 1, metric.WithAttributes(attribute.String("class", class),
		attribute.String("end", oneOf(end, []string{"judge_impossible", "stalled", "not_met", "finished"})), s.repoOf(ctx)))
}

// ContractEnd counts a tester run's end once the factory has read it (R23).
func (s *Set) ContractEnd(ctx context.Context, class, end string) {
	s.contracts.Add(ctx, 1, metric.WithAttributes(attribute.String("class", class),
		attribute.String("end", oneOf(end, []string{"ready", "not_testable", "contract_not_red", "contract_outside_tests", "contract_invalid", "contract_too_large", "unreported"})), s.repoOf(ctx)))
}

// ClassWithoutHoldout sets agent_factory_class_without_holdout to 1 for a live class with no holdoutChecks (TO5).
func (s *Set) ClassWithoutHoldout(ctx context.Context, repository, class string) {
	s.noHoldout.Record(ctx, 1, metric.WithAttributes(attribute.String("repository", s.repository(repository)),
		attribute.String("class", class)))
}

// SteeringRead counts a task's steering note read at triage, in a scope mode other than off.
func (s *Set) SteeringRead(ctx context.Context, class, read string) {
	s.steeringReads.Add(ctx, 1, metric.WithAttributes(attribute.String("class", class),
		attribute.String("read", oneOf(read, []string{"used", "absent", "invalid", "unowned"})), s.repoOf(ctx)))
}

// ScoperRun counts a scoper run's outcome once it ends.
func (s *Set) ScoperRun(ctx context.Context, class, outcome string) {
	s.scoperRuns.Add(ctx, 1, metric.WithAttributes(attribute.String("class", class),
		attribute.String("outcome", oneOf(outcome, []string{"ready", "question", "assumed", "missing", "failed", "budget", "budget_calls"})), s.repoOf(ctx)))
}

// Split counts a split handoff's outcome once.
func (s *Set) Split(ctx context.Context, class, outcome string) {
	s.splits.Add(ctx, 1, metric.WithAttributes(attribute.String("class", class),
		attribute.String("outcome", oneOf(outcome, []string{"approved", "declined", "superseded", "expired", "ignored", "unstated"})), s.repoOf(ctx)))
}

// SplitChild counts a part's outcome once, when its parent ends.
func (s *Set) SplitChild(ctx context.Context, class, outcome string) {
	s.splitChildren.Add(ctx, 1, metric.WithAttributes(attribute.String("class", class),
		attribute.String("outcome", oneOf(outcome, []string{"landed", "dropped", "reverted"})), s.repoOf(ctx)))
}

// ScopeBounce counts a bounced task's exit.
func (s *Set) ScopeBounce(ctx context.Context, class, exit string) {
	s.scopeBounces.Add(ctx, 1, metric.WithAttributes(attribute.String("class", class),
		attribute.String("exit", oneOf(exit, []string{"relabel", "retry", "expired"})), s.repoOf(ctx)))
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
