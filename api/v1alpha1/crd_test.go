// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// The generated CRD is what cloud-native-ref vendors and validates against
// (skipMissingSchemas: false). These are the rules the design relies on, each
// asserted on the field that must carry it.
func TestCRDCarriesTheDesignRules(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/agents.ogenki.io_rooms.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(raw, &crd); err != nil {
		t.Fatal(err)
	}
	if crd.Spec.Group != "agents.ogenki.io" || crd.Spec.Scope != apiextensionsv1.NamespaceScoped {
		t.Fatalf("group %q scope %q", crd.Spec.Group, crd.Spec.Scope)
	}
	if len(crd.Spec.Versions) != 1 || crd.Spec.Versions[0].Subresources == nil || crd.Spec.Versions[0].Subresources.Status == nil {
		t.Fatal("want one version with the status subresource")
	}
	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	if !slices.ContainsFunc(root.XValidations, func(r apiextensionsv1.ValidationRule) bool {
		return r.Rule == `self.metadata.name.matches('^[a-z2-7]{8}$')`
	}) {
		t.Error("the root lacks the C2 name rule")
	}

	for _, tc := range []struct {
		path     string
		check    func(apiextensionsv1.JSONSchemaProps) bool
		wantRule string
	}{
		{"spec.dataClass", enum("public", "internal"), "enum public, internal"},
		{"spec.members", func(p apiextensionsv1.JSONSchemaProps) bool { return p.MaxItems != nil && *p.MaxItems == 20 }, "maxItems 20"},
		{"spec.members.items.role", enum("watcher", "collaborator", "owner"), "enum watcher, collaborator, owner"},
		{"spec.approvals", hasDefault(`{}`), "default {}"},
		{"spec.approvals.profile", enum("attended", "unattended"), "enum attended, unattended"},
		{"spec.retention", hasDefault(`"90d"`), "default 90d"},
		{"spec.retention", pattern(`^[1-9][0-9]{0,3}d$`), "a bounded retention"},
		{"spec.retention", rule("self == oldSelf"), "immutability: the log's retention is fixed at creation"},
		{"spec.dataClass", rule("self == oldSelf"), "immutability: a room's class is decided at creation (ruling TD)"},
		{"spec.approvals.ttl", hasDefault(`"4h"`), "default 4h"},
		{"spec.approvals.ttl", pattern(`^[1-9][0-9]{0,3}(m|h)$`), "a bounded ttl"},
		{"spec.owner", maxLength(261), "maxLength 261"},
		{"spec.driver", maxLength(261), "maxLength 261"},
		{"spec.members.items.principal", maxLength(261), "maxLength 261"},
	} {
		t.Run(tc.path+"/"+tc.wantRule, func(t *testing.T) {
			p, ok := field(*root, tc.path)
			if !ok {
				t.Fatalf("no field %s", tc.path)
			}
			if !tc.check(p) {
				t.Errorf("%s lacks %s", tc.path, tc.wantRule)
			}
		})
	}
}

// field walks a dotted path; "items" steps into an array's item schema.
func field(p apiextensionsv1.JSONSchemaProps, path string) (apiextensionsv1.JSONSchemaProps, bool) {
	for _, name := range strings.Split(path, ".") {
		if name == "items" {
			if p.Items == nil || p.Items.Schema == nil {
				return p, false
			}
			p = *p.Items.Schema
			continue
		}
		next, ok := p.Properties[name]
		if !ok {
			return p, false
		}
		p = next
	}
	return p, true
}

func enum(want ...string) func(apiextensionsv1.JSONSchemaProps) bool {
	return func(p apiextensionsv1.JSONSchemaProps) bool {
		var got []string
		for _, v := range p.Enum {
			var s string
			if json.Unmarshal(v.Raw, &s) != nil {
				return false
			}
			got = append(got, s)
		}
		return slices.Equal(got, want)
	}
}

func hasDefault(raw string) func(apiextensionsv1.JSONSchemaProps) bool {
	return func(p apiextensionsv1.JSONSchemaProps) bool { return p.Default != nil && string(p.Default.Raw) == raw }
}

func pattern(want string) func(apiextensionsv1.JSONSchemaProps) bool {
	return func(p apiextensionsv1.JSONSchemaProps) bool { return p.Pattern == want }
}

func rule(want string) func(apiextensionsv1.JSONSchemaProps) bool {
	return func(p apiextensionsv1.JSONSchemaProps) bool {
		return slices.ContainsFunc(p.XValidations, func(r apiextensionsv1.ValidationRule) bool { return r.Rule == want })
	}
}

func maxLength(want int64) func(apiextensionsv1.JSONSchemaProps) bool {
	return func(p apiextensionsv1.JSONSchemaProps) bool { return p.MaxLength != nil && *p.MaxLength == want }
}
