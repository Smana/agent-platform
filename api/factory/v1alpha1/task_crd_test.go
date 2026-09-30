// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"os"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

// The generated CRD carries the rules §4 relies on. Tasks are runtime objects, so this CRD
// ships in the chart, not in cloud-native-ref's validation catalog.
func TestTaskCRDCarriesTheDesignRules(t *testing.T) {
	raw, err := os.ReadFile("../../../config/crd/agents.ogenki.io_tasks.yaml")
	if err != nil {
		t.Fatal(err)
	}
	crd := string(raw)
	for _, want := range []string{
		"kind: Task",
		"scope: Namespaced",
		`self.metadata.name.matches('^[a-z2-7]{8}$')`,
		"- issue",
		"- runlore",
		"- schedule",
		"- untrusted",
		"- AwaitingHuman",
		"- Verifying",
		"- Stopped",
		"maxLength: 65536",
		"maximum: 5000000",
		"subresources:",
	} {
		if !strings.Contains(crd, want) {
			t.Errorf("CRD lacks %q", want)
		}
	}
}

// Review I3: etcd stores the object and CEL costs every rule against the declared bounds, so
// every free-form string in spec and status has a maxLength and every list a maxItems.
func TestTaskCRDBoundsEveryStringAndList(t *testing.T) {
	raw, err := os.ReadFile("../../../config/crd/agents.ogenki.io_tasks.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(raw, &crd); err != nil {
		t.Fatal(err)
	}
	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	for _, top := range []string{"spec", "status"} {
		s := root.Properties[top]
		bounded(t, top, &s)
	}
	status := root.Properties["status"].Properties
	for list, want := range map[string]int64{"narrated": 512, "handled": 512, "outbox": 16} {
		if got := status[list].MaxItems; got == nil || *got != want {
			t.Errorf("status.%s maxItems = %v, want %d", list, got, want)
		}
	}
	// Ruling SK: roomSeq is the task_state clientSeq ledger, never trimmed, never lowered.
	seq := status["roomSeq"]
	if seq.Type != "integer" || seq.Minimum == nil || *seq.Minimum != 0 ||
		len(seq.XValidations) != 1 || seq.XValidations[0].Rule != "self >= oldSelf" {
		t.Errorf("status.roomSeq = %+v", seq)
	}
}

// bounded fails for a string without maxLength, other than an enum or a timestamp, and for a
// list without maxItems, anywhere under s.
func bounded(t *testing.T, path string, s *apiextensionsv1.JSONSchemaProps) {
	t.Helper()
	switch s.Type {
	case "string":
		if len(s.Enum) == 0 && s.Format != "date-time" && s.MaxLength == nil {
			t.Errorf("%s: a string without maxLength", path)
		}
	case "array":
		if s.MaxItems == nil {
			t.Errorf("%s: a list without maxItems", path)
		}
		bounded(t, path+"[]", s.Items.Schema)
	case "object":
		for name, p := range s.Properties {
			bounded(t, path+"."+name, &p)
		}
	}
}

func TestPhaseSets(t *testing.T) {
	for _, p := range []string{PhaseRejected, PhaseNoOp, PhaseDone, PhaseReverted, PhaseClosed, PhaseStopped} {
		if !TerminalPhase(p) || ActivePhase(p) {
			t.Errorf("%s is terminal and never active", p)
		}
	}
	for _, p := range []string{PhaseImplementing, PhaseReviewing, PhaseAwaitingCI, PhaseAutoMerging, PhaseVerifying} {
		if !ActivePhase(p) || TerminalPhase(p) {
			t.Errorf("%s is active", p)
		}
	}
	// Escalated and AwaitingHuman wait for a human: neither terminal nor active.
	for _, p := range []string{PhaseEscalated, PhaseAwaitingHuman, PhaseQueued} {
		if ActivePhase(p) || TerminalPhase(p) {
			t.Errorf("%s waits", p)
		}
	}
}

// Ruling SD: the kinds register through the builder, not an init().
func TestAddToSchemeRegistersTaskKinds(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"Task", "TaskList"} {
		if !s.Recognizes(GroupVersion.WithKind(kind)) {
			t.Errorf("%s is not registered", kind)
		}
	}
}

// R46: the task's root span ids are W3C-valid, bounded, and never change once minted.
func TestTaskCRDKeepsTheTraceIds(t *testing.T) {
	raw, err := os.ReadFile("../../../config/crd/agents.ogenki.io_tasks.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(raw, &crd); err != nil {
		t.Fatal(err)
	}
	tr := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"].Properties["trace"]
	rules := map[string]bool{}
	for _, v := range tr.XValidations {
		rules[v.Rule] = true
	}
	if !rules["self.traceID == oldSelf.traceID && self.spanID == oldSelf.spanID"] || !rules["!has(oldSelf.exported) || !oldSelf.exported || (has(self.exported) && self.exported)"] {
		t.Errorf("status.trace rules %v", tr.XValidations)
	}
	status := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
	if len(status.XValidations) != 1 || status.XValidations[0].Rule != "!has(oldSelf.trace) || has(self.trace)" {
		t.Errorf("status rules %v: a trace is never removed (review M3)", status.XValidations)
	}
	for field, want := range map[string]string{"traceID": `^[0-9a-f]{32}$`, "spanID": `^[0-9a-f]{16}$`} {
		p := tr.Properties[field]
		if p.Pattern != want || len(p.XValidations) != 1 || !strings.HasPrefix(p.XValidations[0].Rule, "self != '0000") {
			t.Errorf("status.trace.%s = %+v", field, p)
		}
	}
}
