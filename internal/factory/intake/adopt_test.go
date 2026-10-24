// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/forge/forgetest"
	"github.com/Smana/agent-platform/internal/factory/taskid"
)

const (
	adoptRepo = "Smana/cloud-native-ref"
	digA      = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digB      = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func renovatePR(n int, ref string) forge.PRSummary {
	return forge.PRSummary{Number: n, Author: "renovate[bot]", HeadRef: ref, HeadSHA: "h" + fmt.Sprint(n)}
}

func adoptClass(shape string, live bool) config.Class {
	return config.Class{Live: live, Shadow: !live, DiffShape: shape, ModifyOnly: true, MaxPatchFiles: 3, MaxModifiedLines: 10,
		Author: &config.Author{Adopt: &config.Adopt{Logins: []string{"renovate[bot]"}, Branches: []string{"^renovate/"}}}}
}

func adoptCfg(minorOn bool) *config.Config {
	cls := map[string]config.Class{"deps-patch": adoptClass("semver-patch", false)}
	if minorOn {
		cls["deps-minor"] = adoptClass("semver-minor", false)
	}
	return &config.Config{Repository: adoptRepo, Defaults: config.Defaults{DataClass: "internal"}, Classes: cls}
}

func adoptScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	return s
}

func adopter(f *forgetest.Fake, c client.Client, cfg *config.Config) *Adopter {
	return &Adopter{Clients: func(string) (ScheduleForge, ScheduleMerger, bool) { return f, f, true }, Client: c,
		Namespace: "agent-system", Cfg: cfg, Owned: func(string) bool { return true },
		Stopped: func(context.Context) bool { return false }, Now: func() time.Time { return time.Date(2026, 10, 5, 6, 20, 0, 0, time.UTC) },
		Errors: &counter{}}
}

