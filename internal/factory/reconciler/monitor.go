// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/finding"
	"github.com/Smana/maku/internal/factory/forge"
	"github.com/Smana/maku/internal/factory/intake"
	"github.com/Smana/maku/internal/factory/narrate"
)

// monitorEvery is how often a merge under watch is read: its signals move on CI's, Flux's and the
// detector's clocks, and each read costs the merger's GitHub calls.
const monitorEvery = 10 * time.Minute

// searchLag is GitHub's search indexing delay, as the monitor budgets it: the follow-up read
// starts that much before the merge, and the watch ends that much after its window, so a pull
// request merged at the very end of the window is still found before the verdict is final.
const searchLag = 5 * time.Minute

// maxFluxBlamed bounds status.monitor.fluxBlamed (the CRD's maxItems).
const maxFluxBlamed = 5

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
	if undecidedAdoptedMerge(t, r.repo(t)) {
		return false // Renovate's automerge of a head the gate never decided is not the class's merge
	}
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
	// The verification endpoint seeds Since from the Task's MergedAt (the factory's clock) when it
	// records a verdict before the first pass: GitHub's merge time is the one every window counts from (L6).
	if !pr.MergedAt.IsZero() && m.Since.Time.Equal(ref.MergedAt.Time) && !pr.MergedAt.Equal(m.Since.Time) {
		m.Since = metav1.Time{Time: pr.MergedAt}
	}
	r.label(ctx, pr, MergedLabel(t.Spec.PredictedClass))
	if m.Main == "" {
		if m.Main, err = r.mainSignal(ctx, t); err != nil {
			return err
		}
	}
	if cl.Monitor.OracleRuns > 0 && m.Oracle == "" {
		m.Oracle = r.oracleSignal(ctx, t, cl)
	}
	if cl.Monitor.Flux != nil && m.Flux == "" {
		m.Flux = r.fluxSignal(ctx, t, *cl.Monitor.Flux)
	}
	if vw := cl.Monitor.Verification; vw != nil && m.Verified == "" && r.verifiedBy(ctx, t) {
		switch v := m.Verification; {
		case v != nil && v.Verdict == "resolved":
			m.Verified = "healthy"
		case v != nil && v.Verdict == "persisting":
			m.Verified = "regression"
		case r.Now().Sub(m.Since.Time) >= vw.Within.Duration:
			m.Verified = "inconclusive"
		}
		if m.Verified != "" {
			class, verdict := t.Spec.PredictedClass, cmp.Or(map[string]string{"healthy": "resolved", "regression": "persisting"}[m.Verified], "none")
			record(ctx, func(ctx context.Context) { r.Metrics.MonitorVerification(ctx, class, verdict) })
		}
	}
	r.followUps(ctx, t, cl)
	ended := r.Now().Sub(m.Since.Time) >= cl.Monitor.FollowUpWindow.Duration+searchLag
	signals := []*string{&m.Main}
	if cl.Monitor.OracleRuns > 0 {
		signals = append(signals, &m.Oracle)
	}
	if cl.Monitor.Flux != nil {
		signals = append(signals, &m.Flux)
	}
	if cl.Monitor.Verification != nil && m.Verified != "" {
		signals = append(signals, &m.Verified)
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
	if m.Verified == "regression" && m.FollowUpIssue == 0 {
		r.fileFollowUp(ctx, t)
	}
	if m.Verdict != "" && !m.Posted {
		if first {
			class, verdict := t.Spec.PredictedClass, m.Verdict
			record(ctx, func(ctx context.Context) { r.Metrics.MonitorVerdict(ctx, class, verdict) })
		}
		demotes := r.repo(t).Merge.Breaker.MaxFollowUps > 0
		narrateOn(t, ref.Number, narrate.MonitorVerdict(t, ref.Number, m.Verdict, monitorWhy(m, t.Spec.Origin, r.Cfg.RoomsURL), false, demotes))
		if m.Verdict == "regression" {
			// M2: its own key, or narrateOn drops it as the PR's comment already queued.
			if n := r.controlIssue(t); n > 0 {
				narrateOn(t, n, narrate.MonitorVerdict(t, ref.Number, m.Verdict, monitorWhy(m, t.Spec.Origin, r.Cfg.RoomsURL), true, demotes))
			}
		}
		m.Posted = m.Verdict != "regression" || ended // a regression is posted at once and watched on for follow-ups
	}
	if m.Posted {
		r.mon.forget(t.Name) // nothing is due again
	} else {
		r.mon.passed(t.Name, r.Now())
	}
	// AM9, R-I6: refresh the class's class_paused gauge, so a class with no decision still reports.
	// paused records it; a failed read leaves the last value and never fails the pass.
	if _, err := r.paused(ctx, r.repo(t), t.Spec.PredictedClass); err != nil {
		r.log().Warn("class_paused refresh failed", "task.id", t.Name, "class", t.Spec.PredictedClass, "err", err)
	}
	return nil
}

