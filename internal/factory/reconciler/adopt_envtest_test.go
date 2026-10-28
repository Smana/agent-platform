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

// M10, R39: an adopt Task, with no sponsor, no ceiling and no template, passes the Source rules,
// and its kind never changes once written.
func TestEnvtestAnAdoptedTask(t *testing.T) {
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
	tk := issueTask("aaaaaaae", 30, "Adopted pull request #30 from renovate[bot] (renovate/kyverno-3.x).")
	tk.CreationTimestamp = metav1.Time{}
	tk.Spec.Template = ""
	tk.Spec.Source = v1alpha1.Source{Kind: "adopt", Ref: "Smana/cloud-native-ref#30", Key: "adopt:Smana/cloud-native-ref#30",
		RequestedBy: "github:renovate[bot]", Trust: "untrusted", ContentSHA256: strings.Repeat("a", 64)}
	if err := c.Create(ctx, tk); err != nil {
		t.Fatalf("an adopted task is refused: %v", err)
	}
	tk.Spec.Source.Kind = "issue"
	if err := c.Update(ctx, tk); err == nil || !strings.Contains(err.Error(), "a task's source kind never changes") {
		t.Fatalf("got %v, want the kind rule", err)
	}
}
