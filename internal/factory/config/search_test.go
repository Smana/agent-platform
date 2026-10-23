// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// v3 feature 5 (ST9, offline proof 5): loops.<class>.search parses, absent is off, a row is whole,
// and it never switches the loop on, as scope does not.
func TestSearchFor(t *testing.T) {
	cfg, err := Parse([]byte(good + `loops:
  default: {search: true}
  docs: {search: true}
  tests: {stallSteps: 40}
`))
	if err != nil {
		t.Fatal(err)
	}
	for class, want := range map[string]bool{"docs": true, "tests": false, "review": true, "dashboards": true} {
		if got := cfg.SearchFor(class); got != want {
			t.Errorf("SearchFor(%s) = %v, want %v", class, got, want)
		}
	}
	if cfg.LoopFor("docs") != nil || cfg.LoopFor("review") != nil {
		t.Error("a search row switched the loop on")
	}
	none, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if none.SearchFor("docs") {
		t.Error("no loops: search on, want off (D4)")
	}
}

func TestSearchIsRefused(t *testing.T) {
	for name, c := range map[string]struct{ loops, want string }{
		"a string":            {`{docs: {search: "true"}}`, "cannot unmarshal string"},
		"a number":            {"{docs: {search: 1}}", "cannot unmarshal number"},
		"an undeclared class": {"{scripts: {search: true}}", `loops.scripts: "scripts" is not default, review or a class`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(good + "loops: " + c.loops + "\n"))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}
