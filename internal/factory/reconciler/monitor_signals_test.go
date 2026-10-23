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

// fakeFlux serves a source and the objects turned not Ready; err fails both reads.
type fakeFlux struct {
	src      fluxwatch.Source
	notReady []string
	err      error
	since    time.Time // what NotReadySince was asked
}

func (f *fakeFlux) Source(context.Context, string, string) (fluxwatch.Source, error) {
	return f.src, f.err
}

func (f *fakeFlux) NotReadySince(_ context.Context, _, _ string, since time.Time) ([]string, error) {
	f.since = since
	return f.notReady, f.err
}

// fluxRig sets the reader only when there is one: a nil *fakeFlux in the interface is not a nil
// interface, and the monitor would call through it (M6).
func fluxRig(t *testing.T, f *fakeFlux) *rig {
	g := monitorRig(t)
	cl := g.r.Cfg.Classes["dead-links"]
	cl.Monitor.Flux = &config.FluxWatch{Source: "flux-system/flux-system", Within: config.Duration{Duration: time.Hour}}
	g.r.Cfg.Classes["dead-links"] = cl
	if f != nil {
		g.r.Flux = f
	}
	return g
}

// Offline proof 6: a HelmRelease turning not Ready is regression; a suspended source is
// inconclusive; all Ready for `within` after the source applied the merge is healthy. An empty
// not-ready list alone is never healthy.
func TestFluxSignal(t *testing.T) {
	applied := fluxwatch.Source{Revision: mSHA, At: now.Add(-90 * time.Minute)}
	for name, c := range map[string]struct {
		f    *fakeFlux
		want string
	}{
		"a HelmRelease turned not Ready": {&fakeFlux{src: applied, notReady: []string{"HelmRelease/tooling/grafana"}}, "regression"},
		"suspended":                      {&fakeFlux{src: fluxwatch.Source{Suspended: true}}, "inconclusive"},
		"never reached the merge":        {&fakeFlux{src: fluxwatch.Source{Revision: "older"}}, "inconclusive"},
		"all Ready":                      {&fakeFlux{src: applied}, "healthy"},
		"all Ready, but within not over": {&fakeFlux{src: fluxwatch.Source{Revision: mSHA, At: now.Add(-10 * time.Minute)}}, ""},
		"the source's time unknown":      {&fakeFlux{src: fluxwatch.Source{Revision: mSHA}}, ""},
		"an unreadable object":           {&fakeFlux{src: applied, err: errors.New("lastTransitionTime")}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			g := fluxRig(t, c.f)
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

// A reader that fails all window long ends inconclusive, never healthy; and once the source holds
// the merge, NotReadySince is asked from that moment, which a later fetch does not move.
func TestFluxSignalErrorsAndApplied(t *testing.T) {
	f := &fakeFlux{src: fluxwatch.Source{Revision: mSHA, At: now.Add(-10 * time.Minute)}}
	g := fluxRig(t, f)
	g.reconcile(t, "3buqdlot", 1)
	if !f.since.Equal(f.src.At) {
		t.Fatalf("not ready since the source applied the merge: %v", f.since)
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
	elsewhere := `[{"file":"website/content/docs/other.md","line":7,"url":"https://a","status":"404"}]`
	for name, c := range map[string]struct {
		runs []forge.WorkflowRun
		arts map[int64][]byte
		want string
	}{
		"a written target gone": {[]forge.WorkflowRun{run(2, now.Add(-time.Hour), "success")}, map[int64][]byte{2: []byte(written)}, "regression"},
		"two clean runs": {[]forge.WorkflowRun{run(3, now.Add(-time.Hour), "success"), run(2, now.Add(-90*time.Minute), "success"), run(1, now.Add(-3*time.Hour), "success")},
			map[int64][]byte{3: []byte("[]"), 2: []byte("[]"), 1: []byte(written)}, "healthy"},
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
