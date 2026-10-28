// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/factory/rooms"
	"github.com/Smana/maku/internal/wire"
)

// stored is the rig's task as its client has it, so postFacts can write its status.
func stored(t *testing.T, c client.Client) *v1alpha1.Task {
	t.Helper()
	var tk v1alpha1.Task
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "agent-system", Name: "3buqdlot"}, &tk); err != nil {
		t.Fatal(err)
	}
	return &tk
}

func TestFactsOfATaskWithARunAndAPR(t *testing.T) {
	task := awaiting()
	task.Spec.Repository, task.Spec.Issue, task.Spec.IssueAuthor = "Smana/cloud-native-ref", 2238, "dev1"
	task.Spec.Source.RequestedBy = "github:smana"
	task.Spec.Budget.TaskTokens = 1500000
	task.Status.Usage.Tokens = 189093
	task.Status.PullRequest = &v1alpha1.PullRequestRef{Number: 2239, URL: "https://github.com/Smana/cloud-native-ref/pull/2239",
		Author: "ogenki-agent-factory[bot]", Reviewers: []string{"smana"}}
	task.Status.Runs = []v1alpha1.RunRecord{{ID: "cf4ato2x", Role: "implementer", Trigger: "human"}}
	f := factsOf(task)
	if f.Phase != "AwaitingHuman" || f.Run.ID != "cf4ato2x" || f.Budget.UsedTokens != 189093 ||
		f.Issue.Number != 2238 || f.Issue.Author != "dev1" || f.Issue.LabelledBy != "smana" ||
		f.PR.Number != 2239 || f.PR.Author != "ogenki-agent-factory[bot]" || f.PR.Reviewers[0] != "smana" {
		t.Fatalf("facts %+v", f)
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("invalid: %v", err)
	}
}

// The issue's title is the snapshot's first line ("# title"), bounded; a snapshot without one
// has none.
func TestFactsCarryTheIssueTitle(t *testing.T) {
	task := issueTask("3buqdlot", 7, "x")
	task.Spec.Repository, task.Spec.Text = "Smana/cloud-native-ref", "# docs(agents): runbook 11\n\nthe body"
	if f := factsOf(task); f.Issue.Title != "docs(agents): runbook 11" {
		t.Fatalf("title %q", f.Issue.Title)
	}
	task.Spec.Text = "# " + strings.Repeat("é", 500) + "\n\nbody"
	if f := factsOf(task); len([]rune(f.Issue.Title)) > 121 {
		t.Fatalf("title of %d runes is unbounded", len([]rune(f.Issue.Title)))
	}
	task.Spec.Text = "no heading here"
	if f := factsOf(task); f.Issue.Title != "" {
		t.Fatalf("title %q", f.Issue.Title)
	}
}

// A task with no run, no cap, no issue and no PR has a phase alone; a RunLore task was labelled by
// nobody.
func TestFactsOfABareTask(t *testing.T) {
	task := issueTask("3buqdlot", 0, "x")
	task.Spec.Source.RequestedBy = "system:runlore"
	task.Status.Phase = v1alpha1.PhaseQueued
	if f := factsOf(task); f.Phase != "Queued" || f.Run != nil || f.Budget != nil || f.Issue != nil || f.PR != nil {
		t.Fatalf("facts %+v", f)
	}
	task.Spec.Issue = 7
	if f := factsOf(task); f.Issue == nil || f.Issue.LabelledBy != "" || f.Validate() != nil {
		t.Fatalf("facts %+v", f.Issue)
	}
}

func TestFactsArePostedOncePerChange(t *testing.T) {
	g := newRig(t, awaiting())
	tk := stored(t, g.c)
	ctx := t.Context()
	if err := g.r.postFacts(ctx, tk); err != nil {
		t.Fatal(err)
	}
	if g.r.factsDue(tk) {
		t.Fatal("the same facts are due again")
	}
	tk.Status.Phase = v1alpha1.PhaseReviewing
	if !g.r.factsDue(tk) {
		t.Fatal("a phase change is not due")
	}
	if err := g.r.postFacts(ctx, tk); err != nil {
		t.Fatal(err)
	}
	if got := g.log.factsSeqs; !slices.Equal(got, []int64{1, 2}) || g.log.facts[2].Phase != "Reviewing" {
		t.Fatalf("seqs %v, want [1 2]", got)
	}
	// Ruling SK: each seq is the task's before it is posted.
	if s := stored(t, g.c).Status; s.RoomSeq != 2 || s.Facts == nil || s.Facts.Seq != 2 {
		t.Fatalf("persisted %d %+v", s.RoomSeq, s.Facts)
	}
}

