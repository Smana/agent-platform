// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/api"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/fmetrics"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/meter"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

func TestQueuesAndReviewerTier(t *testing.T) {
	g := pairRig(t)
	var tk v1alpha1.Task
	_ = g.c.Get(t.Context(), client.ObjectKey{Namespace: "agent-system", Name: "3buqdlot"}, &tk)
	if got := g.runs.specs[rid(0)].Queue; got != runs.QueueFactory {
		t.Fatalf("factory runs are admitted by Kueue's factory queue: %q", got)
	}
	s := g.runs.specs[rid(1)]
	if s.Model != g.r.Cfg.Tiers["frontier"].Model || s.MaxTokens != tk.Spec.Budget.RunTokens {
		t.Fatalf("the reviewer uses another tier's model and the task's run budget: %+v", s)
	}
}

func TestWIPHoldsReviewClassWork(t *testing.T) {
	var waiting []client.Object
	for _, n := range []string{"aaaaaaaa", "bbbbbbbb"} {
		o := issueTask(n, 1, "x")
		o.Status.Phase = v1alpha1.PhaseAwaitingHuman
		waiting = append(waiting, o)
	}
	g := newRig(t, append(waiting, issueTask("3buqdlot", 7, "x"))...)
	g.r.Cfg.Caps.AwaitingHumanWIP = 2
	tk := g.reconcile(t, "3buqdlot", 3)
	if tk.Status.Phase != v1alpha1.PhaseQueued || tk.Status.Reason != "waiting_review_wip" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

// The task cap is checked at admission, before every run of the task: a run starts only while one
// whole RunTokens still fits under TaskTokens (owner, 2026-10-07), the rule a resume follows too.
// Standard tier: 1.5 M per run, 3 M per task. Two paths into a run: a maintainer's revision and the
// pair's reviewer, the run a 3.35 M task kept starting while the cap was shadow (aws-0, 2026-10-06).
func TestTaskTokenCapLeavesRoomForAWholeRun(t *testing.T) {
	paths := map[string]func(t *testing.T, used int64, enforce bool) (*rig, *v1alpha1.Task){
		"revision": func(t *testing.T, used int64, enforce bool) (*rig, *v1alpha1.Task) {
			tk := awaiting()
			tk.Status.Phase, tk.Status.NextTrigger = v1alpha1.PhaseQueued, "human"
			tk.Spec.Budget.TaskTokens = 3_000_000
			// observe rebuilds the total from the records (R49); a record keeps its last reading
			// when its claim is gone.
			tk.Status.Runs[0].Tokens = used
			g := newRig(t, tk, roomOf("3buqdlot"))
			g.f.SetPR(pr12()) // Queued re-reads the task's pull request (lateReviews) first
			g.r.Cfg.Budgets.EnforceTask = enforce
			return g, g.reconcile(t, "3buqdlot", 1)
		},
		"reviewer": func(t *testing.T, used int64, enforce bool) (*rig, *v1alpha1.Task) {
			g := newRig(t, issueTask("3buqdlot", 7, "x"))
			g.r.Triage = staticWith("pair")
			g.r.Cfg.Budgets.EnforceTask = enforce
			g.reconcile(t, "3buqdlot", 3)
			g.f.SetBranch("agent/3buqdlot", 12)
			g.f.SetPR(pr12At(head1))
			r := g.runs.runs[rid(0)]
			r.Tokens = used
			g.runs.runs[rid(0)] = r
			g.finish(rid(0), "Succeeded", "agent_finished")
			return g, g.reconcile(t, "3buqdlot", 2) // → Queued for the reviewer → admitted or not
		},
	}
	for path, start := range paths {
		for name, c := range map[string]struct {
			used    int64
			enforce bool
			started bool
		}{
			"a run's worth left":                  {1_500_000, true, true},
			"less than a run's worth left":        {1_500_001, true, false},
			"in shadow, counted but not enforced": {1_500_001, false, true},
		} {
			t.Run(path+"/"+name, func(t *testing.T) {
				g, tk := start(t, c.used, c.enforce)
				_, started := g.runs.specs[rid(1)]
				if started != c.started {
					t.Fatalf("run started %v, want %v: %s %s", started, c.started, tk.Status.Phase, tk.Status.Reason)
				}
				shadow := slices.Contains(g.metrics.recorded, "revoked budget-task-shadow")
				if shadow != (c.started && c.used > 1_500_000) {
					t.Fatalf("shadow counted %v: %q", shadow, g.metrics.recorded)
				}
				if c.started {
					return
				}
				if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != "budget-task" {
					t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
				}
				if say := strings.Join(g.f.Comments(7), "\n"); !strings.Contains(say, "no room left for another full run") ||
					!strings.Contains(say, "Tokens used: 1.5 M") {
					t.Fatalf("the escalation says why, honestly: %q", say)
				}
			})
		}
	}
}

// A task queued for its reviewer whose pull request was merged meanwhile needs no run: the late
// review path ends it, and the task cap, which only guards a run's start, never escalates it.
func TestAMergedPullRequestIsNotEscalatedForTheTaskCap(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	g.r.Triage = staticWith("pair")
	g.r.Cfg.Budgets.EnforceTask = true
	g.reconcile(t, "3buqdlot", 3)
	g.f.SetBranch("agent/3buqdlot", 12)
	g.f.SetPR(pr12At(head1))
	r := g.runs.runs[rid(0)]
	r.Tokens = 1_500_001 // less than a run's worth left
	g.runs.runs[rid(0)] = r
	g.finish(rid(0), "Succeeded", "agent_finished")
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseQueued || tk.Status.NextRole != "reviewer" {
		t.Fatalf("queued for the reviewer: %s %q", tk.Status.Phase, tk.Status.NextRole)
	}
	g.f.SetPR(forge.PR{Number: 12, State: "MERGED", MergedBy: "Smana", HeadSHA: head1})
	// Not even for one reconcile: Escalated would itself find the merge, but only after pinging
	// the maintainers about a budget the task no longer needs.
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
	tk := g.reconcile(t, "3buqdlot", 2)
	if tk.Status.Phase != v1alpha1.PhaseDone || len(g.runs.specs) != 1 {
		t.Fatalf("%s %s, %d runs", tk.Status.Phase, tk.Status.Reason, len(g.runs.specs))
	}
	if say := strings.Join(g.f.Comments(7), "\n"); strings.Contains(say, "no room left") || strings.Contains(say, "needs a maintainer") {
		t.Fatalf("no escalation was narrated: %q", say)
	}
}

// A maintainer's "Request changes" on a task with no room left for a run escalates it as
// budget-task without first announcing a revision that cannot start; the review is still queued
// in the room for whoever picks the task up. In shadow the revision is announced and starts.
// Two ways in: the review lands while the task awaits a human, or while it is queued for its
// reviewer.
func TestARequestChangesPastTheCapIsQueuedNotAnnounced(t *testing.T) {
	review := forge.Review{ID: 901, Author: "Smana", State: "CHANGES_REQUESTED", Body: "Use the relative link.", At: now.Add(time.Minute)}
	paths := map[string]func(t *testing.T, enforce bool) (*rig, *v1alpha1.Task){
		"awaiting a human": func(t *testing.T, enforce bool) (*rig, *v1alpha1.Task) {
			tk := awaiting()
			tk.Spec.Budget.TaskTokens = 3_000_000
			tk.Status.Runs[0].Tokens, tk.Status.Usage.Tokens = 1_500_001, 1_500_001
			g := newRig(t, tk, roomOf("3buqdlot"))
			g.f.SetPR(pr12(review))
			g.r.Cfg.Budgets.EnforceTask = enforce
			return g, g.reconcile(t, "3buqdlot", 2) // → Queued → admitted or not
		},
		"queued for its reviewer": func(t *testing.T, enforce bool) (*rig, *v1alpha1.Task) {
			g := newRig(t, issueTask("3buqdlot", 7, "x"))
			g.r.Triage = staticWith("pair")
			g.r.Cfg.Budgets.EnforceTask = enforce
			g.reconcile(t, "3buqdlot", 3)
			g.f.SetBranch("agent/3buqdlot", 12)
			g.f.SetPR(pr12At(head1))
			r := g.runs.runs[rid(0)]
			r.Tokens = 1_500_001
			g.runs.runs[rid(0)] = r
			g.finish(rid(0), "Succeeded", "agent_finished")
			g.reconcile(t, "3buqdlot", 1) // → Queued for the reviewer
			pr := pr12At(head1)
			pr.Reviews = []forge.Review{review}
			g.f.SetPR(pr)
			return g, g.reconcile(t, "3buqdlot", 1)
		},
	}
	for path, start := range paths {
		for name, enforce := range map[string]bool{"enforced": true, "shadow": false} {
			t.Run(path+"/"+name, func(t *testing.T) {
				g, tk := start(t, enforce)
				if !slices.Equal(g.log.reviews(), []int64{901}) {
					t.Fatalf("the review is queued in the room either way: %v", g.log.reviews())
				}
				say := strings.Join(g.f.Comments(7), "\n")
				announced := strings.Contains(say, "is revising after @Smana's review")
				_, started := g.runs.specs[rid(1)]
				if enforce {
					if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != "budget-task" || announced || started {
						t.Fatalf("%s %s, started %v: %q", tk.Status.Phase, tk.Status.Reason, started, say)
					}
					return
				}
				if !announced || !started || g.runs.specs[rid(1)].Role != "implementer" {
					t.Fatalf("in shadow the revision is announced and starts: %s, started %v: %q", tk.Status.Phase, started, say)
				}
			})
		}
	}
}

// factoryLedger is today's R50 ledger (now's UTC day) with system:factory's spend, as the meter
// settles it, and reservations keyed by run id.
func factoryLedger(spent int64, reserved map[string]int64) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: api.LedgerPrefix + now.UTC().Format(api.LedgerDayLayout),
		Namespace: "agent-system"}, Data: map[string]string{}}
	if err := api.SetLedgerSpentColumn(cm, map[string]int64{runs.PrincipalFactory: spent}); err != nil {
		panic(err)
	}
	for id, n := range reserved {
		cm.Data[api.LedgerReserved+id] = fmt.Sprintf(`{"principal":%q,"maxTokens":%d}`, runs.PrincipalFactory, n)
	}
	return cm
}

