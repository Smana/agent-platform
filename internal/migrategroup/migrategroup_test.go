// SPDX-License-Identifier: Apache-2.0

package migrategroup

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Smana/maku/api/v1alpha1"
)

const (
	oldG = "agents.ogenki.io"
	newG = "maku.ogenki.io"
	ns   = "agent-system"
)

func obj(group, kind, name, uid string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: Version, Kind: kind})
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetUID(types.UID(uid))
	return u
}

func room(group, name, uid string) *unstructured.Unstructured {
	u := obj(group, "Room", name, uid)
	u.Object["spec"] = map[string]any{"owner": "system:factory", "driver": "system:factory", "dataClass": "public", "retention": "90d"}
	u.Object["status"] = map[string]any{"phase": "Closed", "lastSeq": int64(42), "driver": "system:factory"}
	u.SetLabels(map[string]string{"agents.ogenki.io/issue": "7"})
	u.SetAnnotations(map[string]string{"agents.ogenki.io/forked-from": "aaaaaaaa@3"})
	u.SetFinalizers([]string{"agents.ogenki.io/room-log"})
	return u
}

func task(group, name, uid string) *unstructured.Unstructured {
	u := obj(group, "Task", name, uid)
	u.Object["spec"] = map[string]any{"repository": "Smana/x", "text": "t", "dataClass": "public",
		"source": map[string]any{"kind": "issue", "ref": "Smana/x#7", "key": "k"}}
	u.Object["status"] = map[string]any{"phase": "Done", "roomRef": name}
	u.SetLabels(map[string]string{"agents.ogenki.io/issue": "7"})
	return u
}

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, g := range []string{oldG, newG} {
		for _, k := range Kinds {
			s.AddKnownTypeWithName(schema.GroupVersionKind{Group: g, Version: Version, Kind: k}, &unstructured.Unstructured{})
			s.AddKnownTypeWithName(schema.GroupVersionKind{Group: g, Version: Version, Kind: k + "List"}, &unstructured.UnstructuredList{})
		}
	}
	return s
}

// fail lets a case refuse one call: op is create, status, list, update or delete; name is the
// object's name, or the kind for a list. The fake gives a created object a UID, as the server does.
type fail func(op, group, name string) error

func newClient(f fail, objs ...client.Object) client.Client {
	if f == nil {
		f = func(string, string, string) error { return nil }
	}
	var protos []client.Object
	for _, g := range []string{oldG, newG} {
		for _, k := range Kinds {
			protos = append(protos, obj(g, k, "", ""))
		}
	}
	group := func(o client.Object) string { return o.GetObjectKind().GroupVersionKind().Group }
	return fake.NewClientBuilder().WithScheme(scheme()).WithStatusSubresource(protos...).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
				if err := f("create", group(o), o.GetName()); err != nil {
					return err
				}
				o.SetUID(types.UID("uid-" + group(o) + "-" + o.GetName()))
				return c.Create(ctx, o, opts...)
			},
			List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
				gvk := l.GetObjectKind().GroupVersionKind()
				if err := f("list", gvk.Group, gvk.Kind); err != nil {
					return err
				}
				return c.List(ctx, l, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
				if err := f("update", group(o), o.GetName()); err != nil {
					return err
				}
				return c.Update(ctx, o, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
				if err := f("delete", group(o), o.GetName()); err != nil {
					return err
				}
				return c.Delete(ctx, o, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, o client.Object, opts ...client.SubResourceUpdateOption) error {
				if err := f("status", group(o), o.GetName()); err != nil {
					return err
				}
				return c.SubResource(sub).Update(ctx, o, opts...)
			},
		}).Build()
}

func get(t *testing.T, c client.Client, group, kind, name string) (*unstructured.Unstructured, bool) {
	t.Helper()
	u := obj(group, kind, name, "")
	err := c.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, u)
	if err != nil && client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
	return u, err == nil
}

func outcomes(r Report) []Outcome {
	out := make([]Outcome, 0, len(r))
	for _, x := range r {
		out = append(out, x.Outcome)
	}
	return out
}

