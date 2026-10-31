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
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/forge/forgetest"
	"github.com/Smana/maku/internal/factory/triage"
)

// An oracle class's decision carries a classification the Task CRD accepts: status.classification.fallback
// is a required enum, so an empty one made every status write of a scheduled dead-links task fail.
func TestEnvtestAnOracleClassClassificationIsAcceptedByTheCRD(t *testing.T) {
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
	cf := cfg()
	cf.Classes["dead-links"] = config.Class{Shadow: true, Oracle: &config.Oracle{Probe: "dead-links", Workflow: "dead-links.yaml", MaxFindings: 10}}
	tk := issueTask("orcl2345", 0, "x")
	tk.CreationTimestamp = metav1.Time{}
	tk.Spec.Source = v1alpha1.Source{Kind: "schedule", Ref: "dead-links", Key: "k", RequestedBy: "schedule:dead-links", Trust: "trusted", ContentSHA256: strings.Repeat("b", 64)}
	tk.Spec.PredictedClass = "dead-links"
	if err := c.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}
	d, err := triage.Classify{Cfg: cf, Forge: forgetest.NewFake()}.Triage(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	tk.Status.Classification = &d.Classification
	if err := c.Status().Update(ctx, tk); err != nil {
		t.Fatalf("status write refused: %v", err)
	}
}
