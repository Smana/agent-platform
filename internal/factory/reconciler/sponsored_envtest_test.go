// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
)

// R5, R8, C1: the CRD installs with its new CEL rules, an api task needs its sponsor and ceiling,
// neither moves once written, and the splitter may still rewrite a task's snapshot hash.
func TestEnvtestSponsoredTask(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset: run `task test`, which sets it")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true}
	restCfg, err := env.Start()
	if err != nil {
		t.Fatal(err) // a CEL rule the apiserver cannot cost fails here
	}
	defer func() { _ = env.Stop() }()
	c, err := client.New(restCfg, client.Options{Scheme: scheme()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agent-system"}}); err != nil {
		t.Fatal(err)
	}
	task := func(name string, src v1alpha1.Source, maxUSD int64) *v1alpha1.Task {
		tk := issueTask(name, 7, "t")
		tk.CreationTimestamp = metav1.Time{}
		tk.Spec.Source, tk.Spec.MaxMicroUSD = src, maxUSD
		return tk
	}
	sum := strings.Repeat("a", 64)
	api := v1alpha1.Source{Kind: "api", Ref: "api:slack-app/k", Key: "k", RequestedBy: "system:slack-app",
		Sponsor: "github:smana", OnBehalfOf: "slack:T01ABC/U02DEF", Trust: "untrusted", ContentSHA256: sum}
	for name, c2 := range map[string]struct {
		t    *v1alpha1.Task
		want string
	}{
		"an api task without a sponsor": {task("aaaaaaaa", func() v1alpha1.Source { s := api; s.Sponsor = ""; return s }(), 5_000_000), "an api task names its sponsor"},
		"an api task without a ceiling": {task("aaaaaaab", api, 0), "an api task carries maxMicroUSD"},
		"a sponsor on an issue":         {task("aaaaaaac", v1alpha1.Source{Kind: "issue", Ref: "r", Key: "k", RequestedBy: "github:x", Sponsor: "github:smana", Trust: "untrusted", ContentSHA256: sum}, 0), "only an api task has a sponsor"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := c.Create(ctx, c2.t); err == nil || !strings.Contains(err.Error(), c2.want) {
				t.Fatalf("got %v, want %q", err, c2.want)
			}
		})
	}
	ok := task("aaaaaaad", api, 5_000_000)
	if err := c.Create(ctx, ok); err != nil {
		t.Fatal(err)
	}
	for name, e := range map[string]struct {
		edit func(*v1alpha1.Task)
		want string
	}{
		"a raised ceiling": {func(tk *v1alpha1.Task) { tk.Spec.MaxMicroUSD = 9_000_000 }, "maxMicroUSD never changes"},
		"a new sponsor":    {func(tk *v1alpha1.Task) { tk.Spec.Source.Sponsor = "github:alice" }, "source.sponsor never changes"},
		"a new link":       {func(tk *v1alpha1.Task) { tk.Spec.Source.OnBehalfOf = "slack:T01ABC/U09XYZ" }, "source.onBehalfOf never changes"},
		"no sponsor":       {func(tk *v1alpha1.Task) { tk.Spec.Source.Sponsor = "" }, "source.sponsor never changes"},
		"no link":          {func(tk *v1alpha1.Task) { tk.Spec.Source.OnBehalfOf = "" }, "source.onBehalfOf never changes"},
		"no ceiling":       {func(tk *v1alpha1.Task) { tk.Spec.MaxMicroUSD = 0 }, "an api task carries maxMicroUSD"},
	} {
		t.Run(name, func(t *testing.T) {
			tk := ok.DeepCopy()
			e.edit(tk)
			if err := c.Update(ctx, tk); err == nil || !strings.Contains(err.Error(), e.want) {
				t.Fatalf("got %v, want %q", err, e.want)
			}
		})
	}
	// C1: the splitter's inherit() rewrites a re-labelled part's text and hash; the source's other
	// fields hold, so the update goes through.
	ok.Spec.Text, ok.Spec.Source.ContentSHA256 = "u", strings.Repeat("c", 64)
	if err := c.Update(ctx, ok); err != nil {
		t.Fatalf("a new snapshot hash: %v", err)
	}
	// A status write on an api task goes through: the rules hold spec only.
	ok.Status.RetriedBy = "github:alice"
	if err := c.Status().Update(ctx, ok); err != nil {
		t.Fatalf("a status update: %v", err)
	}
	plain := task("aaaaaaae", v1alpha1.Source{Kind: "issue", Ref: "r", Key: "k", RequestedBy: "github:x", Trust: "untrusted", ContentSHA256: sum}, 0)
	plain.Spec.Issue = 0
	if err := c.Create(ctx, plain); err != nil {
		t.Fatal(err)
	}
	plain.Spec.Issue = 7 // the rules never bite a task without a ceiling or a sponsor
	if err := c.Update(ctx, plain); err != nil {
		t.Fatalf("a non-api task's update: %v", err)
	}
	for name, e := range map[string]struct {
		edit func(*v1alpha1.Task)
		want string
	}{
		"a sponsor added to a non-api task": {func(tk *v1alpha1.Task) { tk.Spec.Source.Sponsor = "github:smana" }, "only an api task has a sponsor"},
		"another kind":                      {func(tk *v1alpha1.Task) { tk.Spec.Source.Kind = "runlore" }, "a task's source kind never changes"},
	} {
		t.Run(name, func(t *testing.T) {
			tk := plain.DeepCopy()
			e.edit(tk)
			err := c.Update(ctx, tk)
			if err == nil || !strings.Contains(err.Error(), e.want) {
				t.Fatalf("got %v, want %q", err, e.want)
			}
			if name == "another kind" && strings.Count(err.Error(), "never changes")+strings.Count(err.Error(), "only an api") != 1 {
				t.Fatalf("more than the kind rule tripped: %v", err)
			}
		})
	}
}
