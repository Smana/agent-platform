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

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
)

// Splitter design SL5: the API server holds a child's parent fixed from creation and its scope fixed
// once set. Installing the CRD here proves the new rules' CEL cost is within the API server's budget.
func TestEnvtestAChildsParentAndScopeAreFixed(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset: run `task test`, which sets it")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true}
	restCfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
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
	child := func(name string, p v1alpha1.Parent) *v1alpha1.Task {
		tk := issueTask(name, 501, "# The whole task")
		tk.CreationTimestamp = metav1.Time{}
		tk.Spec.Parent = &p
		return tk
	}
	if err := c.Create(ctx, child("chldbadx", v1alpha1.Parent{Task: "3buqdlot", Issue: 7, Index: 3, Of: 2})); err == nil ||
		!strings.Contains(err.Error(), "a part's index is at most its count") {
		t.Fatalf("index past its count: %v", err)
	}
	tk := child("chldpart", v1alpha1.Parent{Task: "3buqdlot", Issue: 7, Index: 1, Of: 2})
	if err := c.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}
	tk.Spec.Scope = &v1alpha1.TaskScope{State: "ready", Task: "Part one.", Touches: []string{"docs/a.md"}, Plan: []string{"Edit a."},
		Acceptance: []v1alpha1.TaskAcceptance{{Check: "task docs", Proves: "the site builds"}}}
	if err := c.Update(ctx, tk); err != nil {
		t.Fatalf("a scope set once: %v", err)
	}
	for name, edit := range map[string]struct {
		f    func(*v1alpha1.Task)
		want string
	}{
		"the scope changed":  {func(t *v1alpha1.Task) { t.Spec.Scope.Task = "Part two." }, "spec.scope never changes once set"},
		"the scope removed":  {func(t *v1alpha1.Task) { t.Spec.Scope = nil }, "spec.scope never changes once set"},
		"the parent changed": {func(t *v1alpha1.Task) { t.Spec.Parent.Index = 2 }, "spec.parent is fixed at creation"},
		"the parent removed": {func(t *v1alpha1.Task) { t.Spec.Parent = nil }, "spec.parent is fixed at creation"},
	} {
		var cur v1alpha1.Task
		if err := c.Get(ctx, client.ObjectKeyFromObject(tk), &cur); err != nil {
			t.Fatal(err)
		}
		edit.f(&cur)
		if err := c.Update(ctx, &cur); err == nil || !strings.Contains(err.Error(), edit.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	plain := issueTask("plaintsk", 9, "x")
	plain.CreationTimestamp = metav1.Time{}
	if err := c.Create(ctx, plain); err != nil {
		t.Fatal(err)
	}
	plain.Spec.Parent = &v1alpha1.Parent{Task: "3buqdlot", Issue: 7, Index: 1, Of: 2}
	if err := c.Update(ctx, plain); err == nil || !strings.Contains(err.Error(), "spec.parent is fixed at creation") {
		t.Errorf("a parent added later: %v", err)
	}
}
