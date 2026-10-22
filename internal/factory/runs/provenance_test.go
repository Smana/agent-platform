// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Smana/agent-platform/internal/version"
)

// R10: every claim names the factory build that created it, CREATE only; a read takes the
// CompositionRevision Crossplane bound it to and the harness image the composition read.
func TestProvenanceRoundTrip(t *testing.T) {
	u := Build(spec())
	if got := u.GetAnnotations()[AnnFactoryVersion]; got != version.Version {
		t.Fatalf("factory-version = %q, want %q", got, version.Version)
	}
	image := "ghcr.io/smana/agent-harness@sha256:" + strings.Repeat("c", 64)
	_ = unstructured.SetNestedField(u.Object, "xagentruns.cloud.ogenki.io-41b6efe", "spec", "crossplane", "compositionRevisionRef", "name")
	_ = unstructured.SetNestedField(u.Object, image, "status", "harnessImage")
	r, ok := FromUnstructured(u)
	if !ok || r.FactoryVersion != version.Version || r.CompositionRevision != "xagentruns.cloud.ogenki.io-41b6efe" || r.HarnessImage != image {
		t.Fatalf("%+v", r)
	}
	if err := (Client{}).Annotate(t.Context(), "7f3cq2xz", map[string]string{AnnFactoryVersion: "v9"}); err == nil {
		t.Fatal("factory-version is CREATE only")
	}
}
