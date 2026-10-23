// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/fluxwatch"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

// fakeFlux serves a source and the objects that turned not Ready, each at its time; NotReadySince
// answers those in (since, until], as the reader does. err fails both reads.
type fakeFlux struct {
	src          fluxwatch.Source
	flaps        map[string]time.Time
	err          error
	since, until time.Time // what NotReadySince was asked
}

func (f *fakeFlux) Source(context.Context, string, string) (fluxwatch.Source, error) {
	return f.src, f.err
}

func (f *fakeFlux) NotReadySince(_ context.Context, _, _ string, since, until time.Time) ([]string, error) {
	f.since, f.until = since, until
	var out []string
	for n, at := range f.flaps {
		if at.After(since) && !at.After(until) {
			out = append(out, n)
		}
	}
	return out, f.err
}

// fluxRig sets the reader only when there is one: a nil *fakeFlux in the interface is not a nil
// interface, and the monitor would call through it (M6). within is 40 minutes: the merge is two
// hours old, so a source that applied it 50 minutes ago has settled.
func fluxRig(t *testing.T, f *fakeFlux) *rig {
	g := monitorRig(t)
	cl := g.r.Cfg.Classes["dead-links"]
	cl.Monitor.Flux = &config.FluxWatch{Source: "flux-system/flux-system", Within: config.Duration{Duration: 40 * time.Minute}}
	g.r.Cfg.Classes["dead-links"] = cl
	if f != nil {
		g.r.Flux = f
	}
	return g
}

const hr = "HelmRelease/tooling/grafana"

// Offline proof 6: a HelmRelease turning not Ready is regression; a suspended source is
// inconclusive; all Ready for `within` after the source applied the merge is healthy. An empty
// not-ready list alone is never healthy.
func TestFluxSignal(t *testing.T) {
	applied := fluxwatch.Source{Revision: mSHA, At: now.Add(-50 * time.Minute)} // after the merge (now-2h)
	for name, c := range map[string]struct {
		f    *fakeFlux
		want string
	}{
		"a HelmRelease turned not Ready": {&fakeFlux{src: applied, flaps: map[string]time.Time{hr: applied.At.Add(5 * time.Minute)}}, "regression"},
		"suspended":                      {&fakeFlux{src: fluxwatch.Source{Suspended: true}}, "inconclusive"},
		"never reached the merge":        {&fakeFlux{src: fluxwatch.Source{Revision: "older"}}, "inconclusive"},
		"all Ready":                      {&fakeFlux{src: applied}, "healthy"},
		"all Ready, but within not over": {&fakeFlux{src: fluxwatch.Source{Revision: mSHA, At: now.Add(-10 * time.Minute)}}, ""},
		"the source's time unknown":      {&fakeFlux{src: fluxwatch.Source{Revision: mSHA}}, ""},
		"an unreadable object":           {&fakeFlux{src: applied, err: errors.New("lastTransitionTime")}, ""},
		// I1: the blame window closes at Applied+within; a pass after it must not blame a later flap.
		"a flap after within, read late": {&fakeFlux{src: applied, flaps: map[string]time.Time{hr: now.Add(-5 * time.Minute)}}, "healthy"},
		// IsAncestor true: the source moved on past the merge.
		"a descendant, all Ready": {&fakeFlux{src: fluxwatch.Source{Revision: "later", At: now.Add(-5 * time.Minute)}}, "healthy"},
		// I2: Applied is the merge's own time (Since = now-2h) when the revision is a descendant: an
		// object that went False after the merge and before the first observed fetch is caught.
		"a descendant, a flap before the fetch": {&fakeFlux{src: fluxwatch.Source{Revision: "later", At: now.Add(-5 * time.Minute)},
			flaps: map[string]time.Time{hr: now.Add(-100 * time.Minute)}}, "regression"},
	} {
		t.Run(name, func(t *testing.T) {
			g := fluxRig(t, c.f)
			g.f.SetAncestors("later", mSHA)
			tk := g.reconcile(t, "3buqdlot", 1)
			if tk.Status.Monitor.Flux != c.want {
				t.Fatalf("%+v", tk.Status.Monitor)
			}
			if c.want == "" && tk.Status.Monitor.Verdict != "" {
				t.Fatalf("an open signal posts no verdict: %+v", tk.Status.Monitor)
			}
		})
	}
	if tk := fluxRig(t, nil).reconcile(t, "3buqdlot", 1); tk.Status.Monitor.Flux != "inconclusive" {
		t.Fatal("no reader (the chart's value off): inconclusive")
	}
}

// A reader that fails all window long ends inconclusive, never healthy; once the source holds the
// merge, NotReadySince is asked from that moment to within later, which a later fetch does not move.
func TestFluxSignalErrorsAndApplied(t *testing.T) {
	f := &fakeFlux{src: fluxwatch.Source{Revision: mSHA, At: now.Add(-10 * time.Minute)}}
	g := fluxRig(t, f)
	g.reconcile(t, "3buqdlot", 1)
	if !f.since.Equal(f.src.At) || !f.until.Equal(f.src.At.Add(40*time.Minute)) {
		t.Fatalf("blamed from the source's apply to within later: %v..%v", f.since, f.until)
	}
	f.src.At = now.Add(time.Hour) // a later commit's fetch
	g.r.Now = func() time.Time { return now.Add(2 * time.Hour) }
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Monitor.Flux != "healthy" || !f.since.Equal(now.Add(-10*time.Minute)) {
		t.Fatalf("applied is first-time only: %+v since %v", tk.Status.Monitor, f.since)
	}

	e := fluxRig(t, &fakeFlux{err: errors.New("forbidden")})
	e.reconcile(t, "3buqdlot", 1)
	e.r.Now = func() time.Time { return now.Add(170 * time.Hour) }
	if tk := e.reconcile(t, "3buqdlot", 1); tk.Status.Monitor.Flux != "inconclusive" || tk.Status.Monitor.Verdict != "inconclusive" {
		t.Fatalf("a reader that never answered is inconclusive at the window's end: %+v", tk.Status.Monitor)
	}
}

