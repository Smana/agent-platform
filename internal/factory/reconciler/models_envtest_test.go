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

// v3 F15, F4: the Task CRD holds a provider judge to a public task, at create and on a flip; the
// judge enum is C5's names and mistral-light only (F3).
func TestEnvtestAProviderJudgeIsPublicOnly(t *testing.T) {
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
	task := func(name, dataClass, judge string) *v1alpha1.Task {
		tk := issueTask(name, 7, "t")
		tk.CreationTimestamp = metav1.Time{}
		tk.Spec.DataClass = dataClass
		tk.Spec.Loop = &v1alpha1.Loop{StallSteps: 80, Continuations: 2, JudgeModel: judge}
		return tk
	}
	for name, c2 := range map[string]struct {
		t    *v1alpha1.Task
		want string
	}{
		"internal, mistral-light":  {task("aaaaaaaa", "internal", "mistral-light"), "a provider judgeModel is a public task's only"},
		"public, kimi-frontier":    {task("aaaaaaab", "public", "kimi-frontier"), "Unsupported value"},
		"public, mistral-standard": {task("aaaaaaac", "public", "mistral-standard"), "Unsupported value"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := c.Create(ctx, c2.t); err == nil || !strings.Contains(err.Error(), c2.want) {
				t.Fatalf("got %v, want %q", err, c2.want)
			}
		})
	}
	ok := task("aaaaaaad", "public", "mistral-light")
	if err := c.Create(ctx, ok); err != nil {
		t.Fatalf("a public task's mistral-light judge: %v", err)
	}
	if err := c.Create(ctx, task("aaaaaaae", "internal", "tier-light")); err != nil {
		t.Fatalf("an internal task's tier-light judge: %v", err)
	}
	ok.Spec.DataClass = "internal"
	if err := c.Update(ctx, ok); err == nil || !strings.Contains(err.Error(), "a provider judgeModel is a public task's only") {
		t.Fatalf("a flip to internal: %v", err)
	}
}