func TestRun(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name  string
		objs  []client.Object
		fail  fail
		wrap  func(client.Client) client.Client
		apply bool
		want  []Outcome
		check func(t *testing.T, c client.Client, r Report)
	}{
		{
			name: "a dry run writes nothing and reports each object",
			objs: []client.Object{room(oldG, "aaaaaaaa", "u1"), task(oldG, "aaaaaaaa", "u2")},
			want: []Outcome{WouldCopy, WouldCopy},
			check: func(t *testing.T, c client.Client, _ Report) {
				if _, ok := get(t, c, newG, "Room", "aaaaaaaa"); ok {
					t.Error("a dry run created a copy")
				}
				if _, ok := get(t, c, oldG, "Room", "aaaaaaaa"); !ok {
					t.Error("a dry run deleted the original")
				}
			},
		},
		{
			name: "apply copies spec, labels, annotations and status, then deletes the original",
			objs: []client.Object{room(oldG, "aaaaaaaa", "u1"), task(oldG, "aaaaaaaa", "u2")}, apply: true,
			want: []Outcome{Migrated, Migrated},
			check: func(t *testing.T, c client.Client, _ Report) {
				for _, want := range []*unstructured.Unstructured{room(newG, "aaaaaaaa", ""), task(newG, "aaaaaaaa", "")} {
					kind := want.GetKind()
					got, ok := get(t, c, newG, kind, "aaaaaaaa")
					if !ok {
						t.Fatalf("no %s copy", kind)
					}
					if !reflect.DeepEqual(got.Object["spec"], want.Object["spec"]) || !reflect.DeepEqual(got.Object["status"], want.Object["status"]) {
						t.Errorf("%s copy spec %v status %v", kind, got.Object["spec"], got.Object["status"])
					}
					if got.GetLabels()["agents.ogenki.io/issue"] != "7" || len(got.GetFinalizers()) != 0 {
						t.Errorf("%s copy labels %v finalizers %v", kind, got.GetLabels(), got.GetFinalizers())
					}
					a := got.GetAnnotations()
					if _, guarded := a[v1alpha1.AnnotationMigrating]; guarded || a[v1alpha1.AnnotationMigratedFrom] == "" {
						t.Errorf("%s copy annotations %v: want migrated-from and no guard", kind, a)
					}
					if _, ok := get(t, c, oldG, kind, "aaaaaaaa"); ok {
						t.Errorf("the %s original is still there", kind)
					}
				}
				if r, _ := get(t, c, newG, "Room", "aaaaaaaa"); r.GetAnnotations()["agents.ogenki.io/forked-from"] != "aaaaaaaa@3" ||
					r.GetAnnotations()[v1alpha1.AnnotationMigratedFrom] != "u1" {
					t.Errorf("room annotations %v", r.GetAnnotations())
				}
			},
		},
		{
			name: "a copy an earlier run made is finished, not duplicated",
			objs: func() []client.Object {
				cp := room(newG, "aaaaaaaa", "c1")
				delete(cp.Object, "status")
				cp.SetFinalizers(nil)
				cp.SetAnnotations(map[string]string{"agents.ogenki.io/forked-from": "aaaaaaaa@3",
					v1alpha1.AnnotationMigratedFrom: "u1", v1alpha1.AnnotationMigrating: "true"})
				return []client.Object{room(oldG, "aaaaaaaa", "u1"), cp}
			}(),
			apply: true, want: []Outcome{Resumed},
			check: func(t *testing.T, c client.Client, _ Report) {
				got, _ := get(t, c, newG, "Room", "aaaaaaaa")
				if _, guarded := got.GetAnnotations()[v1alpha1.AnnotationMigrating]; guarded || got.Object["status"] == nil {
					t.Errorf("the resumed copy: annotations %v, status %v", got.GetAnnotations(), got.Object["status"])
				}
				if _, ok := get(t, c, oldG, "Room", "aaaaaaaa"); ok {
					t.Error("the original is still there")
				}
			},
		},
		{
			name:  "a same-named object no run copied is a conflict, and the original stays",
			objs:  []client.Object{room(oldG, "aaaaaaaa", "u1"), room(newG, "aaaaaaaa", "someone-else")},
			apply: true, want: []Outcome{Conflict},
			check: func(t *testing.T, c client.Client, r Report) {
				if r.OK() {
					t.Error("a conflict reported OK")
				}
				if _, ok := get(t, c, oldG, "Room", "aaaaaaaa"); !ok {
					t.Error("the original was deleted over a conflict")
				}
			},
		},
		{
			name: "a failed create keeps that original, and the next object still moves",
			objs: []client.Object{room(oldG, "aaaaaaaa", "u1"), room(oldG, "bbbbbbbb", "u2")},
			fail: func(op, _, name string) error {
				if op == "create" && name == "aaaaaaaa" {
					return boom
				}
				return nil
			},
			apply: true, want: []Outcome{Failed, Migrated},
			check: func(t *testing.T, c client.Client, _ Report) {
				kept(t, c, "Room", "aaaaaaaa", "the original of a failed copy was deleted")
			},
		},
		{
			name: "a failed status write keeps the original and leaves the copy guarded",
			objs: []client.Object{room(oldG, "aaaaaaaa", "u1")},
			fail: func(op, _, _ string) error {
				if op == "status" {
					return boom
				}
				return nil
			},
			apply: true, want: []Outcome{Failed},
			check: func(t *testing.T, c client.Client, _ Report) {
				cp, _ := get(t, c, newG, "Room", "aaaaaaaa")
				if _, guarded := cp.GetAnnotations()[v1alpha1.AnnotationMigrating]; !guarded {
					t.Error("a copy without its status lost its guard")
				}
				kept(t, c, "Room", "aaaaaaaa", "the original was deleted before its copy was confirmed")
			},
		},
		{
			name: "a copy the server changed is not confirmed, and the original stays",
			objs: []client.Object{room(oldG, "aaaaaaaa", "u1")},
			wrap: pruning, apply: true, want: []Outcome{Failed},
			check: func(t *testing.T, c client.Client, _ Report) {
				kept(t, c, "Room", "aaaaaaaa", "an unconfirmed copy cost the original")
			},
		},
		{
			name: "a copy whose status the server prunes is not confirmed, and the original stays",
			objs: []client.Object{room(oldG, "aaaaaaaa", "u1")},
			wrap: pruningStatus, apply: true, want: []Outcome{Failed},
			check: func(t *testing.T, c client.Client, _ Report) {
				kept(t, c, "Room", "aaaaaaaa", "a copy without its full status cost the original")
			},
		},
		{
			name: "an original rewritten after the list is not deleted",
			objs: []client.Object{task(oldG, "aaaaaaaa", "u2")},
			wrap: rewriting, apply: true, want: []Outcome{Failed},
			check: func(t *testing.T, c client.Client, _ Report) {
				if _, ok := get(t, c, oldG, "Task", "aaaaaaaa"); !ok {
					t.Error("the delete ignored the original's resourceVersion")
				}
			},
		},
		{
			name: "a resumed copy already live keeps its newer status",
			objs: func() []client.Object {
				cp := room(newG, "aaaaaaaa", "c1")
				cp.Object["status"] = map[string]any{"phase": "Closed", "lastSeq": int64(50), "driver": "system:factory"}
				cp.SetFinalizers([]string{newG + "/room-log"})
				cp.SetAnnotations(map[string]string{"agents.ogenki.io/forked-from": "aaaaaaaa@3", v1alpha1.AnnotationMigratedFrom: "u1"})
				return []client.Object{room(oldG, "aaaaaaaa", "u1"), cp}
			}(),
			apply: true, want: []Outcome{Resumed},
			check: func(t *testing.T, c client.Client, _ Report) {
				got, _ := get(t, c, newG, "Room", "aaaaaaaa")
				if seq := got.Object["status"].(map[string]any)["lastSeq"]; seq != int64(50) {
					t.Errorf("the live copy's lastSeq went from 50 to %v", seq)
				}
				if _, ok := get(t, c, oldG, "Room", "aaaaaaaa"); ok {
					t.Error("the original is still there")
				}
			},
		},
		{
			name: "a kind the source group does not serve is reported, not failed",
			objs: []client.Object{room(oldG, "aaaaaaaa", "u1")},
			fail: func(op, g, kind string) error {
				if op == "list" && kind == "TaskList" {
					return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: g, Kind: "Task"}}
				}
				return nil
			},
			apply: true, want: []Outcome{Migrated, NotServed},
			check: func(t *testing.T, _ client.Client, r Report) {
				if !r.OK() {
					t.Errorf("not-served failed the run: %+v", r)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(tc.fail, tc.objs...)
			if tc.wrap != nil {
				c = tc.wrap(c)
			}
			r, err := Run(t.Context(), c, Options{From: oldG, To: newG, Apply: tc.apply})
			if err != nil {
				t.Fatal(err)
			}
			if got := outcomes(r); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("outcomes %v, want %v (%+v)", got, tc.want, r)
			}
			tc.check(t, c, r)
		})
	}
}

