// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/narrate"
)

// monitorEvery is how often a merge under watch is read: its signals move on CI's, Flux's and the
// detector's clocks, and each read costs the merger's GitHub calls.
const monitorEvery = 10 * time.Minute

// searchLag is GitHub's search indexing delay, as the monitor budgets it: the follow-up read
// starts that much before the merge, and the watch ends that much after its window, so a pull
// request merged at the very end of the window is still found before the verdict is final.
const searchLag = 5 * time.Minute

// maxFollowUps bounds status.monitor.followUps (the CRD's maxItems).
const maxFollowUps = 16

// monitorCache holds what the monitor reads more than once, per process: the last pass of each
// task (the throttle), GitHub's merged window, and what it derived from the files and blame of
// the pull requests it compares. Every entry carries the merge time of the pull request it
// describes and is pruned once that is older than the longest follow-up window plus searchLag; a
// task's throttle entry goes when its verdict is posted. Raw patches are never kept.
type monitorCache struct {
	mu     sync.Mutex
	last   map[string]time.Time // task -> its last completed pass
	merged map[string]mergedRead
	files  map[string]derivedFiles
	blame  map[string]derivedBlame
}

// mergedRead is one MergedSince answer: from is where it starts, narrowed when the search
// truncated and the read began later than asked.
type mergedRead struct {
	from  time.Time
	at    time.Time
	pulls []forge.MergedPull
}

// derivedFile is what the overlap check needs of one file of a pull request: its names and the
// base lines its patch changes. unread: the lines are unknown (an omitted patch, a malformed hunk).
type derivedFile struct {
	path, blamePath string
	lines           []int
	unread          bool
}

type derivedFiles struct {
	files []derivedFile
	at    time.Time // the pull request's merge time
}

type derivedBlame struct {
	ranges []forge.BlameRange
	at     time.Time
}

// monitorDue: a Done task merged through a class that declares a monitor (R29), whose verdict is
// not posted yet. Reverted tasks are not watched: the revert is their outcome.
func (r *Reconciler) monitorDue(t *v1alpha1.Task) bool {
	ref := t.Status.PullRequest
	if t.Status.Phase != v1alpha1.PhaseDone || ref == nil || ref.MergedAt == nil || ref.MergeCommitSHA == "" {
		return false
	}
	cl, ok := r.repo(t).Classes[t.Spec.PredictedClass]
	if !ok || cl.Monitor == nil || (t.Status.Monitor != nil && t.Status.Monitor.Posted) {
		return false
	}
	// Declaring a monitor never back-fills history: a merge whose window is already over, counted
	// from GitHub's merge time, starts no watch.
	return t.Status.Monitor != nil || r.Now().Sub(ref.MergedAt.Time) <= cl.Monitor.FollowUpWindow.Duration+searchLag
}

// Worst is the monitor's verdict over its signals: regression over inconclusive over healthy; ""
// while any is open and none regressed.
func Worst(vs ...string) string {
	rank := map[string]int{"healthy": 0, "inconclusive": 1, "regression": 2}
	out, open := "healthy", false
	for _, v := range vs {
		if v == "" {
			open = true
			continue
		}
		if rank[v] > rank[out] {
			out = v
		}
	}
	if out != "regression" && open {
		return ""
	}
	return out
}