// R34: the factory's own day is checked before every run it starts, not only by the meter, and
// read from the day's ledger the meter and the API use (N2), never from the live claims.
func TestFactoryDailyBudget(t *testing.T) {
	budgets := config.Budgets{FactoryDaily: 25_000_000, HumanDaily: 5_000_000}
	g := newRig(t, issueTask("3buqdlot", 7, "x"), factoryLedger(25_000_000, nil))
	g.r.Cfg.Budgets = budgets
	if tk := g.reconcile(t, "3buqdlot", 3); tk.Status.Phase != v1alpha1.PhaseImplementing ||
		!slices.Contains(g.metrics.recorded, "revoked budget-principal-shadow") {
		t.Fatalf("in shadow the day is counted, not enforced (R3): %s %q", tk.Status.Phase, g.metrics.recorded)
	}
	budgets.EnforcePrincipal = true
	g = newRig(t, issueTask("3buqdlot", 7, "x"), factoryLedger(25_000_000, nil))
	g.r.Cfg.Budgets = budgets
	if tk := g.reconcile(t, "3buqdlot", 3); tk.Status.Phase != v1alpha1.PhaseQueued || tk.Status.Reason != "waiting_daily_budget" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
	g.r.Now = func() time.Time { return now.Add(24 * time.Hour) }
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseImplementing {
		t.Fatalf("a new UTC day, a new ledger: %s", tk.Status.Phase)
	}
}