// R12: a meter tick alone never posts. Usage counts in 10% steps of the cap, an overrun is one
// step, and the facts posted still carry the exact count.
func TestTokenUsageIsPostedInTenPercentSteps(t *testing.T) {
	capped := awaiting()
	capped.Spec.Budget.TaskTokens = 1_000_000 // in the stored spec: a status write reads the spec back
	g := newRig(t, capped)
	tk := stored(t, g.c)
	tk.Status.Usage.Tokens = 100_000
	if err := g.r.postFacts(t.Context(), tk); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		tokens int64
		phase  string
		due    bool
	}{
		{"a tick inside the step", 199_999, v1alpha1.PhaseAwaitingHuman, false},
		{"a phase change inside the step", 150_000, v1alpha1.PhaseReviewing, true},
		{"crossing a step", 200_000, v1alpha1.PhaseAwaitingHuman, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tk.Status.Usage.Tokens, tk.Status.Phase = c.tokens, c.phase
			if g.r.factsDue(tk) != c.due {
				t.Fatalf("due %v, want %v", !c.due, c.due)
			}
		})
	}
	if err := g.r.postFacts(t.Context(), tk); err != nil {
		t.Fatal(err)
	}
	if b := g.log.facts[tk.Status.Facts.Seq].Budget; b == nil || b.UsedTokens != 200_000 {
		t.Fatalf("posted %+v, want the exact 200000", b)
	}
	tk.Status.Usage.Tokens = 1_000_000
	if err := g.r.postFacts(t.Context(), tk); err != nil {
		t.Fatal(err)
	}
	if tk.Status.Usage.Tokens = 1_400_000; g.r.factsDue(tk) {
		t.Fatal("an overrun is one step")
	}
}

// Without a cap, usage counts in steps of 100 k tokens.
func TestUncappedUsageStepsAre100k(t *testing.T) {
	f := factsOf(awaiting())
	f.Budget = &envelope.BudgetFact{UsedTokens: 100_000}
	k := factsKey(f)
	if f.Budget.UsedTokens = 199_999; factsKey(f) != k {
		t.Fatal("a tick inside the step changed the key")
	}
	if f.Budget.UsedTokens = 200_000; factsKey(f) == k {
		t.Fatal("crossing a step kept the key")
	}
}

func TestAFailedFactsPostReusesItsSeq(t *testing.T) {
	g := newRig(t, awaiting())
	tk := stored(t, g.c)
	g.log.failFacts = errors.New("broker down")
	if err := g.r.postFacts(t.Context(), tk); err == nil {
		t.Fatal("the failure is swallowed")
	}
	g.log.failFacts = nil
	if err := g.r.postFacts(t.Context(), tk); err != nil {
		t.Fatal(err)
	}
	if got := g.log.factsSeqs; !slices.Equal(got, []int64{1, 1}) || tk.Status.RoomSeq != 1 {
		t.Fatalf("seqs %v roomSeq %d: a retry must resend seq 1", got, tk.Status.RoomSeq)
	}
}

// No log for the room yet, or no permission for the factory yet (FR-1), is a wait as it is for the
// snapshot: no error, and the facts stay due under the seq they took. So is a sealed room, which
// would otherwise hold the task in error backoff for good.
func TestARefusedFactsPostWaits(t *testing.T) {
	for name, refuse := range map[string]func(*fakeLog){
		"no room log":   func(l *fakeLog) { l.noRoom = true },
		"not permitted": func(l *fakeLog) { l.noPermit = true },
		"sealed":        func(l *fakeLog) { l.failFacts = &rooms.APIError{Status: 410, Reason: "sealed"} },
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, awaiting())
			tk := stored(t, g.c)
			refuse(g.log)
			if err := g.r.postFacts(t.Context(), tk); err != nil {
				t.Fatal(err)
			}
			if !g.r.factsDue(tk) || tk.Status.Facts.Seq != 1 || stored(t, g.c).Status.RoomSeq != 1 {
				t.Fatalf("due %v, ledger %+v", g.r.factsDue(tk), tk.Status.Facts)
			}
		})
	}
}