// kept fails t unless the original is still there with its finalizers: a failure never strips them.
func kept(t *testing.T, c client.Client, kind, name, msg string) {
	t.Helper()
	orig, ok := get(t, c, oldG, kind, name)
	if !ok {
		t.Error(msg)
		return
	}
	if len(orig.GetFinalizers()) != 1 {
		t.Errorf("a failure released the original's finalizers: %v", orig.GetFinalizers())
	}
}

// pruningStatus wraps c so that a status write under the target group loses status.lastSeq.
func pruningStatus(c client.Client) client.Client {
	return interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, w client.Client, sub string, o client.Object, opts ...client.SubResourceUpdateOption) error {
			if u, ok := o.(*unstructured.Unstructured); ok && u.GroupVersionKind().Group == newG {
				unstructured.RemoveNestedField(u.Object, "status", "lastSeq")
			}
			return w.SubResource(sub).Update(ctx, o, opts...)
		},
	})
}

// rewriting wraps c so that someone else updates an original under the source group just before
// it is deleted: the delete's resourceVersion precondition must refuse it.
func rewriting(c client.Client) client.Client {
	return interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, w client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
			if o.GetObjectKind().GroupVersionKind().Group == oldG {
				cur := &unstructured.Unstructured{}
				cur.SetGroupVersionKind(o.GetObjectKind().GroupVersionKind())
				if err := w.Get(ctx, client.ObjectKeyFromObject(o), cur); err != nil {
					return err
				}
				cur.SetLabels(map[string]string{"rewritten": "yes"})
				if err := w.Update(ctx, cur); err != nil {
					return err
				}
			}
			return w.Delete(ctx, o, opts...)
		},
	})
}