func adoptedTasks(t *testing.T, c client.Client) []v1alpha1.Task {
	t.Helper()
	var l v1alpha1.TaskList
	if err := c.List(context.Background(), &l); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

// MC10, R39: an open Renovate PR whose diff passes semver-patch becomes one run-less Task of
// deps-patch; a minor goes to deps-minor when it is declared on; anything else, another author,
// another branch, a class off, or the 11th open adopted Task, creates nothing. A second tick
// creates nothing more (the name is the dedup).
func TestAdopter(t *testing.T) {
	for name, c := range map[string]struct {
		pr      forge.PRSummary
		base    string
		head    string
		minorOn bool
		want    string // the class, "" for no Task
	}{
		"a patch":        {renovatePR(30, "renovate/kyverno-3.x"), "version: 3.9.1\n", "version: 3.9.2\n", false, "deps-patch"},
		"a minor, off":   {renovatePR(31, "renovate/es-2.x"), "version: 2.12.4\n", "version: 2.13.0\n", false, ""},
		"a minor, on":    {renovatePR(31, "renovate/es-2.x"), "version: 2.12.4\n", "version: 2.13.0\n", true, "deps-minor"},
		"a digest":       {renovatePR(32, "renovate/x-digest"), "x@" + digA + "\n", "x@" + digB + "\n", false, ""},
		"a human's PR":   {forge.PRSummary{Number: 33, Author: "Smana", HeadRef: "renovate/x", HeadSHA: "h"}, "v: 1.2.3\n", "v: 1.2.4\n", false, ""},
		"another branch": {forge.PRSummary{Number: 34, Author: "renovate[bot]", HeadRef: "main-x", HeadSHA: "h"}, "v: 1.2.3\n", "v: 1.2.4\n", false, ""},
		"a prefix is literal": {forge.PRSummary{Number: 35, Author: "renovate[bot]", HeadRef: "xrenovate/x", HeadSHA: "h"},
			"v: 1.2.3\n", "v: 1.2.4\n", false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := forgetest.NewFake()
			f.SetOpenPRs(c.pr)
			f.SetFiles(map[string]string{"a.yaml": c.base}, map[string]string{"a.yaml": c.head})
			a := adopter(f, fake.NewClientBuilder().WithScheme(adoptScheme()).Build(), adoptCfg(c.minorOn))
			for range 2 {
				if err := a.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			got := adoptedTasks(t, a.Client)
			if c.want == "" {
				if len(got) != 0 {
					t.Fatalf("no task, got %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("one task after two ticks, got %d", len(got))
			}
			tk, key := got[0], fmt.Sprintf("adopt:%s#%d", adoptRepo, c.pr.Number)
			sp := tk.Spec
			if tk.Name != taskid.Name(key) || sp.Source.Kind != "adopt" || sp.Source.Key != key ||
				sp.Source.Ref != fmt.Sprintf("%s#%d", adoptRepo, c.pr.Number) || sp.Source.RequestedBy != "github:renovate[bot]" ||
				sp.Source.Trust != "untrusted" || len(sp.Source.ContentSHA256) != 64 || sp.PredictedClass != c.want ||
				sp.Template != "" || sp.DataClass != "internal" || sp.Repository != adoptRepo ||
				sp.Text != fmt.Sprintf("Adopted pull request #%d from renovate[bot] (%s).", c.pr.Number, c.pr.HeadRef) {
				t.Fatalf("%+v", sp)
			}
		})
	}
}

func TestAdopterOffAndStopped(t *testing.T) {
	for name, mod := range map[string]func(*config.Config, *Adopter){
		"neither live nor shadow": func(cfg *config.Config, _ *Adopter) {
			cl := cfg.Classes["deps-patch"]
			cl.Shadow, cl.Live = false, false
			cfg.Classes["deps-patch"] = cl
		},
		"the kill switch":  func(_ *config.Config, a *Adopter) { a.Stopped = func(context.Context) bool { return true } },
		"a repo not owned": func(_ *config.Config, a *Adopter) { a.Owned = func(string) bool { return false } },
	} {
		t.Run(name, func(t *testing.T) {
			f := forgetest.NewFake()
			f.SetOpenPRs(renovatePR(30, "renovate/kyverno-3.x"))
			f.SetFiles(map[string]string{"a.yaml": "version: 3.9.1\n"}, map[string]string{"a.yaml": "version: 3.9.2\n"})
			cfg := adoptCfg(false)
			a := adopter(f, fake.NewClientBuilder().WithScheme(adoptScheme()).Build(), cfg)
			mod(cfg, a)
			if err := a.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := len(adoptedTasks(t, a.Client)); n != 0 {
				t.Fatalf("%d tasks", n)
			}
		})
	}
}

// Ten open adopted Tasks block the eleventh; a terminal one does not count.
func TestAdopterBoundsOpenTasks(t *testing.T) {
	var seed []client.Object
	for i := 1; i <= maxOpenAdopted; i++ {
		seed = append(seed, adoptedTask(i, v1alpha1.PhaseAwaitingCI))
	}
	seed = append(seed, adoptedTask(99, v1alpha1.PhaseDone))
	f := forgetest.NewFake()
	f.SetOpenPRs(renovatePR(30, "renovate/kyverno-3.x"))
	f.SetFiles(map[string]string{"a.yaml": "version: 3.9.1\n"}, map[string]string{"a.yaml": "version: 3.9.2\n"})
	a := adopter(f, fake.NewClientBuilder().WithScheme(adoptScheme()).WithStatusSubresource(&v1alpha1.Task{}).WithObjects(seed...).Build(), adoptCfg(false))
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(adoptedTasks(t, a.Client)); n != maxOpenAdopted+1 {
		t.Fatalf("the 11th open adopted task is not created: %d", n)
	}
	// one of the ten ends: the next tick adopts.
	var tk v1alpha1.Task
	_ = a.Client.Get(context.Background(), client.ObjectKey{Namespace: "agent-system", Name: taskid.Name(adoptKey(1))}, &tk)
	tk.Status.Phase = v1alpha1.PhaseClosed
	if err := a.Client.Status().Update(context.Background(), &tk); err != nil {
		t.Fatal(err)
	}
	if err := a.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(adoptedTasks(t, a.Client)); n != maxOpenAdopted+2 {
		t.Fatalf("a free slot adopts: %d", n)
	}
}

func adoptKey(n int) string { return fmt.Sprintf("adopt:%s#%d", adoptRepo, n) }

func adoptedTask(n int, phase string) *v1alpha1.Task {
	tk := &v1alpha1.Task{Spec: v1alpha1.TaskSpec{Repository: adoptRepo, PredictedClass: "deps-patch",
		Source: v1alpha1.Source{Kind: "adopt", Key: adoptKey(n), Ref: fmt.Sprintf("%s#%d", adoptRepo, n)}}}
	tk.Name, tk.Namespace = taskid.Name(adoptKey(n)), "agent-system"
	tk.Status.Phase = phase
	return tk
}

// A PR whose Task already exists, in any phase (a terminal hand-off, a re-entry, a restart: a new
// Adopter has no memory) never gets a second Task.
func TestAdopterNeverOpensASecondTask(t *testing.T) {
	for _, phase := range []string{v1alpha1.PhaseAwaitingCI, v1alpha1.PhaseAwaitingHuman, v1alpha1.PhaseDone,
		v1alpha1.PhaseClosed, v1alpha1.PhaseEscalated, v1alpha1.PhaseRejected} {
		t.Run(phase, func(t *testing.T) {
			f := forgetest.NewFake()
			f.SetOpenPRs(renovatePR(30, "renovate/kyverno-3.x"))
			f.SetFiles(map[string]string{"a.yaml": "version: 3.9.1\n"}, map[string]string{"a.yaml": "version: 3.9.2\n"})
			c := fake.NewClientBuilder().WithScheme(adoptScheme()).WithObjects(adoptedTask(30, phase)).Build()
			for range 2 { // the second Adopter is a restart
				if err := adopter(f, c, adoptCfg(false)).Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if n := len(adoptedTasks(t, c)); n != 1 {
				t.Fatalf("%d tasks", n)
			}
		})
	}
	// a Task under a different name but the same key (a renamed scheme) is the same PR too.
	f := forgetest.NewFake()
	f.SetOpenPRs(renovatePR(30, "renovate/kyverno-3.x"))
	f.SetFiles(map[string]string{"a.yaml": "version: 3.9.1\n"}, map[string]string{"a.yaml": "version: 3.9.2\n"})
	other := adoptedTask(30, v1alpha1.PhaseDone)
	other.Name = "zzzzzzzz"
	c := fake.NewClientBuilder().WithScheme(adoptScheme()).WithObjects(other).Build()
	if err := adopter(f, c, adoptCfg(false)).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(adoptedTasks(t, c)); n != 1 {
		t.Fatalf("the key is the identity: %d tasks", n)
	}
}

// A failed or truncated list, and a diff too large to read, create nothing.
func TestAdopterFailsClosed(t *testing.T) {
	f := forgetest.NewFake()
	f.SetFiles(map[string]string{"a.yaml": "version: 3.9.1\n"}, map[string]string{"a.yaml": "version: 3.9.2\n"})
	var prs []forge.PRSummary
	for i := range 100 { // a full page: the rest is unread
		prs = append(prs, renovatePR(100+i, "renovate/x"))
	}
	f.SetOpenPRs(prs...)
	a := adopter(f, fake.NewClientBuilder().WithScheme(adoptScheme()).Build(), adoptCfg(false))
	if err := a.Tick(context.Background()); err == nil {
		t.Fatal("a truncated list is an error")
	}
	if n := len(adoptedTasks(t, a.Client)); n != 0 {
		t.Fatalf("a truncated list adopts nothing: %d", n)
	}

	f.SetOpenPRs(renovatePR(30, "renovate/kyverno-3.x"))
	f.SetDiffError(forge.ErrDiffTooLarge)
	if err := a.Tick(context.Background()); err != nil {
		t.Fatalf("a diff too large skips the PR: %v", err)
	}
	f.SetOpenPRs(renovatePR(31, "renovate/kyverno-3.x")) // a head not judged yet
	f.SetDiffError(errors.New("boom"))
	if err := a.Tick(context.Background()); err == nil {
		t.Fatal("a failed diff read is an error")
	}
	if n := len(adoptedTasks(t, a.Client)); n != 0 {
		t.Fatalf("%d tasks", n)
	}
}

// The runnable is leader-only.
func TestAdopterIsLeaderOnly(t *testing.T) {
	if !(&Adopter{}).NeedLeaderElection() {
		t.Fatal("two adopters would race")
	}
}
