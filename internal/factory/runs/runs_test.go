// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// R46, R47: the task's trace and the run's tier ride on the claim, and only when there is one.
func TestBuildCarriesTraceparentAndTier(t *testing.T) {
	s := spec()
	s.Traceparent, s.Tier = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "standard"
	u := Build(s)
	if u.GetAnnotations()[AnnTraceparent] != s.Traceparent || u.GetLabels()[LabelTier] != "standard" {
		t.Fatalf("%v %v", u.GetAnnotations(), u.GetLabels())
	}
	u = Build(spec())
	if _, ok := u.GetAnnotations()[AnnTraceparent]; ok {
		t.Error("no task trace, no annotation: the harness starts its own")
	}
	if _, ok := u.GetLabels()[LabelTier]; ok {
		t.Error("no tier, no label")
	}
}

// Ruling SF: the harness footer writes `Agent-Task: <URL>` from TASK_URL, which the composition
// takes from spec.task.url, else from this CREATE-time annotation. A text task gets it too.
func TestBuildCarriesTheTaskURLAnnotation(t *testing.T) {
	s := spec()
	s.Traceparent, s.SourceURL = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "https://github.com/Smana/cloud-native-ref/issues/2112"
	u := Build(s)
	if u.GetAnnotations()[AnnTaskURL] != s.SourceURL || u.GetAnnotations()[AnnTraceparent] != s.Traceparent {
		t.Fatalf("both annotations ride together: %v", u.GetAnnotations())
	}
	if text, _, _ := unstructured.NestedString(u.Object, "spec", "task", "text"); text != "brief" {
		t.Errorf("the annotation leaves task.text alone: %q", text)
	}
	if _, ok := Build(spec()).GetAnnotations()[AnnTaskURL]; ok {
		t.Error("no source URL, no annotation")
	}
}

// R48: the room's lastSeq at the run's creation and a verifier's head ride the claim, so a
// replay that finds it records what the run was given, never what it would give one now. A
// start seq of 0 — an empty room — is the absent annotation's own value.
func TestBuildCarriesTheStartSeqAndHead(t *testing.T) {
	s := spec()
	s.StartSeq, s.Head = 1202, "4be1c9d0a1b2c3d4e5f60718293a4b5c6d7e8f90"
	u := Build(s)
	if u.GetAnnotations()[AnnStartSeq] != "1202" || u.GetAnnotations()[AnnHead] != s.Head {
		t.Fatalf("%v", u.GetAnnotations())
	}
	r, ok := FromUnstructured(u)
	if !ok || r.StartSeq != 1202 || r.Head != s.Head {
		t.Fatalf("read back: %+v", r)
	}
	u = Build(spec())
	if _, ok := u.GetAnnotations()[AnnStartSeq]; ok {
		t.Error("an empty room's lastSeq is 0, the absent annotation's own value")
	}
	r, _ = FromUnstructured(u)
	if r.StartSeq != 0 || r.Head != "" {
		t.Fatalf("an absent annotation reads back as zero values: %+v", r)
	}
}

