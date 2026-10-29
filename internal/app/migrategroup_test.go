// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func oldRoom(name, uid string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"owner": "human:local"}}}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "agents.ogenki.io", Version: "v1alpha1", Kind: "Room"})
	u.SetNamespace("maku")
	u.SetName(name)
	u.SetUID(types.UID(uid))
	return u
}

// migrateScheme knows Room and Task under both groups, as unstructured: the fake client needs the
// kinds of the objects it starts with.
func migrateScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, g := range []string{"agents.ogenki.io", "maku.ogenki.io"} {
		for _, k := range []string{"Room", "Task"} {
			s.AddKnownTypeWithName(schema.GroupVersionKind{Group: g, Version: "v1alpha1", Kind: k}, &unstructured.Unstructured{})
			s.AddKnownTypeWithName(schema.GroupVersionKind{Group: g, Version: "v1alpha1", Kind: k + "List"}, &unstructured.UnstructuredList{})
		}
	}
	return s
}

func TestRunMigrateGroup(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		objs    []client.Object
		wantErr string
		wantLog []string
	}{
		{"no --from", []string{"--to", "maku.ogenki.io"}, nil, `--from "" is not an API group`, nil},
		{"the same group twice", []string{"--from", "maku.ogenki.io", "--to", "maku.ogenki.io"}, nil, "the same group", nil},
		{"an unknown flag", []string{"--from", "a.b", "--to", "c.d", "--force"}, nil, "flag provided but not defined", nil},
		{"a stray argument", []string{"--from", "a.b", "--to", "c.d", "now"}, nil, `unexpected argument "now"`, nil},
		{"a dry run logs each object and the totals", []string{"--from", "agents.ogenki.io", "--to", "maku.ogenki.io"},
			[]client.Object{oldRoom("aaaaaaaa", "u1")}, "",
			[]string{`"outcome":"would-copy"`, `"name":"aaaaaaaa"`, `"msg":"migrate-group done"`, `"wouldCopy":1`}},
		{"a conflict fails the command", []string{"--from", "agents.ogenki.io", "--to", "maku.ogenki.io", "--apply"},
			[]client.Object{oldRoom("aaaaaaaa", "u1"), func() client.Object {
				u := oldRoom("aaaaaaaa", "other")
				u.SetGroupVersionKind(schema.GroupVersionKind{Group: "maku.ogenki.io", Version: "v1alpha1", Kind: "Room"})
				return u
			}()}, "1 conflicts, 0 failed",
			[]string{`"outcome":"conflict"`, `"conflicts":1`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&buf, nil))
			newClient := func() (client.Client, error) {
				return fake.NewClientBuilder().WithScheme(migrateScheme()).WithObjects(tc.objs...).Build(), nil
			}
			err := runMigrateGroup(t.Context(), log, tc.args, newClient)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("runMigrateGroup = %v, want %q", err, tc.wantErr)
			}
			for _, w := range tc.wantLog {
				if !strings.Contains(buf.String(), w) {
					t.Errorf("log lacks %s:\n%s", w, buf.String())
				}
			}
		})
	}
}