// monitorWhy lists the signals the verdict read, for the comment.
func monitorWhy(m *v1alpha1.MonitorStatus, origin, roomsURL string) []string {
	out := []string{"main CI: " + cmpOpen(m.Main), fmt.Sprintf("follow_ups=%d", len(m.FollowUps))}
	if m.Oracle != "" {
		out = append(out, "detector: "+m.Oracle)
	}
	if m.Flux != "" {
		out = append(out, "Flux: "+m.Flux)
		if len(m.FluxBlamed) > 0 {
			out = append(out, fmt.Sprintf("not Ready since the merge: %v", m.FluxBlamed))
		}
	}
	if v := m.Verification; v != nil && m.Verified != "" {
		out = append(out, fmt.Sprintf("RunLore: %s, alert `%s`, %s/r/%s", v.Verdict, finding.LabelSafe(v.Alert.Name),
			strings.TrimSuffix(roomsURL, "/"), origin))
	}
	if len(m.FollowUps) > 0 {
		out = append(out, fmt.Sprintf("follow-up fixes: %v, counted by the breaker, not in this verdict", m.FollowUps))
	}
	if m.FollowUpsUnread {
		out = append(out, "follow-up reads incomplete")
	}
	return out
}

// verifiedBy: the merge's task came from an investigate task of an internal principal that
// verifies (R12). Anything else (no origin, a sponsored principal, an unreadable origin) is no signal.
func (r *Reconciler) verifiedBy(ctx context.Context, t *v1alpha1.Task) bool {
	if t.Spec.Origin == "" {
		return false
	}
	var o v1alpha1.Task
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: t.Namespace, Name: t.Spec.Origin}, &o); err != nil {
		return false
	}
	ip := r.Cfg.InternalBy(o.Spec.Source.RequestedBy)
	return ip != nil && ip.Verification
}