// Review I4: the task URL becomes the harness footer's `Agent-Task: <URL>`, so a newline in it
// would forge trailers, Agent-Run included. Create refuses every value it cannot vouch for, at
// CREATE, before the claim exists; so do the other CREATE-only values.
func TestCreateRefusesWhatItCannotVouchFor(t *testing.T) {
	const issue = "https://github.com/Smana/cloud-native-ref/issues/2112"
	for name, edit := range map[string]func(*Spec){
		"http":                func(s *Spec) { s.SourceURL = "http://github.com/Smana/cloud-native-ref/issues/2112" },
		"a foreign host":      func(s *Spec) { s.SourceURL = "https://github.com.evil.example/Smana/cloud-native-ref/issues/2112" },
		"a host lookalike":    func(s *Spec) { s.SourceURL = "https://gist.github.com/Smana/cloud-native-ref/issues/2112" },
		"a port":              func(s *Spec) { s.SourceURL = "https://github.com:443/Smana/cloud-native-ref/issues/2112" },
		"userinfo":            func(s *Spec) { s.SourceURL = "https://x@github.com/Smana/cloud-native-ref/issues/2112" },
		"a newline trailer":   func(s *Spec) { s.SourceURL = issue + "\nAgent-Run: aaaaaaaa" },
		"a CR":                func(s *Spec) { s.SourceURL = issue + "\r" },
		"a tab":               func(s *Spec) { s.SourceURL = issue + "\t" },
		"a C1 control":        func(s *Spec) { s.SourceURL = issue + "\u0085" },
		"an encoded newline":  func(s *Spec) { s.SourceURL = issue + "%0AAgent-Run:%20aaaaaaaa" },
		"a query":             func(s *Spec) { s.SourceURL = issue + "?x=1" },
		"a fragment":          func(s *Spec) { s.SourceURL = issue + "#issuecomment-1" },
		"a trailing slash":    func(s *Spec) { s.SourceURL = issue + "/" },
		"another repository":  func(s *Spec) { s.SourceURL = "https://github.com/Smana/agent-platform/issues/2112" },
		"not an issue or PR":  func(s *Spec) { s.SourceURL = "https://github.com/Smana/cloud-native-ref/commit/2112" },
		"no number":           func(s *Spec) { s.SourceURL = "https://github.com/Smana/cloud-native-ref/issues/" },
		"number zero":         func(s *Spec) { s.SourceURL = "https://github.com/Smana/cloud-native-ref/issues/0" },
		"a dot-dot path":      func(s *Spec) { s.SourceURL = "https://github.com/Smana/cloud-native-ref/../x/issues/1" },
		"a leading space":     func(s *Spec) { s.SourceURL = " " + issue },
		"task URL not https":  func(s *Spec) { s.TaskText, s.TaskURL = "", "http://github.com/Smana/cloud-native-ref/pull/12" },
		"task URL newline":    func(s *Spec) { s.TaskText, s.TaskURL = "", "https://github.com/Smana/cloud-native-ref/pull/12\nx" },
		"traceparent not W3C": func(s *Spec) { s.Traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01\nx" },
		"traceparent version": func(s *Spec) { s.Traceparent = "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" },
		"traceparent upper":   func(s *Spec) { s.Traceparent = "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01" },
		"traceparent flags":   func(s *Spec) { s.Traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-0g" },
		// W3C marks an all-zero trace id or parent id invalid: the harness would parent on nothing.
		"traceparent zero trace":  func(s *Spec) { s.Traceparent = "00-00000000000000000000000000000000-00f067aa0ba902b7-01" },
		"traceparent zero parent": func(s *Spec) { s.Traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01" },
		"traceparent short id":    func(s *Spec) { s.Traceparent = "00-4bf92f3577b34da6a3ce929d0e0e473-00f067aa0ba902b7-01" },
		"tier unknown":            func(s *Spec) { s.Tier = "medium" },
		"run id empty":            func(s *Spec) { s.RunID = "" },
		"run id not C2":           func(s *Spec) { s.RunID = "NOTANID1" },
	} {
		t.Run(name, func(t *testing.T) {
			c := newClient()
			s := spec()
			edit(&s)
			if err := c.Create(t.Context(), s); err == nil {
				t.Fatal("created")
			}
			if all, _ := c.List(t.Context()); len(all) != 0 {
				t.Fatalf("a claim exists: %v", all)
			}
		})
	}
}

func TestCreateAcceptsTheURLsGitHubWrites(t *testing.T) {
	for name, edit := range map[string]func(*Spec){
		"an issue":          func(s *Spec) { s.SourceURL = "https://github.com/Smana/cloud-native-ref/issues/2112" },
		"a pull request":    func(s *Spec) { s.SourceURL = "https://github.com/Smana/cloud-native-ref/pull/12" },
		"owner in any case": func(s *Spec) { s.SourceURL = "https://github.com/smana/Cloud-Native-Ref/issues/1" },
		"a task URL":        func(s *Spec) { s.TaskText, s.TaskURL = "", "https://github.com/Smana/cloud-native-ref/pull/12" },
		"a sampled trace":   func(s *Spec) { s.Traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" },
		"an unsampled one":  func(s *Spec) { s.Traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00" },
		// W3C Trace Context level 2 adds the random-trace-id flag (0x02).
		"a random trace id": func(s *Spec) { s.Traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-03" },
		"tier light":        func(s *Spec) { s.Tier = "light" },
		"tier standard":     func(s *Spec) { s.Tier = "standard" },
		"tier frontier":     func(s *Spec) { s.Tier = "frontier" },
	} {
		t.Run(name, func(t *testing.T) {
			s := spec()
			edit(&s)
			if err := newClient().Create(t.Context(), s); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The CREATE-only annotations never change afterwards, whatever the phase-5 patch limit admits.
func TestAnnotateRefusesTheCreateOnlyKeys(t *testing.T) {
	c := newClient()
	s := spec()
	s.SourceURL = "https://github.com/Smana/cloud-native-ref/issues/2112"
	if err := c.Create(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{AnnTaskURL, AnnTraceparent, AnnStartSeq, AnnHead} {
		if err := c.Annotate(t.Context(), s.RunID, map[string]string{key: "https://github.com/Smana/cloud-native-ref/issues/1"}); err == nil {
			t.Errorf("%s was patched", key)
		}
	}
	r := Build(s)
	if err := c.C.Get(t.Context(), client.ObjectKeyFromObject(r), r); err != nil || r.GetAnnotations()[AnnTaskURL] != s.SourceURL {
		t.Fatalf("%v %v", r.GetAnnotations(), err)
	}
}

// An id that is not a C2 id names no run: nothing is read, patched or deleted under it, even
// when a claim with that name exists.
func TestClientRefusesAnIDThatIsNotARun(t *testing.T) {
	c := newClient()
	for _, id := range []string{"notanid1", "7f3cq2x"} {
		u := Build(spec())
		u.SetName(Name(id))
		if err := c.C.Create(t.Context(), u); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"", "NOTANID1", "notanid1", "7f3cq2x", "7f3cq2xz/.."} {
		if _, found, err := c.Get(t.Context(), id); err == nil || found {
			t.Errorf("Get(%q) = %v %v", id, found, err)
		}
		if err := c.Annotate(t.Context(), id, map[string]string{AnnUsage: "1"}); err == nil {
			t.Errorf("Annotate(%q)", id)
		}
		if err := c.Delete(t.Context(), id); err == nil {
			t.Errorf("Delete(%q)", id)
		}
	}
}

func TestFromUnstructuredReadsFinishedAt(t *testing.T) {
	u := Build(spec())
	if err := unstructured.SetNestedField(u.Object, "2026-09-30T10:20:00Z", "status", "finishedAt"); err != nil {
		t.Fatal(err)
	}
	r, ok := FromUnstructured(u)
	if want := time.Date(2026, 9, 30, 10, 20, 0, 0, time.UTC); !ok || !r.Finished.Equal(want) {
		t.Fatalf("%v %v", r.Finished, ok)
	}
}

func newClient() Client {
	s := runtime.NewScheme()
	Scheme(s)
	return Client{C: fake.NewClientBuilder().WithScheme(s).Build()}
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