// monitor reads every open signal once, labels what it learns on GitHub (R30, R31), and posts the
// verdict when it is final. Whatever wakes the reconciler, GitHub is read once per monitorEvery.
func (r *Reconciler) monitor(ctx context.Context, t *v1alpha1.Task) error {
	if last, ok := r.mon.lastPass(t.Name); ok && r.Now().Sub(last) < monitorEvery {
		return nil
	}
	ref, cl := t.Status.PullRequest, r.repo(t).Classes[t.Spec.PredictedClass]
	pr, err := r.gh(ctx).PullRequest(ctx, ref.Number)
	if err != nil {
		return err
	}
	m := t.Status.Monitor
	if m == nil {
		// L6: every window counts from GitHub's merge time. The Task's MergedAt is the factory's
		// clock when it saw the merge, which may be later.
		at := pr.MergedAt
		if at.IsZero() {
			at = ref.MergedAt.Time
		}
		m = &v1alpha1.MonitorStatus{Since: metav1.Time{Time: at}}
		t.Status.Monitor = m
	}
	r.label(ctx, pr, MergedLabel(t.Spec.PredictedClass))
	if m.Main == "" {
		if m.Main, err = r.mainSignal(ctx, t); err != nil {
			return err
		}
	}
	// Task 8 adds the readings of m.Oracle (cl.Monitor.OracleRuns > 0) and m.Flux (cl.Monitor.Flux != nil).
	r.followUps(ctx, t, cl)
	ended := r.Now().Sub(m.Since.Time) >= cl.Monitor.FollowUpWindow.Duration+searchLag
	signals := []*string{&m.Main}
	if cl.Monitor.OracleRuns > 0 {
		signals = append(signals, &m.Oracle)
	}
	if cl.Monitor.Flux != nil {
		signals = append(signals, &m.Flux)
	}
	var read []string
	for _, s := range signals {
		if ended && *s == "" {
			*s = "inconclusive" // the window is over and the signal never said: it did not say healthy
		}
		read = append(read, *s)
	}
	first := m.Verdict == ""
	switch v := Worst(read...); {
	case v == "regression": // a follow-up is its own bad outcome (Task 3), never a monitor verdict
		m.Verdict = "regression"
	case ended && m.Verdict == "":
		if v == "healthy" && m.FollowUpsUnread {
			v = "inconclusive" // a follow-up read that could not say is no clean window
		}
		m.Verdict = v
	}
	if m.Verdict == "regression" {
		r.label(ctx, pr, RegressionLabel)
	}
	if m.Verdict != "" && !m.Posted {
		if first {
			class, verdict := t.Spec.PredictedClass, m.Verdict
			record(ctx, func(ctx context.Context) { r.Metrics.MonitorVerdict(ctx, class, verdict) })
		}
		demotes := r.repo(t).Merge.Breaker.MaxFollowUps > 0
		narrateOn(t, ref.Number, narrate.MonitorVerdict(t, ref.Number, m.Verdict, monitorWhy(m), false, demotes))
		if m.Verdict == "regression" {
			// M2: its own key, or narrateOn drops it as the PR's comment already queued.
			if n := r.controlIssue(t); n > 0 {
				narrateOn(t, n, narrate.MonitorVerdict(t, ref.Number, m.Verdict, monitorWhy(m), true, demotes))
			}
		}
		m.Posted = m.Verdict != "regression" || ended // a regression is posted at once and watched on for follow-ups
	}
	if m.Posted {
		r.mon.forget(t.Name) // nothing is due again
	} else {
		r.mon.passed(t.Name, r.Now())
	}
	return nil
}

// monitorWhy lists the signals the verdict read, for the comment.
func monitorWhy(m *v1alpha1.MonitorStatus) []string {
	out := []string{"main CI: " + cmpOpen(m.Main), fmt.Sprintf("follow_ups=%d", len(m.FollowUps))}
	if m.Oracle != "" {
		out = append(out, "detector: "+m.Oracle)
	}
	if m.Flux != "" {
		out = append(out, "Flux: "+m.Flux)
	}
	if len(m.FollowUps) > 0 {
		out = append(out, fmt.Sprintf("follow-up fixes: %v, counted by the breaker, not in this verdict", m.FollowUps))
	}
	if m.FollowUpsUnread {
		out = append(out, "follow-up reads incomplete")
	}
	return out
}

func cmpOpen(s string) string {
	if s == "" {
		return "open"
	}
	return s
}

// mainSignal is main's CI on the merge commit. An auto-merge's was watched by verifying, which
// reached Done "merged_verified": healthy. Any other merge reads the commit's checks: red is a
// regression at once, green once verifyFor has passed is healthy, and a check still pending after
// twice that is inconclusive; otherwise it stays open.
func (r *Reconciler) mainSignal(ctx context.Context, t *v1alpha1.Task) (string, error) {
	ref, m := t.Status.PullRequest, t.Status.Monitor
	if ref.AutoMerged {
		return "healthy", nil
	}
	rc := r.repo(t)
	runs, err := r.merger(ctx).CommitChecks(ctx, ref.MergeCommitSHA)
	if err != nil {
		return "", err
	}
	age, verifyFor := r.Now().Sub(m.Since.Time), rc.Merge.VerifyFor.Duration
	switch MainState(runs, rc.Merge.VerifyChecks) {
	case "FAILURE":
		return "regression", nil
	case "SUCCESS":
		if age >= verifyFor {
			return "healthy", nil
		}
	}
	if age >= 2*verifyFor {
		return "inconclusive", nil
	}
	return "", nil
}

func (c *monitorCache) lastPass(task string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.last[task]
	return at, ok
}

func (c *monitorCache) passed(task string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = map[string]time.Time{}
	}
	c.last[task] = at
}

func (c *monitorCache) forget(task string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.last, task)
}

// prune drops what describes a pull request merged before cutoff, and throttle entries older than it.
func (c *monitorCache) prune(cutoff time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	maps.DeleteFunc(c.files, func(_ string, d derivedFiles) bool { return d.at.Before(cutoff) })
	maps.DeleteFunc(c.blame, func(_ string, d derivedBlame) bool { return d.at.Before(cutoff) })
	maps.DeleteFunc(c.last, func(_ string, at time.Time) bool { return at.Before(cutoff) })
}

