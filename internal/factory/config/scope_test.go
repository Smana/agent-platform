// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// Scoper design SC2, offline proof 1: the switch parses, defaults to off, a row is whole, and
// scope never switches the loop on (loop part 2 R2). The test writes the bare form {scope: off},
// which YAML 1.1 reads as false: it must still mean off.
func TestScopeFor(t *testing.T) {
	cfg, err := Parse([]byte(good + `loops:
  default: {scope: steering}
  docs: {scope: scoper}
  tests: {stallSteps: 40}
  dashboards: {scope: off}
`))
	if err != nil {
		t.Fatal(err)
	}
	for class, want := range map[string]ScopeMode{"docs": ScopeScoper, "tests": ScopeOff, "dashboards": ScopeOff,
		"review": ScopeSteering, "docs-links": ScopeSteering} {
		if got := cfg.ScopeFor(class); got != want {
			t.Errorf("ScopeFor(%s) = %s, want %s", class, got, want)
		}
	}
	if cfg.LoopFor("docs") != nil || cfg.LoopFor("review") != nil {
		t.Error("a scope row switched the loop on")
	}
	none, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if got := none.ScopeFor("docs"); got != ScopeOff {
		t.Errorf("no loops: %s, want off (D4)", got)
	}
}

// A class row is read whole: a row with only scope has no loop, even when default has one (the
// scope row does not inherit default's loop, nor does default's scope reach a row that sets none).
func TestAScopeOnlyRowDropsDefaultsLoop(t *testing.T) {
	cfg, err := Parse([]byte(good + `loops:
  default: {stallSteps: 40}
  docs: {scope: steering}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LoopFor("docs") != nil {
		t.Error("docs inherited default's loop")
	}
	if cfg.LoopFor("tests") == nil {
		t.Error("a class without a row gets default's loop")
	}
	if cfg.ScopeFor("docs") != ScopeSteering || cfg.ScopeFor("tests") != ScopeOff || !cfg.SteeringOn() {
		t.Error("scope follows the row")
	}
}

// YAML 1.1 reads bare false and no as the boolean false: both are off. yes is true, which is refused.
func TestYAML11BooleansInScope(t *testing.T) {
	for _, v := range []string{"false", "no", "off"} {
		cfg, err := Parse([]byte(good + "loops: {docs: {scope: " + v + "}}\n"))
		if err != nil {
			t.Fatalf("scope: %s: %v", v, err)
		}
		if got := cfg.ScopeFor("docs"); got != ScopeOff {
			t.Errorf("scope: %s = %q, want off", v, got)
		}
	}
	_, err := Parse([]byte(good + "loops: {docs: {scope: yes}}\n"))
	if err == nil || !strings.Contains(err.Error(), "scope is off, steering or scoper") {
		t.Fatalf("scope: yes: err = %v", err)
	}
}

func TestScopesAreRefused(t *testing.T) {
	for name, c := range map[string]struct{ loops, want string }{
		"a fourth mode":       {"{docs: {scope: always}}", `loops.docs.scope "always" is off, steering or scoper`},
		"a bare on":           {"{docs: {scope: on}}", "scope is off, steering or scoper"},
		"an undeclared class": {"{scripts: {scope: steering}}", `loops.scripts: "scripts" is not default, review or a class`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(good + "loops: " + c.loops + "\n"))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

// The scoper's read-only guarantee is cloud-native-ref's octo-sts mapping of the triager role: the
// portable runtime gives every role the same token, so scope: scoper is refused there. steering is not.
func TestScoperIsRefusedOnThePortableRuntime(t *testing.T) {
	_, err := Parse([]byte(good + "loops: {docs: {scope: scoper}}\nruns: {group: maku.ogenki.io}\n"))
	if err == nil || !strings.Contains(err.Error(), "scope: scoper needs cloud-native-ref's AgentRun") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Parse([]byte(good + "loops: {docs: {scope: steering}}\nruns: {group: maku.ogenki.io}\n")); err != nil {
		t.Fatalf("steering is fine on the portable runtime: %v", err)
	}
}
