// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

const mSHA = "aaaa000000000000000000000000000000000001"

// MonitorVerdict records a verdict the reconciler measured.
func (m *fakeMetrics) MonitorVerdict(_ context.Context, class, verdict string) {
	m.add("monitor " + class + " " + verdict)
}

// setDone ends the rig's task merged at PR n as sha, by who, at the given time: phase Done, reason
// merged, and the fake's pull request MERGED with the same time (the monitor reads GitHub's, L6).
func (g *rig) setDone(t *testing.T, name string, n int, who, sha string, at time.Time) {
	t.Helper()
	g.patch(t, func(tk *v1alpha1.Task) {
		ma := metav1.NewTime(at)
		tk.Status.Phase, tk.Status.Reason = v1alpha1.PhaseDone, "merged"
		tk.Status.PullRequest.Number, tk.Status.PullRequest.MergedBy = n, who
		tk.Status.PullRequest.MergeCommitSHA, tk.Status.PullRequest.MergedAt = sha, &ma
	})
	g.f.SetPR(forge.PR{Number: n, NodeID: "PR_" + strconv.Itoa(n), State: "MERGED", Title: "docs: fix a link", Author: "ogenki-agents[bot]",
		HeadRef: "agent/" + name, MergedBy: who, MergedAt: at, MergeCommitSHA: sha})
}

// monitorRig: task 3buqdlot of a monitored class, merged by a maintainer 2 h ago at PR 12 as
// mSHA, which changed website/content/docs/a.md line 7.
func monitorRig(t *testing.T) *rig {
	g := declaredRig(t)
	cl := g.r.Cfg.Classes["dead-links"]
	cl.Monitor = &config.Monitor{FollowUpWindow: config.Duration{Duration: 168 * time.Hour}}
	g.r.Cfg.Classes["dead-links"] = cl
	g.r.Cfg.Merge.Breaker = config.Breaker{Window: 10, MaxReverts: 1, MaxFollowUps: 1}
	if g.r.Cfg.ControlFor().Issue == 0 {
		g.r.Cfg.Control = config.Control{Repository: "Smana/cloud-native-ref", Issue: 3}
	}
	g.setDone(t, "3buqdlot", 12, "Smana", mSHA, now.Add(-2*time.Hour))
	g.f.SetPullFiles(12, forge.FilePatch{Path: testFile, Status: "modified", Patch: "@@ -7 +7 @@\n-[x](https://a)\n+[x](https://b)\n"})
	g.f.SetCommitChecks(mSHA, green("SUCCESS").Runs...)
	return g
}

func later(n int, author, branch string, labels ...string) forge.MergedPull {
	return forge.MergedPull{Number: n, Author: author, HeadRef: branch, MergeCommitSHA: "bbbb" + string(rune('0'+n%10)),
		MergedAt: now.Add(-time.Hour), Labels: labels}
}

var mBlame = []forge.BlameRange{{Start: 1, End: 6, Commit: "old"}, {Start: 7, End: 7, Commit: mSHA}}

// the later pull request's retarget of the line m wrote
var overlapping = forge.FilePatch{Path: testFile, Status: "modified", Patch: "@@ -7 +7 @@\n-[x](https://b)\n+[x](https://c)\n"}

