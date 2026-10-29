// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
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

// kubeconfig writes a kubeconfig with one context per name, every one on https://api.test:6443,
// and returns its path.
func kubeconfig(t *testing.T, contexts ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    server: https://api.test:6443/some/path\n")
	b.WriteString("users:\n- name: u\n  user:\n    token: not-a-secret\ncontexts:\n")
	for _, n := range contexts {
		fmt.Fprintf(&b, "- name: %s\n  context:\n    cluster: c\n    user: u\n", n)
	}
	if len(contexts) > 0 {
		fmt.Fprintf(&b, "current-context: %s\n", contexts[0])
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunMigrateGroup(t *testing.T) {
	fwd := []string{"--from", "agents.ogenki.io", "--to", "maku.ogenki.io"}
	one := kubeconfig(t, "staging-admin")
	for _, tc := range []struct {
		name       string
		args       []string
		kubeconfig string
		objs       []client.Object
		wantErr    string
		wantLog    []string
	}{
		{"no --from", []string{"--to", "maku.ogenki.io"}, one, nil, `--from "" is not an API group`, nil},
		{"the same group twice", []string{"--from", "maku.ogenki.io", "--to", "maku.ogenki.io"}, one, nil, "the same group", nil},
		{"an unknown flag", []string{"--from", "a.b", "--to", "c.d", "--force"}, one, nil, "flag provided but not defined", nil},
		{"a stray argument", []string{"--from", "a.b", "--to", "c.d", "now"}, one, nil, `unexpected argument "now"`, nil},
		{"an unset KUBECONFIG is refused, never answered from ~/.kube/config", fwd, "", nil, "KUBECONFIG is not set", nil},
		{"a KUBECONFIG list is refused", fwd, one + string(filepath.ListSeparator) + one, nil, "more than one file", nil},
		{"a kubeconfig with two contexts is refused", fwd, kubeconfig(t, "staging", "prod"), nil, "has 2 contexts", nil},
		{"a kubeconfig with no context is refused", fwd, kubeconfig(t), nil, "has 0 contexts", nil},
		{"a dry run names its cluster first, then logs each object and the totals", fwd, one,
			[]client.Object{oldRoom("aaaaaaaa", "u1")}, "",
			[]string{`"outcome":"would-copy"`, `"name":"aaaaaaaa"`, `"msg":"migrate-group done"`, `"wouldCopy":1`}},
		{"a conflict fails the command", append(fwd, "--apply"), one,
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
			var built *rest.Config
			newClient := func(cfg *rest.Config) (client.Client, error) {
				built = cfg
				return fake.NewClientBuilder().WithScheme(migrateScheme()).WithObjects(tc.objs...).Build(), nil
			}
			getenv := func(k string) string {
				if k == "KUBECONFIG" {
					return tc.kubeconfig
				}
				return ""
			}
			err := runMigrateGroup(t.Context(), log, tc.args, getenv, newClient)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("runMigrateGroup = %v, want %q", err, tc.wantErr)
			}
			if tc.wantLog == nil {
				if built != nil || buf.Len() != 0 {
					t.Errorf("a refused run built a client or logged:\n%s", buf.String())
				}
				return
			}
			if built == nil || built.Host != "https://api.test:6443/some/path" {
				t.Errorf("the client was built for %+v, want the kubeconfig's one context", built)
			}
			first, _, _ := strings.Cut(buf.String(), "\n")
			for _, w := range []string{`"msg":"migrate-group target"`, `"context":"staging-admin"`, `"host":"api.test:6443"`} {
				if !strings.Contains(first, w) {
					t.Errorf("the first line lacks %s: %s", w, first)
				}
			}
			for _, w := range tc.wantLog {
				if !strings.Contains(buf.String(), w) {
					t.Errorf("log lacks %s:\n%s", w, buf.String())
				}
			}
		})
	}
}