// A v0.7 broker serves no task route: a reasonless 404 or 405 is a wait, said once per process, never
// an error that holds every task in backoff (ruling R28).
func TestAnOlderBrokersMissingRouteWaitsAndSaysSoOnce(t *testing.T) {
	var logs bytes.Buffer
	g := newRig(t, awaiting())
	g.r.Log = slog.New(slog.NewTextHandler(&logs, nil))
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotFound} {
		tk := stored(t, g.c)
		g.log.failFacts = &rooms.APIError{Status: status}
		if err := g.r.postFacts(t.Context(), tk); err != nil {
			t.Fatalf("%d: %v", status, err)
		}
		if !g.r.factsDue(tk) {
			t.Fatalf("%d: the facts are no longer due", status)
		}
	}
	if n := strings.Count(logs.String(), "msg="); n != 1 {
		t.Fatalf("%d log lines, want 1:\n%s", n, logs.String())
	}
}

// Every written phase reaches the room as facts, after the write; the snapshot stays the room's one
// task_state message.
func TestEveryPhaseReachesTheRoom(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	tk := g.reconcile(t, "3buqdlot", 3) // Received → Triaged → Queued → Implementing
	var phases []string
	for _, s := range g.log.factsSeqs {
		phases = append(phases, g.log.facts[s].Phase)
	}
	last := g.log.facts[tk.Status.Facts.Seq]
	if !slices.Equal(phases, []string{"Queued", "Implementing"}) || !tk.Status.Facts.Posted || tk.Status.Facts.Seq != tk.Status.RoomSeq ||
		last.Run == nil || last.Run.ID != rid(0) || last.Issue.Number != 7 || last.Issue.LabelledBy != "Smana" || len(g.log.states) != 1 {
		t.Fatalf("phases %v, ledger %+v, facts %+v, %d task_state", phases, tk.Status.Facts, last, len(g.log.states))
	}
	g.reconcile(t, "3buqdlot", 1)
	if len(g.log.factsSeqs) != 2 {
		t.Fatalf("unchanged facts were posted again: %v", g.log.factsSeqs)
	}
}

// An ended task keeps reconciling until the room has its last facts. A failed post never holds the
// end: it is returned, and retried under the same seq.
func TestAnEndedTaskWritesItsLastFacts(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	tk := g.reconcile(t, "3buqdlot", 2) // Queued
	tk.Annotations = map[string]string{v1alpha1.AnnotationStop: "true"}
	if err := g.c.Update(t.Context(), tk); err != nil {
		t.Fatal(err)
	}
	g.log.failFacts = errors.New("broker down")
	if _, err := g.r.Reconcile(t.Context(), reqFor("3buqdlot")); err == nil {
		t.Fatal("the failed post is not returned")
	}
	if s := stored(t, g.c).Status; s.Phase != v1alpha1.PhaseStopped || s.Facts.Posted || s.Facts.Seq != 2 {
		t.Fatalf("%s %+v", s.Phase, s.Facts)
	}
	g.log.failFacts = nil
	g.reconcile(t, "3buqdlot", 2)
	if got := g.log.factsSeqs; !slices.Equal(got, []int64{1, 2, 2}) || g.log.facts[2].Phase != "Stopped" {
		t.Fatalf("seqs %v, last %+v", got, g.log.facts[2])
	}
}