// Offline proof 4: an overlapping later PR is a follow-up and gets the label; one elsewhere is
// not; a maintainer's label counts. Renovate's next bump (an adopt login of the class) is not:
// that case joins this table in Task 16, once config.Author exists.
func TestFollowUps(t *testing.T) {
	for name, c := range map[string]struct {
		pr     forge.MergedPull
		files  []forge.FilePatch
		blame  []forge.BlameRange
		follow bool
	}{
		"changes a line m wrote": {later(20, "Smana", "fix"), []forge.FilePatch{overlapping}, mBlame, true},
		"changes another line": {later(21, "Smana", "fix"), []forge.FilePatch{{Path: testFile, Status: "modified",
			Patch: "@@ -2 +2 @@\n-a\n+b\n"}}, mBlame, false},
		"another file":         {later(22, "Smana", "fix"), []forge.FilePatch{{Path: "docs/b.md", Status: "modified", Patch: "@@ -7 +7 @@\n-a\n+b\n"}}, nil, false},
		"a maintainer's label": {later(24, "Smana", "x", FollowUpOfLabel(12)), nil, nil, true},
		"m's revert": {later(25, "ogenki-agent-merger[bot]", "revert-12", RevertOfLabel(12)), []forge.FilePatch{{Path: testFile,
			Status: "modified", Patch: "@@ -7 +7 @@\n-b\n+a\n"}}, []forge.BlameRange{{Start: 7, End: 7, Commit: mSHA}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			g := monitorRig(t)
			g.f.SetMerged(c.pr)
			g.f.SetPullFiles(c.pr.Number, c.files...)
			g.f.SetBlame(c.pr.MergeCommitSHA, testFile, c.blame...)
			tk := g.reconcile(t, "3buqdlot", 1)
			got := tk.Status.Monitor != nil && slices.Contains(tk.Status.Monitor.FollowUps, c.pr.Number)
			if got != c.follow {
				t.Fatalf("follow-up %v, want %v", got, c.follow)
			}
			labelled := slices.Contains(g.f.Added(c.pr.Number), FollowUpOfLabel(12))
			if c.follow && !slices.Contains(c.pr.Labels, FollowUpOfLabel(12)) && !labelled {
				t.Fatal("the factory labels a follow-up it found (MC13)")
			}
			// M2: the control issue's copy has its own narration key, so it is posted too.
			if c.follow && !contains(g.f.Comments(g.r.Cfg.ControlFor().Issue), "follow-up-"+strconv.Itoa(c.pr.Number)+" control") {
				t.Fatalf("the control issue hears of the follow-up: %q", g.f.Comments(g.r.Cfg.ControlFor().Issue))
			}
			if c.follow && !g.metrics.has("pr dead-links follow_up") {
				t.Fatalf("the follow-up outcome is counted: %v", g.metrics.recorded)
			}
		})
	}
}

func (m *fakeMetrics) has(s string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Contains(m.recorded, s)
}

// The main signal for a merge nobody verified (a maintainer's or Renovate's): red within verifyFor
// is a regression, labelled at once, never a revert (MC7); green past it is healthy.
func TestMainSignal(t *testing.T) {
	g := monitorRig(t)
	g.f.SetCommitChecks(mSHA, forge.Check{Name: "Pre-commit checks", State: "FAILURE"})
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Monitor.Main != "regression" || tk.Status.Monitor.Verdict != "regression" || len(g.f.Reverts()) != 0 ||
		!slices.Contains(g.f.Added(12), RegressionLabel) {
		t.Fatalf("%+v reverts %v", tk.Status.Monitor, g.f.Reverts())
	}
	// M2: the same verdict reaches the kill-switch issue under its own key.
	if ctl := g.r.Cfg.ControlFor().Issue; !contains(g.f.Comments(ctl), "monitor-12 control verdict=regression") {
		t.Fatalf("the control issue %d hears of the regression: %q", ctl, g.f.Comments(ctl))
	}
	// a regression is counted once, however many passes watch on for follow-ups
	g.r.Now = func() time.Time { return now.Add(time.Hour) }
	g.reconcile(t, "3buqdlot", 2)
	n := 0
	for _, s := range g.metrics.recorded {
		if s == "monitor dead-links regression" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the regression verdict is recorded once: %d in %v", n, g.metrics.recorded)
	}
	g = monitorRig(t)
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Monitor.Main != "healthy" || tk.Status.Monitor.Verdict != "" {
		t.Fatalf("main healthy, verdict open until the follow-up window ends: %+v", tk.Status.Monitor)
	}
	// still pending past 2 x verifyFor: inconclusive, never healthy
	g = monitorRig(t)
	g.f.SetCommitChecks(mSHA, forge.Check{Name: "Pre-commit checks", State: "PENDING"})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Monitor.Main != "inconclusive" {
		t.Fatalf("a check pending for 2 h is not healthy: %+v", tk.Status.Monitor)
	}
}

