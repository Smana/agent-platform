// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
)

// R10: the real API server keeps a run's provenance through a status write, and bounds its models.
func TestEnvtestRunProvenance(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset: run `task test`, which sets it")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true}
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
	tk := issueTask("3buqdlot", 7, "x")
	if err := c.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}
	want := v1alpha1.RunProvenance{FactoryVersion: "v0.16.0", CompositionRevision: "xagentruns.cloud.ogenki.io-41b6efe",
		HarnessImage: "ghcr.io/smana/maku-harness@sha256:" + strings.Repeat("c", 64), ServedModels: []string{"claude-opus-5-5", "glm-5.3"}}
	tk.Status.Phase = v1alpha1.PhaseImplementing
	tk.Status.Runs = []v1alpha1.RunRecord{{ID: rid(0), Role: "implementer", Trigger: "initial", Provenance: want}}
	if err := c.Status().Update(ctx, tk); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.Task
	if err := c.Get(ctx, types.NamespacedName{Namespace: "agent-system", Name: "3buqdlot"}, &got); err != nil {
		t.Fatal(err)
	}
	p := got.Status.Runs[0].Provenance
	if p.FactoryVersion != want.FactoryVersion || p.CompositionRevision != want.CompositionRevision ||
		p.HarnessImage != want.HarnessImage || !slices.Equal(p.ServedModels, want.ServedModels) {
		t.Fatalf("the API server changed it: %+v", p)
	}
	got.Status.Runs[0].Provenance.ServedModels = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	if err := c.Status().Update(ctx, &got); err == nil || !strings.Contains(err.Error(), "must have at most 8 items") {
		t.Fatalf("a ninth served model: %v", err)
	}
}

// The API server bounds the scope fields: an unknown mode and a malformed blob sha are refused.
func TestEnvtestScopeFieldsAreBounded(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset: run `task test`, which sets it")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true}
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
	st := &v1alpha1.Steering{Path: ".github/agent-factory/steering/review.md", SHA: strings.Repeat("5", 40), Text: "## Verify\n- x\n"}
	for name, mut := range map[string]func(*v1alpha1.Task){
		"a bogus mode": func(tk *v1alpha1.Task) { tk.Spec.ScopeMode = "bogus" },
		"a bad sha": func(tk *v1alpha1.Task) {
			tk.Spec.ScopeMode, tk.Spec.Steering = "steering", &v1alpha1.Steering{Path: st.Path, SHA: "XYZ", Text: st.Text}
		},
	} {
		tk := issueTask("3buqdlot", 7, "x")
		mut(tk)
		if err := c.Create(ctx, tk); err == nil {
			t.Errorf("%s: the API server accepted it", name)
		}
	}
	ok := issueTask("4buqdlot", 8, "x")
	ok.Spec.ScopeMode, ok.Spec.Steering = "steering", st
	if err := c.Create(ctx, ok); err != nil {
		t.Fatalf("a valid scope: %v", err)
	}
}
