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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// ended runs the watch of a merge made 167 h ago: its first pass starts it inside the window, and
// a pass 2 h later (past followUpWindow plus searchLag) ends it. Declaring a monitor never
// back-fills a merge whose window is already over, so a window ends only by being watched.
func (g *rig) ended(t *testing.T) *v1alpha1.Task {
	t.Helper()
	g.reconcile(t, "3buqdlot", 1)
	g.r.Now = func() time.Time { return now.Add(2 * time.Hour) }
	return g.reconcile(t, "3buqdlot", 2)
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
		// R35, M3: the lane's own next bump rewrites the line the adopted merge wrote, and is no follow-up.
		"the lane's next bump": {later(26, "renovate[bot]", "renovate/foo-2.x"), []forge.FilePatch{overlapping}, mBlame, false},
		"another bot's bump":   {later(27, "dependabot[bot]", "dependabot/foo"), []forge.FilePatch{overlapping}, mBlame, true},
	} {
		t.Run(name, func(t *testing.T) {
			g := monitorRig(t)
			cl := g.r.Cfg.Classes["dead-links"]
			cl.Author = &config.Author{Adopt: &config.Adopt{Logins: []string{"renovate[bot]"}, Branches: []string{"^renovate/"}}}
			g.r.Cfg.Classes["dead-links"] = cl
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
	g.setDone(t, "3buqdlot", 12, "Smana", mSHA, now.Add(-167*time.Hour))
	tk := g.ended(t)
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
		"the merge's own files": func(g *rig) { g.f.FailPullFiles(12, errors.New("boom")) },
		"a malformed hunk": func(g *rig) {
			g.f.SetPullFiles(20, forge.FilePatch{Path: testFile, Status: "modified", Patch: "@@ bogus @@\n-a\n+b\n"})
		},
		"no merge commit": func(g *rig) {
			p := later(20, "Smana", "fix")
			p.MergedAt, p.MergeCommitSHA = now.Add(-100*time.Hour), ""
			g.f.SetMerged(p)
		},
		"a multi-commit rebase": func(g *rig) {
			p := later(20, "Smana", "fix")
			p.MergedAt, p.Commits, p.ParentPulls = now.Add(-100*time.Hour), 3, []int{20}
			g.f.SetMerged(p)
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := monitorRig(t)
			g.setDone(t, "3buqdlot", 12, "Smana", mSHA, now.Add(-167*time.Hour))
			p := later(20, "Smana", "fix")
			p.MergedAt = now.Add(-100 * time.Hour)
			g.f.SetMerged(p)
			g.f.SetPullFiles(20, overlapping)
			g.f.SetBlame("bbbb0", testFile, mBlame...)
			arm(g)
			tk := g.ended(t)
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

// The busy repository: the whole window truncates, the later half reads fine and holds no
// follow-up. The read was incomplete, so the window ends inconclusive, never healthy; the same
// read of a window the later half does cover (a younger merge) is complete and stays healthy.
func TestANarrowedReadIsInconclusive(t *testing.T) {
	g := monitorRig(t)
	g.setDone(t, "3buqdlot", 12, "Smana", mSHA, now.Add(-167*time.Hour))
	g.f.TruncateMergedBefore(now.Add(-100 * time.Hour))
	tk := g.ended(t)
	if m := tk.Status.Monitor; m.Verdict != "inconclusive" || !m.FollowUpsUnread || len(m.FollowUps) != 0 {
		t.Fatalf("a half-read window is not healthy: %+v", m)
	}
	g = monitorRig(t)
	g.setDone(t, "3buqdlot", 12, "Smana", mSHA, now.Add(-167*time.Hour))
	g.f.TruncateMergedBefore(now.Add(-170 * time.Hour)) // nothing truncates: the whole window reads
	if tk := g.ended(t); tk.Status.Monitor.Verdict != "healthy" {
		t.Fatalf("a whole read is healthy: %+v", tk.Status.Monitor)
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

// countingForge counts the task App's uncached reads.
type countingForge struct {
	TaskForge
	prs int
}

func (c *countingForge) PullRequest(ctx context.Context, n int) (forge.PR, error) {
	c.prs++
	return c.TaskForge.PullRequest(ctx, n)
}

// The monitor reads GitHub once per monitorEvery, whatever wakes the reconciler: its own status
// write wakes it at once. PullRequest is uncached, so it is the throttle's proof.
func TestTheMonitorReadsOncePerInterval(t *testing.T) {
	g := monitorRig(t)
	cf := &countingForge{TaskForge: g.f}
	g.r.Forge = cf
	g.reconcile(t, "3buqdlot", 1)
	if cf.prs != 1 {
		t.Fatalf("one read on the first pass: %d", cf.prs)
	}
	g.reconcile(t, "3buqdlot", 3)
	if cf.prs != 1 {
		t.Fatalf("a pass inside monitorEvery reads nothing: %d reads", cf.prs)
	}
	g.r.Now = func() time.Time { return now.Add(monitorEvery + time.Minute) }
	g.reconcile(t, "3buqdlot", 1)
	if cf.prs != 2 {
		t.Fatalf("a pass after monitorEvery reads again: %d reads", cf.prs)
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

// MC13 ruling: cloud-native-ref squashes, ai-native-ref may also rebase. A multi-commit rebase
// leaves the first parent short of the base, so its blame is unread; a single-commit rebase, and a
// squash of many commits (whose parent is the base), are read as usual.
func TestRebaseMergesAreReadOnlyWhenSafe(t *testing.T) {
	for name, c := range map[string]struct {
		commits int
		parents []int
		follow  bool
	}{
		"a squash of three commits":                         {3, []int{7}, true},
		"a single-commit rebase":                            {1, []int{20}, true},
		"a multi-commit rebase":                             {3, []int{20}, false},
		"commits unknown, no parent":                        {0, nil, true},
		"three commits, parent pulls unknown (GitHub lags)": {3, nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			g := monitorRig(t)
			g.setDone(t, "3buqdlot", 12, "Smana", mSHA, now.Add(-167*time.Hour))
			p := later(20, "Smana", "fix")
			p.MergedAt, p.Commits, p.ParentPulls = now.Add(-100*time.Hour), c.commits, c.parents
			g.f.SetMerged(p)
			g.f.SetPullFiles(20, overlapping)
			g.f.SetBlame("bbbb0", testFile, mBlame...)
			m := g.ended(t).Status.Monitor
			if got := slices.Contains(m.FollowUps, 20); got != c.follow || m.FollowUpsUnread == c.follow {
				t.Fatalf("follow-up %v unread %v: %+v", got, m.FollowUpsUnread, m)
			}
			if !c.follow && m.Verdict != "inconclusive" {
				t.Fatalf("a read that cannot say ends inconclusive: %+v", m)
			}
		})
	}
}

// At the window's end an open signal never said healthy: Oracle and Flux readings arrive in Task 8,
// until then a class that declares them ends inconclusive.
func TestAnOpenSignalIsInconclusiveAtTheEnd(t *testing.T) {
	g := monitorRig(t)
	cl := g.r.Cfg.Classes["dead-links"]
	cl.Monitor.OracleRuns, cl.Monitor.Flux = 2, &config.FluxWatch{Source: "flux-system/cnref", Within: config.Duration{Duration: time.Hour}}
	g.r.Cfg.Classes["dead-links"] = cl
	g.setDone(t, "3buqdlot", 12, "Smana", mSHA, now.Add(-167*time.Hour))
	m := g.ended(t).Status.Monitor
	if m.Oracle != "inconclusive" || m.Flux != "inconclusive" || m.Verdict != "inconclusive" || m.Main != "healthy" {
		t.Fatalf("%+v", m)
	}
}

// The verdict comment says how many follow-ups were counted, beside a healthy verdict.
func TestTheVerdictNamesItsFollowUps(t *testing.T) {
	g := monitorRig(t)
	g.setDone(t, "3buqdlot", 12, "Smana", mSHA, now.Add(-167*time.Hour))
	p := later(20, "Smana", "fix")
	p.MergedAt = now.Add(-100 * time.Hour)
	g.f.SetMerged(p)
	g.f.SetPullFiles(20, overlapping)
	g.f.SetBlame("bbbb0", testFile, mBlame...)
	g.ended(t)
	if cs := g.f.Comments(12); !contains(cs, "verdict=healthy") || !contains(cs, "follow_ups=1") {
		t.Fatalf("%q", cs)
	}
	g = monitorRig(t)
	g.setDone(t, "3buqdlot", 12, "Smana", mSHA, now.Add(-167*time.Hour))
	g.ended(t)
	if !contains(g.f.Comments(12), "follow_ups=0") {
		t.Fatalf("%q", g.f.Comments(12))
	}
}

// The caches hold what was derived (never a raw patch), prune what describes a merge older than
// the longest window plus searchLag, and a posted verdict ends its task's throttle entry.
func TestTheMonitorCachesAreBounded(t *testing.T) {
	g := monitorRig(t)
	g.f.SetMerged(later(20, "Smana", "fix"))
	g.f.SetPullFiles(20, overlapping)
	g.f.SetBlame("bbbb0", testFile, mBlame...)
	tk := g.reconcile(t, "3buqdlot", 1)
	if len(g.r.mon.files) != 2 || len(g.r.mon.blame) != 1 {
		t.Fatalf("files %d blame %d", len(g.r.mon.files), len(g.r.mon.blame))
	}
	if _, ok := g.r.mon.lastPass("3buqdlot"); !ok {
		t.Fatal("an open watch is throttled")
	}
	g.r.Now = func() time.Time { return now.Add(168*time.Hour + 2*searchLag) }
	if _, _, err := g.r.mergedSince(t.Context(), tk, now, true); err != nil {
		t.Fatal(err)
	}
	if len(g.r.mon.files) != 0 || len(g.r.mon.blame) != 0 {
		t.Fatalf("entries for merges past the horizon are gone: files %d blame %d", len(g.r.mon.files), len(g.r.mon.blame))
	}
	if _, ok := g.r.mon.lastPass("3buqdlot"); ok {
		t.Fatal("the throttle entry goes with the horizon")
	}
	g = monitorRig(t)
	g.setDone(t, "3buqdlot", 12, "Smana", mSHA, now.Add(-167*time.Hour))
	if tk := g.ended(t); !tk.Status.Monitor.Posted {
		t.Fatal("posted")
	}
	if _, ok := g.r.mon.lastPass("3buqdlot"); ok {
		t.Fatal("a posted verdict leaves no throttle entry")
	}
}

// The window's last minutes: a final pass reads fresh, whatever the cache holds.
func TestTheFinalReadBypassesTheMergedCache(t *testing.T) {
	g := monitorRig(t)
	tk := g.reconcile(t, "3buqdlot", 1)
	from := now.Add(-3 * time.Hour)
	if ps, _, _ := g.r.mergedSince(t.Context(), tk, from, false); len(ps) != 0 {
		t.Fatal("nothing yet")
	}
	g.f.SetMerged(later(20, "Smana", "fix"))
	if ps, _, _ := g.r.mergedSince(t.Context(), tk, from, false); len(ps) != 0 {
		t.Fatal("the cache answers inside its interval")
	}
	if ps, _, _ := g.r.mergedSince(t.Context(), tk, from, true); len(ps) != 1 {
		t.Fatal("fresh reads again")
	}
}

// A write that fails does not leave the pass recorded: the retry is not throttled.
func TestAFailedStatusWriteDoesNotThrottleTheRetry(t *testing.T) {
	g := monitorRig(t)
	g.r.mon.passed("3buqdlot", now)
	g.r.mon.forget("3buqdlot")
	if _, ok := g.r.mon.lastPass("3buqdlot"); ok {
		t.Fatal("forget clears the pass")
	}
	lose := true
	c := interceptor.NewClient(g.c.(client.WithWatch), interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, o client.Object, opts ...client.SubResourceUpdateOption) error {
		if tk, ok := o.(*v1alpha1.Task); ok && lose && tk.Status.Monitor != nil {
			lose = false
			return errors.New("stale")
		}
		return cl.SubResource(sub).Update(ctx, o, opts...)
	}})
	g.r.Client = c
	if _, err := g.r.Reconcile(t.Context(), reqFor("3buqdlot")); err == nil {
		t.Fatal("the status write is lost")
	}
	if _, ok := g.r.mon.lastPass("3buqdlot"); ok {
		t.Fatal("a pass whose write was lost is not recorded")
	}
}

// L6: the Task's MergedAt is GitHub's, wherever the factory sees the merge: a human merge from
// AwaitingCI, one from AwaitingHuman (prEnded) and the merger's own from AutoMerging. This changes
// the breaker's window for every class, as intended: it is the merge's real time.
func TestMergedAtIsGitHubsEverywhere(t *testing.T) {
	at := now.Add(-3 * time.Hour)
	for name, c := range map[string]struct {
		phase, who string
	}{
		"awaitingCI":    {v1alpha1.PhaseAwaitingCI, "Smana"},
		"awaitingHuman": {v1alpha1.PhaseAwaitingHuman, "Smana"},
		"autoMerging":   {v1alpha1.PhaseAutoMerging, "ogenki-agent-merger[bot]"},
	} {
		t.Run(name, func(t *testing.T) {
			g := mergeRig(t, c.phase)
			g.f.SetPR(forge.PR{Number: 12, NodeID: "PR_12", State: "MERGED", Author: "ogenki-agents[bot]", HeadRef: "agent/3buqdlot",
				HeadSHA: "abc", MergedBy: c.who, MergedAt: at, MergeCommitSHA: mSHA})
			tk := g.reconcile(t, "3buqdlot", 1)
			if ma := tk.Status.PullRequest.MergedAt; ma == nil || !ma.Time.Equal(at) {
				t.Fatalf("%s %s merged at %v, want GitHub's %v", tk.Status.Phase, tk.Status.Reason, ma, at)
			}
		})
	}
}

// The settle requeue never delays the monitor: the smaller of the two wins, and a watched task
// settles while it is watched.
func TestTheSettleRequeueDoesNotDelayTheMonitor(t *testing.T) {
	g := monitorRig(t)
	g.r.Cfg.Poll.Meter = config.Duration{Duration: 10 * time.Minute} // settle window 20m30s
	g.patch(t, func(tk *v1alpha1.Task) {
		tk.Status.Runs = []v1alpha1.RunRecord{{ID: rid(0), Role: "implementer", Trigger: "initial"}}
		ps := metav1.NewTime(now)
		tk.Status.PhaseSince = &ps
	})
	res, err := g.r.Reconcile(t.Context(), reqFor("3buqdlot"))
	if err != nil || res.RequeueAfter != monitorEvery {
		t.Fatalf("%v %v", res, err)
	}
	g.r.Now = func() time.Time { return now.Add(21 * time.Minute) }
	if tk := g.reconcile(t, "3buqdlot", 1); !tk.Status.UsageSettled {
		t.Fatal("a watched task settles its usage")
	}
}