// The verdict is final and posted once at the end of followUpWindow; the task then leaves the
// reconciler's books. A class without monitor is never watched (R29).
func TestTheVerdictClosesTheWatch(t *testing.T) {
	g := monitorRig(t)
	g.setDone(t, "3buqdlot", 12, "Smana", mSHA, now.Add(-169*time.Hour))
	tk := g.reconcile(t, "3buqdlot", 2)
	if tk.Status.Monitor.Verdict != "healthy" || !tk.Status.Monitor.Posted || !contains(g.f.Comments(12), "verdict=healthy") ||
		!slices.Contains(g.f.Added(12), MergedLabel("dead-links")) {
		t.Fatalf("%+v %q", tk.Status.Monitor, g.f.Comments(12))
	}
	if g.r.monitorDue(tk) {
		t.Fatal("a posted verdict is not due again")
	}
	// R29, not vacuous (L14): a merged docs-links task, with its merge commit and time, is still
	// never watched, because docs-links declares no monitor.
	g = mergeRig(t, v1alpha1.PhaseDone)
	g.setDone(t, "3buqdlot", 12, "Smana", mSHA, now.Add(-2*time.Hour))
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Monitor != nil || g.r.monitorDue(tk) {
		t.Fatal("docs-links declares no monitor (R29)")
	}
}

func TestWorst(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{{[]string{"healthy", "healthy"}, "healthy"}, {[]string{"healthy", "inconclusive"}, "inconclusive"},
		{[]string{"inconclusive", "regression"}, "regression"}, {[]string{"healthy", ""}, ""}} {
		if got := Worst(c.in...); got != c.want {
			t.Fatalf("%v: %q", c.in, got)
		}
	}
}

// Every window counts from GitHub's mergedAt (L6): the Task's MergedAt is the factory's clock
// when it saw the merge, and may be later.
func TestTheWindowCountsFromGitHubsMergedAt(t *testing.T) {
	g := monitorRig(t)
	g.patch(t, func(tk *v1alpha1.Task) {
		ma := metav1.NewTime(now.Add(-time.Minute))
		tk.Status.PullRequest.MergedAt = &ma
	})
	tk := g.reconcile(t, "3buqdlot", 1)
	if got := tk.Status.Monitor.Since.Time; !got.Equal(now.Add(-2 * time.Hour)) {
		t.Fatalf("since %v, want GitHub's merge time", got)
	}
}