// N2: a run's spend stays in the day once its claim is deleted (a stop deletes every claim): the
// ledger refunds nothing, and the live claims are no measure of the day.
func TestDeletedRunsSpendStaysInTheDay(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"), factoryLedger(24_000_000, nil))
	g.r.Cfg.Budgets = config.Budgets{FactoryDaily: 25_000_000, EnforcePrincipal: true}
	// No claim lists any of the 24 M: every run that spent them was deleted.
	if tk := g.reconcile(t, "3buqdlot", 3); tk.Status.Phase != v1alpha1.PhaseQueued || tk.Status.Reason != "waiting_daily_budget" {
		t.Fatalf("24 M spent and a 1.5 M run do not fit 25 M: %s %s", tk.Status.Phase, tk.Status.Reason)
	}
	g = newRig(t, issueTask("3buqdlot", 7, "x"))
	g.r.Cfg.Budgets = config.Budgets{FactoryDaily: 25_000_000, EnforcePrincipal: true}
	g.runs.runs["zzzzzzzz"] = runs.Run{ID: "zzzzzzzz", Principal: runs.PrincipalFactory, Phase: "Succeeded",
		Tokens: 25_000_000, Created: now.Add(-time.Hour)}
	if tk := g.reconcile(t, "3buqdlot", 3); tk.Status.Phase != v1alpha1.PhaseImplementing {
		t.Fatalf("a claim's tokens count once the meter settles them in the ledger: %s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

// factoryToken authenticates every request as the factory's own principal.
type factoryToken struct{}

func (factoryToken) Authenticate(*http.Request) (authn.Principal, error) {
	return authn.Principal{Kind: envelope.ActorSystem, ID: runs.PrincipalFactory}, nil
}

type noRuns struct{}

func (noRuns) Create(context.Context, runs.Spec) error  { return nil }
func (noRuns) List(context.Context) ([]runs.Run, error) { return nil, nil }

// N2: Queued and the API decide the factory's day on one reading of one ledger: for each day,
// the API's answer to a run of the task's size and Queued's agree.
func TestQueuedAndTheAPIDecideTheDayAlike(t *testing.T) {
	m, err := fmetrics.New(nil, nil, "agent-system", func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		l      *corev1.ConfigMap
		refuse bool
	}{
		"empty":                 {factoryLedger(0, nil), false},
		"room for one run":      {factoryLedger(23_500_000, nil), false},
		"no room for a run":     {factoryLedger(23_600_000, nil), true},
		"spent":                 {factoryLedger(25_000_000, nil), true},
		"reservations count":    {factoryLedger(20_000_000, map[string]int64{"aaaaaaaa": 2_000_000, "bbbbbbbb": 2_000_000}), true},
		"reservations leave it": {factoryLedger(20_000_000, map[string]int64{"aaaaaaaa": 2_000_000}), false},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, issueTask("3buqdlot", 7, "x"), c.l.DeepCopy())
			g.r.Cfg.Budgets = config.Budgets{FactoryDaily: 25_000_000, HumanDaily: 5_000_000, EnforcePrincipal: true}
			tk := g.reconcile(t, "3buqdlot", 3)
			queued := tk.Status.Reason == "waiting_daily_budget"

			cf := cfg()
			cf.Budgets = g.r.Cfg.Budgets
			cf.API.Repositories = []string{"Smana/cloud-native-ref"}
			srv := &api.Server{Auth: factoryToken{}, Cfg: cf, Runs: noRuns{},
				Rooms: fake.NewClientBuilder().WithScheme(scheme()).WithObjects(c.l.DeepCopy()).Build(), Namespace: "agent-system",
				Stopped: func(context.Context) bool { return false }, NewRunID: func() string { return "7f3cq2xz" },
				Now: func() time.Time { return now }, Metrics: m}
			body := fmt.Sprintf(`{"role":"implementer","repository":"Smana/cloud-native-ref","task":{"text":"x"},"dataClass":"public","maxTokens":%d}`,
				tk.Spec.Budget.RunTokens)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "POST", "/v1/runs", strings.NewReader(body)))
			byAPI := w.Code == http.StatusTooManyRequests
			if queued != c.refuse || byAPI != c.refuse {
				t.Fatalf("want refused %v: Queued %v (%s %s), the API %v (%d %s)", c.refuse, queued, tk.Status.Phase,
					tk.Status.Reason, byAPI, w.Code, w.Body)
			}
		})
	}
}

