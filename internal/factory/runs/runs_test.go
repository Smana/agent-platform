// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func spec() Spec {
	return Spec{RunID: "7f3cq2xz", TaskID: "3buqdlot", Role: "implementer", Repository: "Smana/cloud-native-ref",
		BaseRef: "main", Branch: "agent/3buqdlot", TaskText: "brief", Principal: PrincipalFactory, DataClass: "public",
		Model: "agent-default", RoomRef: "3buqdlot", MaxTokens: 1_500_000, MaxMinutes: 45}
}

func split(p string) []string { return strings.Split(p, ".") }

func TestBuildIsTheSP1Claim(t *testing.T) {
	u := Build(spec())
	if u.GetName() != "xplane-run-7f3cq2xz" || u.GetNamespace() != "agents" || u.GetLabels()[LabelTask] != "3buqdlot" ||
		u.GetLabels()[LabelPrincipal] != "system.factory" {
		t.Fatalf("identity %s/%s %v", u.GetNamespace(), u.GetName(), u.GetLabels())
	}
	for path, want := range map[string]any{
		"spec.role": "implementer", "spec.branch": "agent/3buqdlot", "spec.principal": "system:factory",
		"spec.dataClass": "public", "spec.roomRef": "3buqdlot", "spec.task.text": "brief",
		"spec.budget.maxTokens": int64(1_500_000), "spec.budget.maxMinutes": int64(45), "spec.size": "small",
	} {
		got, _, _ := unstructured.NestedFieldNoCopy(u.Object, split(path)...)
		if got != want {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "queueName"); found {
		t.Error("no queue until phase 4 sets one")
	}
	r := spec()
	r.Role, r.TaskText, r.TaskURL = "reviewer", "", "https://github.com/Smana/cloud-native-ref/pull/12"
	u = Build(r)
	if _, found, _ := unstructured.NestedString(u.Object, "spec", "task", "text"); found {
		t.Error("task.text and task.url are exclusive (XRD CEL)")
	}
}

func TestBuildWritesQueueAndEgressOnlyWhenSet(t *testing.T) {
	s := spec()
	s.Queue, s.EgressProfiles = QueueFactory, []string{"github", "docs"}
	u := Build(s)
	if q, _, _ := unstructured.NestedString(u.Object, "spec", "queueName"); q != "factory" {
		t.Errorf("queueName = %q", q)
	}
	if p, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "egress", "profiles"); strings.Join(p, ",") != "github,docs" {
		t.Errorf("egress.profiles = %v", p)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(Build(spec()).Object, "spec", "egress"); found {
		t.Error("no profiles, no egress block")
	}
}

func TestFromUnstructuredSkipsWhatIsNotARun(t *testing.T) {
	for name, edit := range map[string]func(*unstructured.Unstructured){
		"not xplane-run-":    func(u *unstructured.Unstructured) { u.SetName("other-7f3cq2xz") },
		"not a C2 id":        func(u *unstructured.Unstructured) { u.SetName("xplane-run-NOTANID") },
		"outside the agents": func(u *unstructured.Unstructured) { u.SetNamespace("default") },
	} {
		t.Run(name, func(t *testing.T) {
			u := Build(spec())
			edit(u)
			if _, ok := FromUnstructured(u); ok {
				t.Error("read as a run")
			}
		})
	}
}

func TestClientRoundTrip(t *testing.T) {
	s := runtime.NewScheme()
	Scheme(s)
	c := Client{C: fake.NewClientBuilder().WithScheme(s).Build()}
	ctx := t.Context()
	if err := c.Create(ctx, spec()); err != nil {
		t.Fatal(err)
	}
	if err := c.Annotate(ctx, "7f3cq2xz", map[string]string{AnnUsage: "1200", AnnRevoked: "budget-run"}); err != nil {
		t.Fatal(err)
	}
	r, found, err := c.Get(ctx, "7f3cq2xz")
	if err != nil || !found || r.Tokens != 1200 || r.Revoked != "budget-run" || r.TaskID != "3buqdlot" || r.MaxTokens != 1_500_000 {
		t.Fatalf("%+v %v %v", r, found, err)
	}
	all, err := c.List(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("%v %v", all, err)
	}
	if err := c.Delete(ctx, "7f3cq2xz"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := c.Get(ctx, "7f3cq2xz"); found {
		t.Fatal("deleted")
	}
	if err := c.Delete(ctx, "7f3cq2xz"); err != nil {
		t.Fatal("deleting a gone run is not an error")
	}
}
