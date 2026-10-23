// SPDX-License-Identifier: Apache-2.0

// Package fluxwatch reads Flux's objects on the factory's cluster for the post-merge monitor
// (merge gate classes MC7): uncached, read-only, `unstructured`, so the factory carries no Flux
// API dependency.
package fluxwatch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	kustomizationList = schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "KustomizationList"}
	helmReleaseList   = schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmReleaseList"}
	gitRepository     = schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository"}
)

const (
	ownerName      = "kustomize.toolkit.fluxcd.io/name"
	ownerNamespace = "kustomize.toolkit.fluxcd.io/namespace"
)

// Reader reads Flux's objects through a client that need not know Flux's types.
type Reader struct{ C client.Reader }

// ErrNoArtifact is a GitRepository that has fetched nothing yet: no revision to compare a merge to.
var ErrNoArtifact = errors.New("fluxwatch: the GitRepository has no artifact")

// ErrRevision is an artifact revision that is not `<ref>@sha1:<commit>`, such as a sha256 one: it
// cannot be matched to a merge commit, so it is refused rather than returned whole.
var ErrRevision = errors.New("fluxwatch: the artifact revision is not a sha1 commit")

// Source is a GitRepository as the monitor needs it: whether it is suspended, the commit it
// delivers and when it last fetched it.
type Source struct {
	Suspended bool
	Revision  string
	At        time.Time
}

// Source reads the GitRepository ns/name. Its revision is the commit after `@sha1:`, or the whole
// value never: anything else (no artifact, no `@sha1:`) is an error.
func (r Reader) Source(ctx context.Context, ns, name string) (Source, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gitRepository)
	if err := r.C.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, u); err != nil {
		return Source{}, fmt.Errorf("fluxwatch: get GitRepository %s/%s: %w", ns, name, err)
	}
	var s Source
	var err error
	if s.Suspended, _, err = unstructured.NestedBool(u.Object, "spec", "suspend"); err != nil {
		return Source{}, fmt.Errorf("fluxwatch: GitRepository %s/%s spec.suspend: %w", ns, name, err)
	}
	rev, _, err := unstructured.NestedString(u.Object, "status", "artifact", "revision")
	if err != nil {
		return Source{}, fmt.Errorf("fluxwatch: GitRepository %s/%s artifact revision: %w", ns, name, err)
	}
	if rev == "" {
		return Source{}, fmt.Errorf("%w: %s/%s", ErrNoArtifact, ns, name)
	}
	_, commit, ok := strings.Cut(rev, "@sha1:")
	if !ok || commit == "" {
		return Source{}, fmt.Errorf("%w: %s/%s has %q", ErrRevision, ns, name, rev)
	}
	s.Revision = commit
	if ts, _, _ := unstructured.NestedString(u.Object, "status", "artifact", "lastUpdateTime"); ts != "" {
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return Source{}, fmt.Errorf("fluxwatch: GitRepository %s/%s lastUpdateTime %q: %w", ns, name, ts, err)
		}
		s.At = t
	}
	return s, nil
}

// NotReadySince lists, as kind/namespace/name sorted, the Kustomizations and HelmReleases that the
// GitRepository ns/name delivers and whose Ready condition turned False after since (L7). A
// Kustomization is delivered by it when its spec.sourceRef names it (a missing namespace is the
// Kustomization's own); a HelmRelease when kustomize-controller's labels name one of those. What an
// AI child on another source (OCIRepository/ai-native-ref) applies is never blamed on a host merge.
func (r Reader) NotReadySince(ctx context.Context, ns, name string, since time.Time) ([]string, error) {
	ks := &unstructured.UnstructuredList{}
	ks.SetGroupVersionKind(kustomizationList)
	if err := r.C.List(ctx, ks); err != nil {
		return nil, fmt.Errorf("fluxwatch: list Kustomizations: %w", err)
	}
	delivered := map[[2]string]bool{}
	var out []string
	for i := range ks.Items {
		k := &ks.Items[i]
		kind, _, _ := unstructured.NestedString(k.Object, "spec", "sourceRef", "kind")
		sn, _, _ := unstructured.NestedString(k.Object, "spec", "sourceRef", "name")
		sns, _, _ := unstructured.NestedString(k.Object, "spec", "sourceRef", "namespace")
		if sns == "" {
			sns = k.GetNamespace()
		}
		if kind != "GitRepository" || sn != name || sns != ns {
			continue
		}
		delivered[[2]string{k.GetNamespace(), k.GetName()}] = true
		bad, err := notReadySince(k, since)
		if err != nil {
			return nil, err
		}
		if bad {
			out = append(out, "Kustomization/"+k.GetNamespace()+"/"+k.GetName())
		}
	}
	hr := &unstructured.UnstructuredList{}
	hr.SetGroupVersionKind(helmReleaseList)
	if err := r.C.List(ctx, hr); err != nil {
		return nil, fmt.Errorf("fluxwatch: list HelmReleases: %w", err)
	}
	for i := range hr.Items {
		h := &hr.Items[i]
		l := h.GetLabels()
		if !delivered[[2]string{l[ownerNamespace], l[ownerName]}] {
			continue
		}
		bad, err := notReadySince(h, since)
		if err != nil {
			return nil, err
		}
		if bad {
			out = append(out, "HelmRelease/"+h.GetNamespace()+"/"+h.GetName())
		}
	}
	slices.Sort(out)
	return out, nil
}

// notReadySince: the object's Ready condition is False and turned so after since. A False one whose
// time is missing or unparsable is an error: it cannot be told apart from a fresh failure.
//
// Only Ready=False counts: an object that has not yet picked up the merge still reads its old
// Ready=True, and a stuck Unknown is never blamed, so an empty list alone is not "healthy" — the
// caller also checks the Source's revision and settle time.
func notReadySince(u *unstructured.Unstructured, since time.Time) (bool, error) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != "Ready" || m["status"] != "False" {
			continue
		}
		ts, _ := m["lastTransitionTime"].(string)
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return false, fmt.Errorf("fluxwatch: %s %s/%s is not Ready with lastTransitionTime %q: %w", u.GetKind(), u.GetNamespace(), u.GetName(), ts, err)
		}
		return t.After(since), nil
	}
	return false, nil
}