// ledgerOf reads today's ledger: system:factory's use and its reservations by run id.
func ledgerOf(t *testing.T, g *rig) (int64, []string) {
	t.Helper()
	used, err := api.DayUsed(t.Context(), g.r.Ledger, "agent-system", runs.PrincipalFactory, g.r.Now())
	if err != nil {
		t.Fatal(err)
	}
	var cm corev1.ConfigMap
	if err := g.r.Ledger.Get(t.Context(), client.ObjectKey{Namespace: "agent-system",
		Name: api.LedgerPrefix + g.r.Now().UTC().Format(api.LedgerDayLayout)}, &cm); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for k := range cm.Data {
		if id, ok := strings.CutPrefix(k, api.LedgerReserved); ok {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return used, ids
}

// #78: Queued reserves each run it starts in the day's ledger before creating its claim, so
// concurrentRuns' worth of runs admitted on one reading cannot overshoot an enforced day: with
// room for two 1.5 M runs, two of four tasks start and two wait.
func TestFactoryRunsAreReservedBeforeTheyStart(t *testing.T) {
	names := []string{"3buqdlot", "4buqdlot", "5buqdlot", "6buqdlot"}
	objs := []client.Object{factoryLedger(22_000_000, nil)}
	for i, n := range names {
		objs = append(objs, issueTask(n, 7+i, "x"))
	}
	g := newRig(t, objs...)
	g.r.Cfg.Caps.ActiveTasks = 4
	g.r.Cfg.Budgets = config.Budgets{FactoryDaily: 25_000_000, HumanDaily: 5_000_000, EnforcePrincipal: true}
	started, waiting := 0, 0
	for _, n := range names {
		switch tk := g.reconcile(t, n, 3); {
		case tk.Status.Phase == v1alpha1.PhaseImplementing:
			started++
		case tk.Status.Phase == v1alpha1.PhaseQueued && tk.Status.Reason == "waiting_daily_budget":
			waiting++
		default:
			t.Fatalf("%s: %s %s", n, tk.Status.Phase, tk.Status.Reason)
		}
	}
	if started != 2 || waiting != 2 {
		t.Fatalf("22 M spent, a 25 M day, 1.5 M runs: %d started, %d wait", started, waiting)
	}
	used, ids := ledgerOf(t, g)
	if used != 25_000_000 || len(ids) != 2 {
		t.Fatalf("the two started runs are reserved: used %d, reservations %v", used, ids)
	}
	for _, id := range ids {
		if _, ok := g.runs.runs[id]; !ok {
			t.Fatalf("reservation %s names no claim: %v", id, g.runs.runs)
		}
	}
}

// R3: in shadow the factory's run is still reserved, and an over day only counts.
func TestShadowReservesAndNeverHolds(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"), factoryLedger(25_000_000, nil))
	g.r.Cfg.Budgets = config.Budgets{FactoryDaily: 25_000_000, HumanDaily: 5_000_000}
	if tk := g.reconcile(t, "3buqdlot", 3); tk.Status.Phase != v1alpha1.PhaseImplementing ||
		!slices.Contains(g.metrics.recorded, "revoked budget-principal-shadow") {
		t.Fatalf("shadow counts, never holds: %s %s %q", tk.Status.Phase, tk.Status.Reason, g.metrics.recorded)
	}
	if used, ids := ledgerOf(t, g); used != 26_500_000 || !slices.Equal(ids, []string{rid(0)}) {
		t.Fatalf("shadow still records the run: used %d, reservations %v", used, ids)
	}
	// A ledger that cannot be read holds nothing in shadow either.
	g = newRig(t, issueTask("3buqdlot", 7, "x"))
	g.r.Cfg.Budgets = config.Budgets{FactoryDaily: 25_000_000, HumanDaily: 5_000_000}
	g.r.Ledger = brokenLedger{g.c}
	if tk := g.reconcile(t, "3buqdlot", 3); tk.Status.Phase != v1alpha1.PhaseImplementing {
		t.Fatalf("an unreadable ledger in shadow: %s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

// brokenLedger fails every ledger read and write.
type brokenLedger struct{ client.Client }

func (brokenLedger) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("apiserver down")
}

func (brokenLedger) Update(context.Context, client.Object, ...client.UpdateOption) error {
	return errors.New("apiserver down")
}

func (brokenLedger) Create(context.Context, client.Object, ...client.CreateOption) error {
	return errors.New("apiserver down")
}

// meterStore is the rig's claims as the meter sees them.
type meterStore struct{ *fakeRuns }

func (meterStore) Annotate(context.Context, string, map[string]string) error { return nil }

type meterSource map[string]int64

func (s meterSource) RunTokens(context.Context) (map[string]int64, error) { return s, nil }

// #78: the reservation's key is the run's deterministic id (R48), so a replay after a lost write
// reserves nothing twice, and the meter that settles the run drops it once: a meter restarted
// after the drop finds nothing left to drop, and the day holds the run's spend once.
func TestAFactoryReservationSettlesOnceAcrossRestarts(t *testing.T) {
	c := losingClient(t, func(tk *v1alpha1.Task) bool { return len(tk.Status.Runs) > 0 })
	g := newRig(t)
	g.c, g.r.Client = c, c // g.r.Ledger keeps the rig's own client: the ledger is not the Task's
	g.r.Cfg.Budgets = config.Budgets{FactoryDaily: 25_000_000, HumanDaily: 5_000_000, EnforcePrincipal: true}
	g.reconcile(t, "3buqdlot", 2) // Queued
	if _, err := g.r.Reconcile(t.Context(), reqFor("3buqdlot")); err == nil {
		t.Fatal("the write recording the run is lost")
	}
	if used, ids := ledgerOf(t, g); used != 1_500_000 || !slices.Equal(ids, []string{rid(0)}) {
		t.Fatalf("reserved before the claim: used %d, %v", used, ids)
	}
	r := g.runs.runs[rid(0)]
	r.Phase = "Failed"
	g.runs.runs[rid(0)] = r
	g.log.end(rid(0), "Failed", "pod_lost")
	g.reconcile(t, "3buqdlot", 2) // the replay records the claim
	if used, ids := ledgerOf(t, g); used != 1_500_000 || len(ids) != 1 {
		t.Fatalf("the replay reserves nothing twice: used %d, %v", used, ids)
	}
	for i, tokens := range []int64{0, 900} { // the second meter is a new leader: the first annotated 900
		r := g.runs.runs[rid(0)]
		r.Tokens = tokens
		g.runs.runs[rid(0)] = r
		m := &meter.Meter{Runs: meterStore{g.runs}, Source: meterSource{rid(0): 900}, Ledger: g.r.Ledger,
			Namespace: "agent-system", Now: g.r.Now}
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		if used, ids := ledgerOf(t, g); used != 900 || len(ids) != 0 {
			t.Fatalf("meter %d: the run's spend once, no reservation: used %d, %v", i, used, ids)
		}
	}
}