func TestOutcomeOf(t *testing.T) {
	fin := metav1.NewTime(now)
	tests := []struct {
		name string
		r    v1alpha1.RunRecord
		want string
	}{
		{"succeeded", v1alpha1.RunRecord{Finished: &fin, Phase: "Succeeded"}, envelope.OutcomeSucceeded},
		{"failed", v1alpha1.RunRecord{Finished: &fin, Phase: "Failed"}, envelope.OutcomeFailed},
		{"budget", v1alpha1.RunRecord{Finished: &fin, Phase: "BudgetExhausted"}, envelope.OutcomeBudgetExhausted},
		{"revoked", v1alpha1.RunRecord{Finished: &fin, Phase: "Revoked"}, envelope.OutcomeRevoked},
		{"Disrupted", v1alpha1.RunRecord{Finished: &fin, Phase: "Failed", Reason: "Disrupted"}, envelope.OutcomeLost},
		{"PodLost", v1alpha1.RunRecord{Finished: &fin, Phase: "Failed", Reason: "PodLost"}, envelope.OutcomeLost},
		{"pod_lost", v1alpha1.RunRecord{Finished: &fin, Phase: "Failed", Reason: "pod_lost"}, envelope.OutcomeLost},
		{"run_lost", v1alpha1.RunRecord{Finished: &fin, Phase: "Revoked", Reason: "run_lost"}, envelope.OutcomeLost},
		{"PodFailed is failed", v1alpha1.RunRecord{Finished: &fin, Phase: "Failed", Reason: "PodFailed"}, envelope.OutcomeFailed},
		{"no phase", v1alpha1.RunRecord{Finished: &fin}, envelope.OutcomeEnded},
		{"unfinished", v1alpha1.RunRecord{Phase: "Succeeded", Reason: "PodLost"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := outcomeOf(tc.r); got != tc.want {
				t.Fatalf("outcome %q, want %q", got, tc.want)
			}
		})
	}
}

func runsOf(n int) []v1alpha1.RunRecord {
	var out []v1alpha1.RunRecord
	for i := range n {
		started := metav1.NewTime(now.Add(time.Duration(i) * time.Hour).Add(123 * time.Millisecond))
		out = append(out, v1alpha1.RunRecord{ID: rid(i), Role: "implementer", Trigger: "initial", Started: &started})
	}
	return out
}

func finish(r *v1alpha1.RunRecord, phase string, usd int64, verdict string) {
	end := metav1.NewTime(r.Started.Add(20*time.Minute + 456*time.Millisecond))
	r.Finished, r.Phase, r.CostMicroUSD, r.Verdict = &end, phase, usd, verdict
}

func TestFactsOfCarriesRuns(t *testing.T) {
	task := awaiting()
	task.Status.Runs = runsOf(3)
	finish(&task.Status.Runs[0], "Succeeded", 120_000, "approve")
	finish(&task.Status.Runs[1], "Failed", 80_000, "")
	task.Status.Runs[2].CostMicroUSD = 5_000
	f := factsOf(task)
	if len(f.Runs) != 3 || f.Runs[0].ID != rid(0) || f.Runs[2].ID != rid(2) {
		t.Fatalf("runs %+v", f.Runs)
	}
	r0 := f.Runs[0]
	if r0.MicroUSD != 120_000 || r0.Verdict != "approve" || r0.Outcome != envelope.OutcomeSucceeded ||
		!r0.EndedAt.Equal(task.Status.Runs[0].Finished.UTC().Truncate(time.Second)) || r0.EndedAt.Nanosecond() != 0 {
		t.Fatalf("run 0 %+v", r0)
	}
	if f.Runs[1].Outcome != envelope.OutcomeFailed || f.Runs[2].Outcome != "" || !f.Runs[2].EndedAt.IsZero() || f.Runs[2].MicroUSD != 5_000 {
		t.Fatalf("runs %+v", f.Runs[1:])
	}
	for i, r := range f.Runs {
		if r.Provenance != nil {
			t.Fatalf("run %d carries provenance", i)
		}
	}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
}