// fileFollowUp files RA12's stub once, on the pass that first reads persisting. A failed filing is
// logged and tried again by the next pass; the record is the Task's status, written with the pass.
func (r *Reconciler) fileFollowUp(ctx context.Context, t *v1alpha1.Task) {
	m, ref := t.Status.Monitor, t.Status.PullRequest
	title, body := narrate.FollowUpIssue(t.Name, ref.Number, m.Verification.Alert.Name, t.Spec.Origin, r.Cfg.RoomsURL)
	// The issue may exist already (its status write was lost, or its create timed out): adopt it by its marker.
	marker := narrate.FollowUpMarker(t.Name)
	// Only issues updated since the merge are read: factory/proposed accumulates, and one page of all of
	// them would stop filing for good. A listing that fails files anyway: a stray duplicate stub beats
	// a verdict nobody is told about.
	found, err := r.gh(ctx).IssuesLabelledSince(ctx, "factory/proposed", m.Since.Time)
	if err != nil {
		r.log().Warn("RunLore follow-up issue lookup failed; filing without it", "task.id", t.Name, "err", err)
	}
	for _, i := range found {
		if forge.SameLogin(i.Author, r.Cfg.FactoryLogin) && strings.Contains(i.Body, marker) {
			m.FollowUpIssue = i.Number
			return
		}
	}
	iss, err := r.gh(ctx).CreateIssue(ctx, title, body, []string{"factory/proposed"})
	if err != nil {
		r.log().Warn("RunLore follow-up issue not filed", "task.id", t.Name, "err", err)
		return
	}
	m.FollowUpIssue = iss.Number
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

// oracleWait is how long after the merge the detector's runs are waited for: a nightly detector
// that has not run twice by then did not say.
const oracleWait = 72 * time.Hour

// oracleSignal reads the detector's scheduled runs after the merge, oldest first (R37), each once:
// a run that reports a target the merge wrote as gone is a regression; a run that did not succeed
// or whose findings are refused is inconclusive; OracleRuns clean runs are healthy. Any read that
// fails leaves the signal open, so the next pass reads again; open after oracleWait is inconclusive.
func (r *Reconciler) oracleSignal(ctx context.Context, t *v1alpha1.Task, cl config.Class) string {
	m, rc := t.Status.Monitor, r.repo(t)
	if cl.Oracle == nil { // config refuses oracleRuns without an oracle; never read a workflow of nothing
		return "inconclusive"
	}
	runs, err := r.merger(ctx).WorkflowRuns(ctx, cl.Oracle.Workflow)
	if err != nil {
		r.log().Warn("detector runs read failed", "task.id", t.Name, "err", err)
		return r.oracleOpen(m)
	}
	var fresh []forge.WorkflowRun
	for _, run := range runs {
		if intake.DetectorRun(run, rc) && run.Created.After(m.Since.Time) && !slices.Contains(m.OracleRuns, run.ID) {
			fresh = append(fresh, run)
		}
	}
	slices.SortFunc(fresh, func(a, b forge.WorkflowRun) int { return a.Created.Compare(b.Created) })
	for _, run := range fresh {
		if run.Conclusion != "success" {
			return "inconclusive"
		}
		raw, err := r.merger(ctx).RunArtifact(ctx, run.ID, "findings", "findings.json")
		if err != nil {
			r.log().Warn("detector findings read failed", "task.id", t.Name, "run", run.ID, "err", err)
			return r.oracleOpen(m)
		}
		found, err := intake.ParseFindings(raw)
		if err != nil {
			r.log().Warn("detector findings refused", "task.id", t.Name, "run", run.ID, "err", err)
			return "inconclusive"
		}
		if wroteGone(t.Spec.Findings, found) {
			return "regression"
		}
		if m.OracleRuns = append(m.OracleRuns, run.ID); len(m.OracleRuns) >= cl.Monitor.OracleRuns {
			return "healthy"
		}
	}
	return r.oracleOpen(m)
}

// oracleOpen leaves the signal open until oracleWait has passed since the merge.
func (r *Reconciler) oracleOpen(m *v1alpha1.MonitorStatus) string {
	if r.Now().Sub(m.Since.Time) >= oracleWait {
		return "inconclusive"
	}
	return ""
}

// wroteGone: a run reports gone a target the task wrote, by its file and its archive or its URL.
// Matching the URL can count another, older occurrence of the same dead URL in that file.
func wroteGone(wrote []v1alpha1.Finding, run []intake.LinkFinding) bool {
	return slices.ContainsFunc(run, func(f intake.LinkFinding) bool {
		return intake.Gone(f.Status) && slices.ContainsFunc(wrote, func(w v1alpha1.Finding) bool {
			return w.File == f.File && (f.URL == w.URL || (w.Archive != "" && f.URL == w.Archive))
		})
	})
}

// fluxSignal is Flux on the factory's cluster (R33, R34). An empty not-ready list is no proof of
// health: an object that has not picked up the merge still reads Ready. So healthy needs the
// source to hold the merge commit, `within` to have passed since it fetched it, and nothing turned
// not Ready between the merge and `within` after the source first showed it (a later flap is never blamed).
// Any reader error leaves the signal open, so the window's end reads inconclusive.
//
// Known limits, by design (the spec's): a suspended source makes the signal inconclusive at any
// pass, even after a clean start; attribution is per source, so overlapping windows, or a later
// commit's failure inside the window, count against every open merge of that source.
func (r *Reconciler) fluxSignal(ctx context.Context, t *v1alpha1.Task, f config.FluxWatch) string {
	if r.Flux == nil {
		return "inconclusive"
	}
	ref, m := t.Status.PullRequest, t.Status.Monitor
	src, err := r.Flux.Source(ctx, f.Namespace(), f.Name())
	if err != nil {
		r.log().Warn("Flux source read failed", "task.id", t.Name, "err", err)
		return ""
	}
	if src.Suspended {
		return "inconclusive"
	}
	if m.Applied == nil {
		held := src.Revision == ref.MergeCommitSHA
		if !held {
			if held, err = r.merger(ctx).IsAncestor(ctx, ref.MergeCommitSHA, src.Revision); err != nil {
				r.log().Warn("Flux revision ancestry read failed", "task.id", t.Name, "err", err)
				return ""
			}
		}
		switch {
		case !held && r.Now().Sub(m.Since.Time) >= f.Within.Duration:
			return "inconclusive" // the source never delivered the merge in time
		case !held || src.At.IsZero():
			return ""
		}
		// The first time only: a later fetch is a later commit's. src.At is when the source fetched
		// what it first showed us, an upper bound on when the merge was applied: the blame window
		// and the settle time end `within` after it. Blame starts at src.At when the source shows
		// the merge commit itself, else at the merge's own time (mergedAt, m.Since): the merge was
		// fetched somewhere before src.At, and a failure from before the merge has an older
		// transition, so it is not blamed.
		at, from := metav1.NewTime(src.At), metav1.NewTime(src.At)
		if src.Revision != ref.MergeCommitSHA {
			from = m.Since
		}
		m.Applied, m.BlameFrom = &at, &from
	}
	from := m.Applied.Time
	if m.BlameFrom != nil {
		from = m.BlameFrom.Time
	}
	bad, err := r.Flux.NotReadySince(ctx, f.Namespace(), f.Name(), from, m.Applied.Add(f.Within.Duration))
	if err != nil {
		r.log().Warn("Flux readiness read failed", "task.id", t.Name, "err", err)
		return ""
	}
	if len(bad) > 0 {
		r.log().Warn("Flux objects turned not Ready after the merge", "task.id", t.Name, "objects", bad)
		bad = slices.Sorted(slices.Values(bad)) // the same five on every pass, whatever the reader's order
		m.FluxBlamed = bad[:min(len(bad), maxFluxBlamed)]
		return "regression"
	}
	if r.Now().Sub(m.Applied.Time) >= f.Within.Duration {
		return "healthy"
	}
	return ""
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
		if cl.Adopted() && slices.ContainsFunc(cl.Author.Adopt.Logins, func(l string) bool { return forge.SameLogin(p.Author, l) }) {
			continue // R35, M3: the lane's own next bump is not a follow-up of the merge it follows
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
