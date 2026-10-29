// SPDX-License-Identifier: Apache-2.0

// Package migrategroup moves Rooms and Tasks from one API group to another: maku-broker
// migrate-group, the maku rebrand's one-shot. Kubernetes serves a kind under one group only, so each
// object is copied. The copy gets the original's spec, labels and annotations on create, behind
// v1alpha1.AnnotationMigrating, which keeps both controllers off it. Its status goes through the
// status subresource. Only once the copy reads back equal is the guard lifted, the original's
// finalizers released and the original deleted. Each object succeeds or fails on its own. A re-run
// finishes a copy it made (v1alpha1.AnnotationMigratedFrom) and refuses a same-named object it did
// not. A room's log stays in Postgres under the room's name, which the copy keeps.
package migrategroup

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Smana/maku/api/v1alpha1"
)

// Version is the one version both groups serve.
const Version = "v1alpha1"

// Kinds are moved in this order: a Task names its Room, never the reverse. AgentRuns are not
// moved: the window has none active, and a finished run's end is already in its room's log.
var Kinds = []string{"Room", "Task"}

// Outcome is what happened to one object, or to a kind the source group does not serve.
type Outcome string

// The outcomes, as the report spells them.
const (
	WouldCopy   Outcome = "would-copy"   // dry run: there is no copy yet
	WouldResume Outcome = "would-resume" // dry run: a copy an earlier run made, to finish
	Migrated    Outcome = "migrated"
	Resumed     Outcome = "resumed"    // a copy an earlier run made, now finished
	Conflict    Outcome = "conflict"   // a same-named object under the target group that no run copied from this one
	Failed      Outcome = "failed"     // the original is kept
	NotServed   Outcome = "not-served" // the source group serves no such kind: nothing to move
)

// Options is one run.
type Options struct {
	From, To  string
	Namespace string // empty: every namespace
	Apply     bool   // false: read and report, write nothing
}

// Result is one object's line in the report. Name is empty for a kind that is not served.
type Result struct {
	Kind, Namespace, Name string
	Outcome               Outcome
	Err                   error
}

// Report is every Result, Rooms first.
type Report []Result

// Count is how many results have outcome o.
func (r Report) Count(o Outcome) int {
	n := 0
	for _, x := range r {
		if x.Outcome == o {
			n++
		}
	}
	return n
}

// OK reports whether nothing conflicted or failed.
func (r Report) OK() bool { return r.Count(Conflict) == 0 && r.Count(Failed) == 0 }

var groupRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// Validate refuses a malformed or identical pair of groups.
func (o Options) Validate() error {
	var errs []error
	for _, g := range []struct{ flag, v string }{{"--from", o.From}, {"--to", o.To}} {
		if !groupRE.MatchString(g.v) {
			errs = append(errs, fmt.Errorf("%s %q is not an API group (a DNS subdomain with a dot)", g.flag, g.v))
		}
	}
	if o.From != "" && o.From == o.To {
		errs = append(errs, errors.New("--from and --to name the same group"))
	}
	return errors.Join(errs...)
}

// Run moves every kind in Kinds. Its error is a refused Options only: every object's fate is in
// the Report.
func Run(ctx context.Context, c client.Client, o Options) (Report, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	rep := Report{}
	for _, kind := range Kinds {
		rep = append(rep, moveKind(ctx, c, o, kind)...)
	}
	return rep, nil
}

func gvk(group, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: Version, Kind: kind}
}

func moveKind(ctx context.Context, c client.Client, o Options, kind string) []Result {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk(o.From, kind+"List"))
	var opts []client.ListOption
	if o.Namespace != "" {
		opts = append(opts, client.InNamespace(o.Namespace))
	}
	if err := c.List(ctx, list, opts...); err != nil {
		if meta.IsNoMatchError(err) {
			return []Result{{Kind: kind, Outcome: NotServed}}
		}
		return []Result{{Kind: kind, Outcome: Failed, Err: fmt.Errorf("list %s.%s: %w", kind, o.From, err)}}
	}
	out := make([]Result, 0, len(list.Items))
	for i := range list.Items {
		old := &list.Items[i]
		outcome, err := moveOne(ctx, c, o, kind, old)
		out = append(out, Result{Kind: kind, Namespace: old.GetNamespace(), Name: old.GetName(), Outcome: outcome, Err: err})
	}
	return out
}

func moveOne(ctx context.Context, c client.Client, o Options, kind string, old *unstructured.Unstructured) (Outcome, error) {
	from := string(old.GetUID())
	if from == "" {
		return Failed, errors.New("the original has no uid")
	}
	cp := &unstructured.Unstructured{}
	cp.SetGroupVersionKind(gvk(o.To, kind))
	err := c.Get(ctx, client.ObjectKeyFromObject(old), cp)
	switch {
	case apierrors.IsNotFound(err):
		if !o.Apply {
			return WouldCopy, nil
		}
		cp = copyOf(old, gvk(o.To, kind))
		if err := c.Create(ctx, cp); err != nil {
			return Failed, fmt.Errorf("create the copy: %w", err)
		}
		return finish(ctx, c, old, cp, Migrated)
	case err != nil:
		return Failed, fmt.Errorf("read the copy: %w", err)
	case cp.GetAnnotations()[v1alpha1.AnnotationMigratedFrom] != from:
		return Conflict, fmt.Errorf("a %s of this name exists under %s and was not copied from uid %s", kind, o.To, from)
	case !o.Apply:
		return WouldResume, nil
	}
	return finish(ctx, c, old, cp, Resumed)
}