// M4: a task that ended without finished() leaves its last run unfinished; its facts say it ended, as
// the task did, rather than that it runs on.
func TestFactsOfEndedTaskEndsItsUnfinishedRun(t *testing.T) {
	for _, phase := range []string{v1alpha1.PhaseStopped, v1alpha1.PhaseEscalated, v1alpha1.PhaseClosed} {
		t.Run(phase, func(t *testing.T) {
			task := awaiting()
			task.Status.Runs = runsOf(2)
			finish(&task.Status.Runs[0], "Succeeded", 1, "")
			task.Status.Phase = phase
			since := metav1.NewTime(task.Status.Runs[1].Started.Add(10 * time.Minute))
			task.Status.PhaseSince = &since
			f := factsOf(task)
			if r := f.Runs[1]; r.Outcome != envelope.OutcomeEnded || !r.EndedAt.Equal(since.UTC().Truncate(time.Second)) {
				t.Fatalf("unfinished run %+v", r)
			}
			if f.Run.Outcome != envelope.OutcomeEnded {
				t.Fatalf("current run %+v", f.Run)
			}
			if f.Runs[0].Outcome != envelope.OutcomeSucceeded {
				t.Fatalf("finished run changed: %+v", f.Runs[0])
			}
			if err := f.Validate(); err != nil {
				t.Fatal(err)
			}
			task.Status.Phase = v1alpha1.PhaseReviewing
			if f := factsOf(task); f.Runs[1].Outcome != "" {
				t.Fatalf("a live task's run ended: %+v", f.Runs[1])
			}
		})
	}
}

