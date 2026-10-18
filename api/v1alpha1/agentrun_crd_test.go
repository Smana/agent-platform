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
	raw, err := os.ReadFile("../../config/crd/agents.ogenki.io_agentruns.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(raw, &crd); err != nil {
		t.Fatal(err)
	}
	if crd.Spec.Group != "agents.ogenki.io" || crd.Spec.Names.Kind != "AgentRun" || crd.Spec.Scope != apiextensionsv1.NamespaceScoped {
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
	rules := []string{}
	for _, r := range spec.XValidations {
		rules = append(rules, r.Rule)
	}
	for _, want := range []string{
		"self.role != 'reviewer' || (has(self.task.url) && self.task.url.matches('^https://github\\\\.com/[^/]+/[^/]+/pull/[0-9]+$'))",
		"!has(self.task.url) || self.task.url.startsWith('https://github.com/' + self.repository + '/')",
		"has(self.roomRef) == has(oldSelf.roomRef) && (!has(self.roomRef) || self.roomRef == oldSelf.roomRef)",
	} {
		if !slices.Contains(rules, want) {
			t.Errorf("spec lacks rule %s", want)
		}
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