// copyOf is the copy's first write: spec, labels and annotations, the original's uid and the guard.
// Never finalizers, owner references or status: the controllers and the server own those.
func copyOf(old *unstructured.Unstructured, to schema.GroupVersionKind) *unstructured.Unstructured {
	cp := &unstructured.Unstructured{Object: map[string]any{}}
	cp.SetGroupVersionKind(to)
	cp.SetNamespace(old.GetNamespace())
	cp.SetName(old.GetName())
	cp.SetLabels(old.GetLabels())
	ann := make(map[string]string, len(old.GetAnnotations())+2)
	for k, v := range old.GetAnnotations() {
		ann[k] = v
	}
	ann[v1alpha1.AnnotationMigratedFrom] = string(old.GetUID())
	ann[v1alpha1.AnnotationMigrating] = "true"
	cp.SetAnnotations(ann)
	if spec, ok := old.Object["spec"]; ok {
		cp.Object["spec"] = runtime.DeepCopyJSONValue(spec)
	}
	return cp
}

// finish writes the status, confirms the copy, lifts the guard, then releases and deletes the
// original. Each step is idempotent, so a run stopped half way is finished by the next.
func finish(ctx context.Context, c client.Client, old, cp *unstructured.Unstructured, done Outcome) (Outcome, error) {
	if st, ok := old.Object["status"]; ok && !reflect.DeepEqual(cp.Object["status"], st) {
		cp.Object["status"] = runtime.DeepCopyJSONValue(st)
		if err := c.Status().Update(ctx, cp); err != nil {
			return Failed, fmt.Errorf("write the copy's status: %w", err)
		}
	}
	fresh := &unstructured.Unstructured{}
	fresh.SetGroupVersionKind(cp.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKeyFromObject(cp), fresh); err != nil {
		return Failed, fmt.Errorf("read the copy back: %w", err)
	}
	if err := same(old, fresh); err != nil {
		return Failed, fmt.Errorf("the copy differs, so the original is kept: %w", err)
	}
	if _, guarded := fresh.GetAnnotations()[v1alpha1.AnnotationMigrating]; guarded {
		patch := []byte(`{"metadata":{"annotations":{"` + v1alpha1.AnnotationMigrating + `":null}}}`)
		if err := c.Patch(ctx, fresh, client.RawPatch(types.MergePatchType, patch)); err != nil {
			return Failed, fmt.Errorf("lift the copy's guard: %w", err)
		}
	}
	if len(old.GetFinalizers()) > 0 {
		old.SetFinalizers(nil)
		if err := c.Update(ctx, old); err != nil {
			return Failed, fmt.Errorf("release the original's finalizers: %w", err)
		}
	}
	uid, rv := old.GetUID(), old.GetResourceVersion()
	if err := c.Delete(ctx, old, client.Preconditions{UID: &uid, ResourceVersion: &rv}); client.IgnoreNotFound(err) != nil {
		return Failed, fmt.Errorf("delete the original: %w", err)
	}
	return done, nil
}

// same is nil when the copy carries everything the original does: spec and status as subsets (a
// newer schema may default a field the original lacks), and every label and annotation except the
// migration's own. It names fields, never values: a spec holds task text.
func same(old, cp *unstructured.Unstructured) error {
	var errs []error
	for _, f := range []string{"spec", "status"} {
		if !subset(old.Object[f], cp.Object[f]) {
			errs = append(errs, fmt.Errorf("%s", f))
		}
	}
	for k, v := range old.GetLabels() {
		if cp.GetLabels()[k] != v {
			errs = append(errs, fmt.Errorf("label %s", k))
		}
	}
	for k, v := range old.GetAnnotations() {
		if k == v1alpha1.AnnotationMigratedFrom || k == v1alpha1.AnnotationMigrating {
			continue
		}
		if cp.GetAnnotations()[k] != v {
			errs = append(errs, fmt.Errorf("annotation %s", k))
		}
	}
	return errors.Join(errs...)
}

// subset: every field of a is in b with the same value; lists and scalars compare whole.
func subset(a, b any) bool {
	am, ok := a.(map[string]any)
	if !ok {
		return reflect.DeepEqual(a, b)
	}
	bm, ok := b.(map[string]any)
	if !ok {
		return len(am) == 0 && b == nil
	}
	for k, v := range am {
		if !subset(v, bm[k]) {
			return false
		}
	}
	return true
}
