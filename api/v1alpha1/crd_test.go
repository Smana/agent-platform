package v1alpha1

import (
	"os"
	"strings"
	"testing"
)

// The generated CRD is what cloud-native-ref vendors and validates against
// (skipMissingSchemas: false). These are the rules the design relies on.
func TestCRDCarriesTheDesignRules(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/agents.ogenki.io_rooms.yaml")
	if err != nil {
		t.Fatal(err)
	}
	crd := string(raw)
	for _, want := range []string{
		"group: agents.ogenki.io",
		"scope: Namespaced",
		`self.metadata.name.matches('^[a-z2-7]{8}$')`,
		"- public",
		"- internal",
		"- watcher",
		"- collaborator",
		"- owner",
		"- attended",
		"- unattended",
		"default: 90d",
		"default: 4h",
		"default: {}",
		"maxItems: 20",
		"subresources:",
	} {
		if !strings.Contains(crd, want) {
			t.Errorf("CRD lacks %q", want)
		}
	}
}