// R37: a scheduled run after the merge that reports a written target gone is a regression; two
// clean runs are healthy; a failed run is inconclusive.
func TestOracleSignal(t *testing.T) {
	run := func(id int64, at time.Time, conclusion string) forge.WorkflowRun {
		return forge.WorkflowRun{ID: id, RunAttempt: 1, Event: "schedule", Branch: "main", Conclusion: conclusion, Created: at}
	}
	written := `[{"file":"website/content/docs/a.md","line":7,"url":"https://web.archive.org/web/2020/https://a","status":"404"}]`
	byURL := `[{"file":"website/content/docs/a.md","line":7,"url":"https://a","status":"410"}]`
	elsewhere := `[{"file":"website/content/docs/other.md","line":7,"url":"https://a","status":"404"}]`
	for name, c := range map[string]struct {
		runs []forge.WorkflowRun
		arts map[int64][]byte
		want string
	}{
		"a written target gone": {[]forge.WorkflowRun{run(2, now.Add(-time.Hour), "success")}, map[int64][]byte{2: []byte(written)}, "regression"},
		"two clean runs": {[]forge.WorkflowRun{run(3, now.Add(-time.Hour), "success"), run(2, now.Add(-90*time.Minute), "success"), run(1, now.Add(-3*time.Hour), "success")},
			map[int64][]byte{3: []byte("[]"), 2: []byte("[]"), 1: []byte(written)}, "healthy"},
		"a written URL gone":   {[]forge.WorkflowRun{run(2, now.Add(-time.Hour), "success")}, map[int64][]byte{2: []byte(byURL)}, "regression"},
		"an unread artifact":   {[]forge.WorkflowRun{run(2, now.Add(-time.Hour), "success")}, nil, ""},
		"another file gone":    {[]forge.WorkflowRun{run(2, now.Add(-time.Hour), "success")}, map[int64][]byte{2: []byte(elsewhere)}, ""},
		"one clean run":        {[]forge.WorkflowRun{run(2, now.Add(-time.Hour), "success")}, map[int64][]byte{2: []byte("[]")}, ""},
		"a failed run":         {[]forge.WorkflowRun{run(2, now.Add(-time.Hour), "failure")}, nil, "inconclusive"},
		"a refused artifact":   {[]forge.WorkflowRun{run(2, now.Add(-time.Hour), "success")}, map[int64][]byte{2: []byte(`[{"nope":1}]`)}, "inconclusive"},
		"a pull request's run": {[]forge.WorkflowRun{{ID: 2, Event: "pull_request", Branch: "x", Conclusion: "success", Created: now.Add(-time.Hour)}}, nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			g := monitorRig(t)
			cl := g.r.Cfg.Classes["dead-links"]
			cl.Monitor.OracleRuns = 2
			cl.Oracle = &config.Oracle{Probe: "dead-links", Workflow: "dead-links.yaml", MaxFindings: 10}
			g.r.Cfg.Classes["dead-links"] = cl
			g.patch(t, func(tk *v1alpha1.Task) {
				tk.Spec.Findings = []v1alpha1.Finding{{File: testFile, URL: "https://a", Archive: "https://web.archive.org/web/2020/https://a"}}
			})
			g.f.SetRuns(c.runs, c.arts)
			tk := g.reconcile(t, "3buqdlot", 1)
			if tk.Status.Monitor.Oracle != c.want {
				t.Fatalf("%+v", tk.Status.Monitor)
			}
		})
	}
}

// An oracle read that fails leaves the signal open, and the watch ends inconclusive, never healthy.
func TestOracleReadErrorEndsInconclusive(t *testing.T) {
	g := monitorRig(t)
	cl := g.r.Cfg.Classes["dead-links"]
	cl.Monitor.OracleRuns = 1
	cl.Oracle = &config.Oracle{Probe: "dead-links", Workflow: "dead-links.yaml", MaxFindings: 10}
	g.r.Cfg.Classes["dead-links"] = cl
	g.f.SetRuns([]forge.WorkflowRun{{ID: 2, RunAttempt: 1, Event: "schedule", Branch: "main", Conclusion: "success", Created: now.Add(-time.Hour)}}, nil)
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Monitor.Oracle != "" {
		t.Fatalf("open while the read fails: %+v", tk.Status.Monitor)
	}
	g.r.Now = func() time.Time { return now.Add(170 * time.Hour) }
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Monitor.Oracle != "inconclusive" || tk.Status.Monitor.Verdict != "inconclusive" {
		t.Fatalf("%+v", tk.Status.Monitor)
	}
}