// longestWindow is the longest followUpWindow any class declares.
func (r *Reconciler) longestWindow() time.Duration {
	var w time.Duration
	for _, rc := range r.Cfg.Repos() {
		for _, cl := range rc.Classes {
			if cl.Monitor != nil {
				w = max(w, cl.Monitor.FollowUpWindow.Duration)
			}
		}
	}
	return w
}

// mergedSince is the repository's merged pull requests since from, kept for monitorEvery unless
// fresh asks for a read now. A search that truncates is read again from the middle of the window:
// what it returns is then incomplete for a task that needs the earlier half, which the caller
// learns from complete. The cache lock is not held across the GitHub calls.
func (r *Reconciler) mergedSince(ctx context.Context, t *v1alpha1.Task, from time.Time, fresh bool) (pulls []forge.MergedPull, complete bool, err error) {
	c := &r.mon
	c.mu.Lock()
	hit, ok := c.merged[t.Spec.Repository]
	c.mu.Unlock()
	if ok && !fresh && r.Now().Sub(hit.at) < monitorEvery && !hit.from.After(from) {
		return hit.pulls, true, nil
	}
	c.prune(r.Now().Add(-r.longestWindow() - searchLag))
	read := from
	pulls, err = r.merger(ctx).MergedSince(ctx, read)
	if errors.Is(err, forge.ErrMergedTruncated) {
		read = from.Add(r.Now().Sub(from) / 2)
		r.log().Warn("merged pull requests truncated, reading the later half only", "task.id", t.Name, "from", read)
		pulls, err = r.merger(ctx).MergedSince(ctx, read)
	}
	if err != nil {
		return nil, false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.merged == nil {
		c.merged = map[string]mergedRead{}
	}
	c.merged[t.Spec.Repository] = mergedRead{from: read, at: r.Now(), pulls: pulls}
	return pulls, !read.After(from), nil
}

// pullFiles is what a pull request's files say, read once per process: a merged pull request's
// do not change. at is its merge time, for pruning.
func (r *Reconciler) pullFiles(ctx context.Context, t *v1alpha1.Task, n int, at time.Time) ([]derivedFile, error) {
	k := fmt.Sprintf("%s#%d", t.Spec.Repository, n)
	c := &r.mon
	c.mu.Lock()
	d, ok := c.files[k]
	c.mu.Unlock()
	if ok {
		return d.files, nil
	}
	fs, err := r.merger(ctx).PullFiles(ctx, n)
	if err != nil {
		return nil, err
	}
	d = derivedFiles{at: at}
	for _, f := range fs {
		df := derivedFile{path: f.Path, blamePath: f.BlamePath()}
		if df.lines, err = f.BaseLines(); err != nil {
			df.unread = true
		}
		d.files = append(d.files, df)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.files == nil {
		c.files = map[string]derivedFiles{}
	}
	c.files[k] = d
	return d.files, nil
}

// blameAt is who last changed a file's lines before a merge, read once per process.
func (r *Reconciler) blameAt(ctx context.Context, t *v1alpha1.Task, sha, path string, at time.Time) ([]forge.BlameRange, error) {
	k := t.Spec.Repository + "\x00" + sha + "\x00" + path
	c := &r.mon
	c.mu.Lock()
	d, ok := c.blame[k]
	c.mu.Unlock()
	if ok {
		return d.ranges, nil
	}
	rs, err := r.merger(ctx).BlameAtParent(ctx, sha, path)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blame == nil {
		c.blame = map[string]derivedBlame{}
	}
	c.blame[k] = derivedBlame{ranges: rs, at: at}
	return rs, nil
}

// followUps finds the later pull requests that fixed this merge (MC8): one labelled
// factory/follow-up-of by a maintainer, or one that changes a line this merge wrote, as blame at
// the later pull request's own parent says (the merges are squash merges). A read that fails or
// cannot say (no blame, an omitted patch, a truncated search) is never "no follow-up": it sets
// FollowUpsUnread, which keeps the watch from ending healthy.
func (r *Reconciler) followUps(ctx context.Context, t *v1alpha1.Task, cl config.Class) {
	ref, m := t.Status.PullRequest, t.Status.Monitor
	// L6, search lag: every read starts before the merge, so a pull request the index was slow to
	// show is found by the next one.
	// The final pass reads fresh: the cache's 10 minutes would otherwise hide the window's last.
	merged, complete, err := r.mergedSince(ctx, t, m.Since.Add(-searchLag), r.Now().Sub(m.Since.Time) >= cl.Monitor.FollowUpWindow.Duration+searchLag)
	if err != nil {
		r.log().Warn("follow-up read failed", "task.id", t.Name, "err", err)
		m.FollowUpsUnread = true
		return
	}
	unread := !complete
	end := m.Since.Add(cl.Monitor.FollowUpWindow.Duration)
	var mine map[string]bool // m's paths, read once, lazily
	for _, p := range merged {
		if p.Number == ref.Number || !p.MergedAt.After(m.Since.Time) || p.MergedAt.After(end) ||
			slices.Contains(m.FollowUps, p.Number) || slices.Contains(p.Labels, RevertOfLabel(ref.Number)) {
			continue
		}
		found := slices.Contains(p.Labels, FollowUpOfLabel(ref.Number))
		if !found {
			if mine == nil {
				fs, err := r.pullFiles(ctx, t, ref.Number, m.Since.Time)
				if err != nil {
					r.log().Warn("follow-up read failed: the merge's files", "task.id", t.Name, "err", err)
					m.FollowUpsUnread = true
					return
				}
				mine = map[string]bool{}
				for _, f := range fs {
					mine[f.path] = true
				}
			}
			if p.MultiCommitRebase() {
				// A rebase of several commits leaves the first parent short of the base: blame there
				// reads the wrong lines. Unread, never "no overlap".
				r.log().Warn("follow-up read: a multi-commit rebase merge", "task.id", t.Name, "pr", p.Number)
				unread = true
				continue
			}
			var ok bool
			if found, ok = r.overlaps(ctx, t, p, mine); !ok {
				unread = true
			}
		}
		if found {
			r.followUpFound(ctx, t, p)
		}
	}
	m.FollowUpsUnread = unread
}

// overlaps reports whether pull request p changed a line the monitored merge wrote. ok is false
// when some file of it could not be read: found is then only what the others showed.
func (r *Reconciler) overlaps(ctx context.Context, t *v1alpha1.Task, p forge.MergedPull, mine map[string]bool) (found, ok bool) {
	ref := t.Status.PullRequest
	fs, err := r.pullFiles(ctx, t, p.Number, p.MergedAt)
	if err != nil {
		r.log().Warn("follow-up read failed: a later pull request's files", "task.id", t.Name, "pr", p.Number, "err", err)
		return false, false
	}
	ok = true
	for _, f := range fs {
		if !mine[f.path] && !mine[f.blamePath] {
			continue
		}
		if f.unread {
			r.log().Warn("follow-up read failed: the lines a patch changed", "task.id", t.Name, "pr", p.Number, "file", f.path)
			ok = false
			continue
		}
		if len(f.lines) == 0 {
			continue // a pure addition changes no line m wrote
		}
		if p.MergeCommitSHA == "" {
			ok = false
			continue
		}
		rs, err := r.blameAt(ctx, t, p.MergeCommitSHA, f.blamePath, p.MergedAt)
		if err != nil {
			r.log().Warn("follow-up read failed: blame", "task.id", t.Name, "pr", p.Number, "err", err)
			ok = false
			continue
		}
		if slices.ContainsFunc(rs, func(b forge.BlameRange) bool {
			return b.Commit == ref.MergeCommitSHA && slices.ContainsFunc(f.lines, func(l int) bool { return l >= b.Start && l <= b.End })
		}) {
			return true, ok
		}
	}
	return false, ok
}

// followUpFound records p as a fix of the monitored merge: on the status, as a label on GitHub
// (MC13), as an outcome, and on the merge's pull request and the control issue, each its own key (M2).
func (r *Reconciler) followUpFound(ctx context.Context, t *v1alpha1.Task, p forge.MergedPull) {
	ref, m := t.Status.PullRequest, t.Status.Monitor
	if len(m.FollowUps) >= maxFollowUps {
		return
	}
	m.FollowUps = append(m.FollowUps, p.Number)
	slices.Sort(m.FollowUps)
	r.label(ctx, forge.PR{Number: p.Number, Labels: p.Labels}, FollowUpOfLabel(ref.Number))
	class := t.Spec.PredictedClass
	record(ctx, func(ctx context.Context) { r.Metrics.PROutcome(ctx, class, "follow_up") })
	narrateOn(t, ref.Number, narrate.FollowUpFound(t, ref.Number, p.Number, false))
	if n := r.controlIssue(t); n > 0 {
		narrateOn(t, n, narrate.FollowUpFound(t, ref.Number, p.Number, true))
	}
}

// controlIssue is the kill switch's issue when it lives on the task's repository, else 0: a
// comment is posted through the task's repository, so another repository's issue of the same
// number is never its to write (as intake's controlIssue decides).
func (r *Reconciler) controlIssue(t *v1alpha1.Task) int {
	if ctl := r.Cfg.ControlFor(); ctl.Issue > 0 && strings.EqualFold(ctl.Repository, t.Spec.Repository) {
		return ctl.Issue
	}
	return 0
}