func TestFactsOfKeepsTheLast20Runs(t *testing.T) {
	task := awaiting()
	task.Status.Runs = runsOf(25)
	f := factsOf(task)
	if len(f.Runs) != envelope.MaxRuns || f.Runs[0].ID != rid(5) || f.Runs[19].ID != rid(24) {
		t.Fatalf("%d runs, first %s last %s", len(f.Runs), f.Runs[0].ID, f.Runs[19].ID)
	}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestFactsOfSpend(t *testing.T) {
	task := awaiting()
	if f := factsOf(task); f.Spend != nil {
		t.Fatalf("spend %+v with neither cap nor usage", f.Spend)
	}
	task.Spec.Budget.TaskMicroUSD, task.Status.Usage.CostMicroUSD = 1_500_000, 310_000
	if f := factsOf(task); f.Spend == nil || *f.Spend != (envelope.SpendFact{UsedMicroUSD: 310_000, LimitMicroUSD: 1_500_000}) {
		t.Fatalf("spend %+v", f.Spend)
	}
	task.Spec.Budget.TaskMicroUSD = 0
	if f := factsOf(task); f.Spend == nil || *f.Spend != (envelope.SpendFact{UsedMicroUSD: 310_000}) {
		t.Fatalf("uncapped spend %+v", f.Spend)
	}
	task.Spec.Budget.TaskMicroUSD, task.Status.Usage.CostMicroUSD = 1_500_000, 0
	if f := factsOf(task); f.Spend == nil || f.Spend.UsedMicroUSD != 0 || f.Spend.LimitMicroUSD != 1_500_000 {
		t.Fatalf("a cap alone: %+v", f.Spend)
	}
	task.Spec.Budget.TaskTokens = 0
	if f := factsOf(task); f.Budget != nil {
		t.Fatalf("budget %+v without a token cap", f.Budget)
	}
}

func TestFactsKeyStepsDollars(t *testing.T) {
	key := func(used, limit int64) string {
		f := factsOf(awaiting())
		f.Spend = &envelope.SpendFact{UsedMicroUSD: used, LimitMicroUSD: limit}
		return factsKey(f)
	}
	if key(310_000, 1_500_000) != key(340_000, 1_500_000) {
		t.Fatal("a tick inside a 10% step changed the key")
	}
	if key(310_000, 1_500_000) == key(460_000, 1_500_000) {
		t.Fatal("crossing a step kept the key")
	}
	if key(10_000, 0) != key(90_000, 0) {
		t.Fatal("an uncapped tick inside $0.10 changed the key")
	}
	if key(10_000, 0) == key(110_000, 0) {
		t.Fatal("crossing $0.10 kept the key")
	}
	runKey := func(usd int64, ended bool) string {
		f := factsOf(awaiting())
		f.Runs = []envelope.RunFact{{ID: rid(0), Role: "implementer", MicroUSD: usd}}
		if ended {
			f.Runs[0].EndedAt = now
		}
		return factsKey(f)
	}
	if runKey(10_000, false) != runKey(80_000, false) {
		t.Fatal("a running run's dollars moved the key")
	}
	if runKey(10_000, true) == runKey(80_000, true) {
		t.Fatal("an ended run's dollars are not in the key")
	}
}

// A broker older than the factory refuses the new fields with a 400 (its decoder is strict): the
// facts stay due under their seq, and go through once the broker is rolled.
func TestFactsRefusedByAnOlderBrokerStayDue(t *testing.T) {
	g := newRig(t, awaiting())
	tk := stored(t, g.c)
	g.log.failFacts = &rooms.APIError{Status: 400, Reason: wire.ReasonBadMessage}
	if err := g.r.postFacts(t.Context(), tk); err == nil {
		t.Fatal("the refusal is swallowed")
	}
	if !g.r.factsDue(tk) {
		t.Fatal("refused facts are no longer due")
	}
	g.log.failFacts = nil
	if err := g.r.postFacts(t.Context(), tk); err != nil {
		t.Fatal(err)
	}
	if got := g.log.factsSeqs; !slices.Equal(got, []int64{1, 1}) {
		t.Fatalf("seqs %v, want [1 1]", got)
	}
}

// An adopted Task has no room: no facts are ever due.
func TestAnAdoptedTaskHasNoFactsDue(t *testing.T) {
	task := awaiting()
	task.Spec.Source.Kind, task.Status.RoomRef = "adopt", ""
	g := newRig(t, task)
	if g.r.factsDue(stored(t, g.c)) {
		t.Fatal("an adopted task has facts due")
	}
}

// A run superseded by a later one is over, whatever the task's phase (a retry from Escalated leaves
// the stuck run's record unfinished): it ended when the next began. The last run is live.
func TestFactsOfEndsASupersededUnfinishedRun(t *testing.T) {
	task := awaiting()
	task.Status.Runs = runsOf(3)
	task.Status.Phase = v1alpha1.PhaseImplementing
	f := factsOf(task)
	for i := range 2 {
		next := task.Status.Runs[i+1].Started.UTC().Truncate(time.Second)
		if r := f.Runs[i]; r.Outcome != envelope.OutcomeEnded || !r.EndedAt.Equal(next) {
			t.Fatalf("run %d %+v, want ended at %v", i, r, next)
		}
	}
	if r := f.Runs[2]; r.Outcome != "" || !r.EndedAt.IsZero() {
		t.Fatalf("the last run is not live: %+v", r)
	}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
}

// Dollars past envelope.MaxMicroUSD are clamped, so the facts still validate and the room keeps its
// card; a refused post would leave it with none.
func TestFactsClampDollarsToTheCap(t *testing.T) {
	task := issueTask("3buqdlot", 0, "x")
	task.Status.Phase = v1alpha1.PhaseImplementing
	task.Spec.Budget.TaskMicroUSD = envelope.MaxMicroUSD + 5
	task.Status.Usage.CostMicroUSD = envelope.MaxMicroUSD + 7
	task.Status.Runs = []v1alpha1.RunRecord{{ID: "r1", Role: "implementer", CostMicroUSD: envelope.MaxMicroUSD + 9}}
	f := factsOf(task)
	if err := f.Validate(); err != nil {
		t.Fatalf("facts refused: %v", err)
	}
	if f.Spend == nil || f.Spend.UsedMicroUSD != envelope.MaxMicroUSD || f.Spend.LimitMicroUSD != envelope.MaxMicroUSD {
		t.Errorf("spend %+v, want both clamped", f.Spend)
	}
	if len(f.Runs) == 0 || f.Runs[len(f.Runs)-1].MicroUSD != envelope.MaxMicroUSD {
		t.Errorf("runs %+v, want the run's dollars clamped", f.Runs)
	}
}

// RA10: a fix task's facts carry its spec.origin; any other task's carry none.
func TestFactsCarryTheOrigin(t *testing.T) {
	task := issueTask("3buqdlot", 7, "x")
	task.Status.Phase = v1alpha1.PhaseQueued
	if f := factsOf(task); f.Origin != "" {
		t.Fatalf("%+v", f)
	}
	task.Spec.Origin = "abcdefgh"
	if f := factsOf(task); f.Origin != "abcdefgh" || f.Validate() != nil {
		t.Fatalf("%+v", f)
	}
}