// A read that fails, or that cannot say, is inconclusive at the end of the window: never "no
// follow-up", never healthy. Each of the four unknowns is tried against a later PR that does
// overlap, so a swallowed error would read as a clean window.
func TestAnUnreadFollowUpIsInconclusive(t *testing.T) {
	for name, arm := range map[string]func(g *rig){
		"no blame": func(g *rig) { g.f.FailBlame("bbbb0", testFile, forge.ErrNoBlame) },
		"no patch": func(g *rig) {
			g.f.SetPullFiles(20, forge.FilePatch{Path: testFile, Status: "modified", PatchMissing: true})
		},
		"truncated": func(g *rig) { g.f.FailMergedSince(forge.ErrMergedTruncated) },
		"read error": func(g *rig) {
			g.f.FailPullFiles(20, errors.New("boom"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := monitorRig(t)
			g.setDone(t, "3buqdlot", 12, "Smana", mSHA, now.Add(-169*time.Hour))
			p := later(20, "Smana", "fix")
			p.MergedAt = now.Add(-100 * time.Hour)
			g.f.SetMerged(p)
			g.f.SetPullFiles(20, overlapping)
			g.f.SetBlame("bbbb0", testFile, mBlame...)
			arm(g)
			tk := g.reconcile(t, "3buqdlot", 1)
			m := tk.Status.Monitor
			if m.Verdict != "inconclusive" || len(m.FollowUps) != 0 || !m.Posted {
				t.Fatalf("%+v", m)
			}
			if !contains(g.f.Comments(12), "verdict=inconclusive") {
				t.Fatalf("%q", g.f.Comments(12))
			}
		})
	}
}

// A regression is found by the main signal; an unread follow-up read cannot hide it, nor soften it.
func TestAnUnreadFollowUpDoesNotHideARegression(t *testing.T) {
	g := monitorRig(t)
	g.f.SetCommitChecks(mSHA, forge.Check{Name: "Pre-commit checks", State: "FAILURE"})
	g.f.FailMergedSince(errors.New("boom"))
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Monitor.Verdict != "regression" {
		t.Fatalf("%+v", tk.Status.Monitor)
	}
}

// GitHub's search is eventually consistent: a later PR missing from one read is found by the
// next, since each read starts from the merge, not from where the last one ended.
func TestALateIndexedFollowUpIsFoundByTheNextRead(t *testing.T) {
	g := monitorRig(t)
	if tk := g.reconcile(t, "3buqdlot", 1); len(tk.Status.Monitor.FollowUps) != 0 {
		t.Fatal("nothing merged yet")
	}
	g.f.SetMerged(later(20, "Smana", "fix"))
	g.f.SetPullFiles(20, overlapping)
	g.f.SetBlame("bbbb0", testFile, mBlame...)
	g.r.Now = func() time.Time { return now.Add(monitorEvery + time.Minute) }
	if tk := g.reconcile(t, "3buqdlot", 1); !slices.Equal(tk.Status.Monitor.FollowUps, []int{20}) {
		t.Fatalf("%+v", tk.Status.Monitor)
	}
}

// The monitor reads GitHub once per monitorEvery, whatever wakes the reconciler.
func TestTheMonitorReadsOncePerInterval(t *testing.T) {
	g := monitorRig(t)
	g.f.SetMerged(later(20, "Smana", "fix"))
	g.f.SetPullFiles(20, overlapping)
	g.f.SetBlame("bbbb0", testFile, mBlame...)
	g.reconcile(t, "3buqdlot", 1)
	calls := g.f.BlameCalls()
	g.reconcile(t, "3buqdlot", 3)
	if g.f.BlameCalls() != calls {
		t.Fatalf("blame is cached and the pass throttled: %d then %d", calls, g.f.BlameCalls())
	}
}

// CARRY (Task 3's review): the ledger is cached per process, so the running process must count
// what its own monitor learns from the Tasks, or it under-demotes until a restart.
func TestPausedCountsTheTasksOwnMonitorOutcomes(t *testing.T) {
	for name, st := range map[string]*v1alpha1.MonitorStatus{
		"a follow-up":  {Since: metav1.NewTime(now.Add(-2 * time.Hour)), FollowUps: []int{20}},
		"a regression": {Since: metav1.NewTime(now.Add(-2 * time.Hour)), Main: "regression", Verdict: "regression"},
	} {
		t.Run(name, func(t *testing.T) {
			g := monitorRig(t)
			g.patch(t, func(tk *v1alpha1.Task) { tk.Status.Monitor = st })
			rc := g.r.Cfg.RepositoryFor("Smana/cloud-native-ref")
			if d, err := g.r.paused(t.Context(), rc, "dead-links"); err != nil || !d {
				t.Fatalf("demoted %v: %v", d, err)
			}
			g = monitorRig(t)
			g.patch(t, func(tk *v1alpha1.Task) { tk.Status.Monitor = &v1alpha1.MonitorStatus{Since: st.Since, Main: "healthy"} })
			if d, err := g.r.paused(t.Context(), rc, "dead-links"); err != nil || d {
				t.Fatalf("a healthy watch demotes nothing: %v %v", d, err)
			}
		})
	}
}