// pruning wraps c so that a create under the target group loses spec.dataClass, as a stricter
// target schema would prune it.
func pruning(c client.Client) client.Client {
	return interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, w client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			if u, ok := o.(*unstructured.Unstructured); ok && u.GroupVersionKind().Group == newG {
				unstructured.RemoveNestedField(u.Object, "spec", "dataClass")
			}
			return w.Create(ctx, o, opts...)
		},
	})
}

// A full run, then a run in reverse, then a full run again: the spec's rollback path. A re-run after
// a full run finds nothing to do.
func TestRunIsIdempotentAndRunsInReverse(t *testing.T) {
	c := newClient(nil, room(oldG, "aaaaaaaa", "u1"), task(oldG, "aaaaaaaa", "u2"))
	ctx := t.Context()
	for i, step := range []struct {
		from, to string
		want     []Outcome
	}{
		{oldG, newG, []Outcome{Migrated, Migrated}},
		{oldG, newG, []Outcome{}},
		{newG, oldG, []Outcome{Migrated, Migrated}},
		{oldG, newG, []Outcome{Migrated, Migrated}},
	} {
		r, err := Run(ctx, c, Options{From: step.from, To: step.to, Apply: true})
		if err != nil {
			t.Fatal(err)
		}
		if got := outcomes(r); !reflect.DeepEqual(got, step.want) {
			t.Fatalf("step %d (%s to %s): outcomes %v, want %v", i, step.from, step.to, got, step.want)
		}
	}
	got, ok := get(t, c, newG, "Room", "aaaaaaaa")
	if !ok || got.Object["status"].(map[string]any)["lastSeq"] != int64(42) {
		t.Fatalf("after the round trip: present %v, status %v", ok, got.Object["status"])
	}
}

func TestOptionsValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    Options
		ok   bool
	}{
		{"the forward move", Options{From: oldG, To: newG}, true},
		{"the reverse move", Options{From: newG, To: oldG}, true},
		{"the same group twice", Options{From: newG, To: newG}, false},
		{"a missing --from", Options{To: newG}, false},
		{"a group without a dot", Options{From: "agents", To: newG}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.o.Validate(); (err == nil) != tc.ok {
				t.Fatalf("Validate() = %v, want ok %v", err, tc.ok)
			}
		})
	}
}
