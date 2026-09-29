// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
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
