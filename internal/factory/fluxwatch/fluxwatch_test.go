// SPDX-License-Identifier: Apache-2.0

package fluxwatch

import (
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	ksV1  = "kustomize.toolkit.fluxcd.io/v1"
	hrV2  = "helm.toolkit.fluxcd.io/v2"
	srcV1 = "source.toolkit.fluxcd.io/v1"
)

var at = time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)

// fluxClient is a fake client that knows Flux's kinds: it resolves each kind's scope through its
// RESTMapper, and an empty scheme gives it none for them (M5).
func fluxClient(objs ...client.Object) client.Client {
	m := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"},
		{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"},
		{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository"},
	} {
		m.Add(gvk, meta.RESTScopeNamespace)
		m.Add(gvk.GroupVersion().WithKind(gvk.Kind+"List"), meta.RESTScopeNamespace)
	}
	return fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithRESTMapper(m).WithObjects(objs...).Build()
}

func obj(apiVersion, kind, ns, name, ready string, since time.Time) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(ns)
	u.SetName(name)
	_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"type": "Ready", "status": ready,
		"lastTransitionTime": since.Format(time.RFC3339)}}, "status", "conditions")
	return u
}

// sourced sets a Kustomization's spec.sourceRef in flux-system.
func sourced(u *unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	_ = unstructured.SetNestedMap(u.Object, map[string]any{"kind": kind, "name": name, "namespace": "flux-system"}, "spec", "sourceRef")
	return u
}

// ownedBy labels an object as kustomize-controller labels what a Kustomization applies.
func ownedBy(u *unstructured.Unstructured, ks string) *unstructured.Unstructured {
	u.SetLabels(map[string]string{"kustomize.toolkit.fluxcd.io/name": ks, "kustomize.toolkit.fluxcd.io/namespace": "flux-system"})
	return u
}

// R32, L7: an object the watched source delivers, turned not Ready after the source applied the
// merge, is blamed; one already failing before, Ready, Unknown (still reconciling), or delivered
// by another source (an AI child on OCIRepository/ai-native-ref) is not.
func TestNotReadySince(t *testing.T) {
	c := fluxClient(
		sourced(obj(ksV1, "Kustomization", "flux-system", "tooling", "True", at.Add(time.Minute)), "GitRepository", "flux-system"),
		sourced(obj(ksV1, "Kustomization", "flux-system", "apps", "False", at.Add(-time.Hour)), "GitRepository", "flux-system"),
		sourced(obj(ksV1, "Kustomization", "flux-system", "agent-factory", "False", at.Add(5*time.Minute)), "OCIRepository", "ai-native-ref"),
		ownedBy(obj(hrV2, "HelmRelease", "tooling", "grafana", "False", at.Add(5*time.Minute)), "tooling"),
		ownedBy(obj(hrV2, "HelmRelease", "apps", "x", "Unknown", at.Add(time.Minute)), "apps"),
		ownedBy(obj(hrV2, "HelmRelease", "agent-system", "agent-factory", "False", at.Add(5*time.Minute)), "agent-factory"),
	)
	got, err := Reader{C: c}.NotReadySince(t.Context(), "flux-system", "flux-system", at)
	if err != nil || !slices.Equal(got, []string{"HelmRelease/tooling/grafana"}) {
		t.Fatalf("%v %v", got, err)
	}
}

func TestSource(t *testing.T) {
	u := obj(srcV1, "GitRepository", "flux-system", "flux-system", "True", at)
	_ = unstructured.SetNestedField(u.Object, "main@sha1:abc", "status", "artifact", "revision")
	_ = unstructured.SetNestedField(u.Object, at.Format(time.RFC3339), "status", "artifact", "lastUpdateTime")
	_ = unstructured.SetNestedField(u.Object, true, "spec", "suspend")
	s, err := Reader{C: fluxClient(u)}.Source(t.Context(), "flux-system", "flux-system")
	if err != nil || s.Revision != "abc" || !s.Suspended || !s.At.Equal(at) {
		t.Fatalf("%+v %v", s, err)
	}
}
