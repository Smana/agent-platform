// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"os"
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// The native CRD keeps the XRD's contract (crossplane-configuration v0.9.15,
// apis/agentrun/definition.yaml): one run manifest suits both runtimes (3a R2).
func TestAgentRunCRDKeepsTheXRDContract(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/maku.ogenki.io_agentruns.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(raw, &crd); err != nil {
		t.Fatal(err)
	}
	if crd.Spec.Group != "maku.ogenki.io" || crd.Spec.Names.Kind != "AgentRun" || crd.Spec.Scope != apiextensionsv1.NamespaceScoped {
		t.Fatalf("group %q kind %q scope %q", crd.Spec.Group, crd.Spec.Names.Kind, crd.Spec.Scope)
	}
	v := crd.Spec.Versions[0]
	if v.Name != "v1alpha1" || v.Subresources == nil || v.Subresources.Status == nil {
		t.Fatal("want v1alpha1 with the status subresource")
	}
	spec := v.Schema.OpenAPIV3Schema.Properties["spec"]
	for _, f := range []string{"role", "repository", "principal", "dataClass", "task"} {
		if !slices.Contains(spec.Required, f) {
			t.Errorf("spec.%s must be required", f)
		}
	}
	// rule -> message, for every spec-level rule the XRD carries.
	got := map[string]string{}
	for _, r := range spec.XValidations {
		got[r.Rule] = r.Message
	}
	for rule, msg := range map[string]string{
		"self.role != 'reviewer' || (has(self.task.url) && self.task.url.matches('^https://github\\\\.com/[^/]+/[^/]+/pull/[0-9]+$'))": "a reviewer run needs a pull request URL as its task",
		"!has(self.task.url) || self.task.url.startsWith('https://github.com/' + self.repository + '/')":                               "task.url must point into spec.repository",
		"self.dataClass != 'internal' || !has(self.egress) || !has(self.egress.profiles) || size(self.egress.profiles) == 0":           "an internal run takes no egress profiles",
		"has(self.branch) == has(oldSelf.branch) && (!has(self.branch) || self.branch == oldSelf.branch)":                              "branch is immutable and cannot be added or removed after creation",
		"has(self.roomRef) == has(oldSelf.roomRef) && (!has(self.roomRef) || self.roomRef == oldSelf.roomRef)":                         "roomRef is immutable and cannot be added or removed after creation",
		"has(self.queueName) == has(oldSelf.queueName) && (!has(self.queueName) || self.queueName == oldSelf.queueName)":               "queueName is immutable and cannot be added or removed after creation",
		"has(self.suspendable) == has(oldSelf.suspendable) && (!has(self.suspendable) || self.suspendable == oldSelf.suspendable)":     "suspendable is immutable and cannot be added or removed after creation",
		"self.dataClass != 'internal' || !has(self.loop) || !has(self.loop.checks) || !self.loop.checks":                               "an internal run runs no repository checks",
		"self.dataClass == 'public' || !has(self.model) || !(self.model in ['kimi-frontier', 'mistral-standard', 'mistral-light'])":    "an internal run never names a provider alias",
		"has(self.loop) == has(oldSelf.loop) && (!has(self.loop) || (has(self.loop.checks) == has(oldSelf.loop.checks) && (!has(self.loop.checks) || self.loop.checks == oldSelf.loop.checks) && has(self.loop.search) == has(oldSelf.loop.search) && (!has(self.loop.search) || self.loop.search == oldSelf.loop.search)))": "loop is immutable and cannot be added or removed after creation",
	} {
		if m, ok := got[rule]; !ok || m != msg {
			t.Errorf("spec rule %q: message %q, want %q", rule, m, msg)
		}
	}
	if len(spec.XValidations) != 10 {
		t.Errorf("spec has %d rules, want 10", len(spec.XValidations))
	}
	immutable := func(name string, p apiextensionsv1.JSONSchemaProps) {
		for _, r := range p.XValidations {
			if r.Rule == "self == oldSelf" && r.Message == name+" is immutable" {
				return
			}
		}
		t.Errorf("%s lacks its immutability rule", name)
	}
	for _, f := range []string{"role", "repository", "baseRef", "principal", "model", "dataClass", "harness", "size", "egress", "task"} {
		immutable(f, spec.Properties[f])
	}
	immutable("budget.maxMinutes", spec.Properties["budget"].Properties["maxMinutes"])
	if m := spec.Properties["egress"].Properties["profiles"].MaxItems; m == nil || *m != 4 {
		t.Errorf("egress.profiles maxItems %v, want 4", m)
	}
	if p := spec.Properties["roomRef"].Pattern; p != "^[a-z2-7]{8}$" {
		t.Errorf("roomRef pattern %q", p)
	}
	if p := spec.Properties["task"].Properties["url"].Pattern; p != `^https://github\.com/[A-Za-z0-9-]+/[A-Za-z0-9._-]+/(issues|pull)/[0-9]+$` {
		t.Errorf("task.url pattern %q", p)
	}
	enum := func(p apiextensionsv1.JSONSchemaProps) []string {
		out := []string{}
		for _, e := range p.Enum {
			out = append(out, string(e.Raw))
		}
		return out
	}
	if got := enum(spec.Properties["role"]); !slices.Equal(got, []string{`"implementer"`, `"reviewer"`, `"tester"`, `"triager"`}) {
		t.Errorf("role enum %v", got)
	}
	if got := enum(*spec.Properties["egress"].Properties["profiles"].Items.Schema); !slices.Equal(got, []string{`"pypi"`, `"npm"`, `"golang"`, `"crates"`}) {
		t.Errorf("egress.profiles enum %v", got)
	}
	if got := enum(v.Schema.OpenAPIV3Schema.Properties["status"].Properties["phase"]); len(got) != 7 {
		t.Errorf("status.phase enum %v", got)
	}
}
