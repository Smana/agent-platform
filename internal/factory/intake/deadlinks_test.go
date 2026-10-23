// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

type linksMerger struct {
	runs      []forge.WorkflowRun
	artifacts map[int64][]LinkFinding
	files     map[string]string
}

func (m *linksMerger) WorkflowRuns(context.Context, string) ([]forge.WorkflowRun, error) {
	return m.runs, nil
}

func (m *linksMerger) RunArtifact(_ context.Context, id int64, _, _ string) ([]byte, error) {
	fs, ok := m.artifacts[id]
	if !ok {
		return nil, fmt.Errorf("run %d has no artifact", id)
	}
	return json.Marshal(fs)
}

func (m *linksMerger) FileAt(_ context.Context, p, _ string) (string, error) { return m.files[p], nil }

func linksClass() config.Class {
	return config.Class{AllowedFiles: []string{`^(docs|website/content)/.+\.md$`},
		ProtectedFiles: []string{`^docs/(superpowers|specs)/`, `^website/content/docs/decisions/`},
		Oracle:         &config.Oracle{Probe: "dead-links", Workflow: "dead-links.yaml", MaxFindings: 10}}
}

const goneURL = "https://example.com/gone"

// Offline proof 8: a finding is kept only when both runs report it, the class may change its
// file, and it is a link target there on main.
func TestTheDeadLinksProbe(t *testing.T) {
	both := []LinkFinding{
		{File: "docs/a.md", Line: 3, URL: goneURL, Status: "404", Archive: "https://web.archive.org/web/20261011195309/" + goneURL},
		{File: "docs/specs/x.md", Line: 1, URL: goneURL, Status: "404"},                 // protected
		{File: "docs/a.md", Line: 9, URL: "https://example.com/prose", Status: "410"},   // prose, not a link target
		{File: "docs/a.md", Line: 4, URL: "https://example.com/fenced", Status: "404"},  // inside a fence
		{File: "docs/a.md", Line: 5, URL: "https://example.com/limited", Status: "429"}, // never a finding
		{File: "docs/a.md", Line: 6, URL: "http://example.com/plain", Status: "404"},    // R52 cannot retarget http
		{File: "../etc/passwd", Line: 1, URL: goneURL, Status: "404"},                   // not a clean path
	}
	once := LinkFinding{File: "docs/a.md", Line: 7, URL: "https://example.com/once", Status: "404"}
	m := &linksMerger{
		runs: []forge.WorkflowRun{
			{ID: 30, Event: "pull_request", Branch: "agent/abcdefgh", Conclusion: "success"}, // a pull request's run
			{ID: 20, Event: "schedule", Branch: "main", Conclusion: "success"},
			{ID: 15, Event: "workflow_dispatch", Branch: "main", Actor: "stranger", Conclusion: "success"},
			{ID: 10, Event: "workflow_dispatch", Branch: "main", Actor: "Smana", Conclusion: "success"},
		},
		artifacts: map[int64][]LinkFinding{20: append(both, once), 10: both, 30: {once}, 15: {once}},
		files: map[string]string{"docs/a.md": "[a](" + goneURL + ") and [b](https://example.com/once).\n" +
			"See https://example.com/prose.\n```\n[c](https://example.com/fenced)\n```\n[d](https://example.com/limited) [e](http://example.com/plain)\n"},
	}
	rc := &config.Repository{Name: "Smana/cloud-native-ref", Maintainers: []string{"Smana"}}
	got, err := deadLinks(t.Context(), m, rc, linksClass())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].File != "docs/a.md" || got[0].URL != goneURL || got[0].Archive == "" {
		t.Fatalf("got %+v", got)
	}
	text, spec := deadLinksTask(got)
	golden := filepath.Join("testdata", "dead-links-task.golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		_ = os.MkdirAll("testdata", 0o755)
		_ = os.WriteFile(golden, []byte(text), 0o644)
	}
	if want, err := os.ReadFile(golden); err != nil || string(want) != text {
		t.Fatalf("task text differs from %s:\n%s", golden, text)
	}
	if len(spec) != 1 || spec[0].URL != goneURL {
		t.Fatalf("%+v", spec)
	}
	// A failed run among the newest two: nothing, not a half-agreed set.
	m.runs[1].Conclusion = "failure"
	if got, err := deadLinks(t.Context(), m, rc, linksClass()); err != nil || len(got) != 0 {
		t.Fatalf("a failed run finds nothing: %+v %v", got, err)
	}
}

func TestParseFindingsIsStrict(t *testing.T) {
	if _, err := ParseFindings([]byte(`[{"file":"a.md","url":"https://x","status":"404","line":1,"note":"x"}]`)); err == nil {
		t.Fatal("an unknown key refuses the file")
	}
	if _, err := ParseFindings([]byte(`[] []`)); err == nil {
		t.Fatal("trailing data refuses the file")
	}
}

// A run's findings that break a bound are refused whole: over the cap, a missing field, an
// archive that is no Wayback snapshot of the URL, a URL carrying whitespace.
func TestAnUntrustworthyRunFindsNothing(t *testing.T) {
	good := LinkFinding{File: "docs/a.md", Line: 3, URL: goneURL, Status: "404"}
	two := LinkFinding{File: "docs/a.md", Line: 4, URL: "https://example.com/two", Status: "404"}
	cl := linksClass()
	cl.Oracle.MaxFindings = 1
	for name, bad := range map[string][]LinkFinding{
		"over the cap":    {good, two},
		"no URL":          {good, {File: "docs/a.md", Line: 1, Status: "404"}},
		"no line":         {good, {File: "docs/a.md", URL: "https://example.com/x", Status: "404"}},
		"foreign archive": {{File: "docs/a.md", Line: 3, URL: "https://example.com/three", Status: "404", Archive: "https://evil.example/web/1/https://example.com/three"}},
		"archive of another page": {{File: "docs/a.md", Line: 3, URL: "https://example.com/three", Status: "404",
			Archive: "https://web.archive.org/web/1/https://example.com/other"}},
		"space in URL": {{File: "docs/a.md", Line: 3, URL: "https://example.com/a b", Status: "404"}},
	} {
		t.Run(name, func(t *testing.T) {
			c := cl
			if name != "over the cap" {
				c.Oracle = &config.Oracle{Probe: "dead-links", Workflow: "dead-links.yaml", MaxFindings: 10}
			}
			m := &linksMerger{
				runs:      []forge.WorkflowRun{{ID: 2, Event: "schedule", Branch: "main", Conclusion: "success"}, {ID: 1, Event: "schedule", Branch: "main", Conclusion: "success"}},
				artifacts: map[int64][]LinkFinding{2: append([]LinkFinding{good, two}, bad...), 1: append([]LinkFinding{good, two}, bad...)},
				files:     map[string]string{"docs/a.md": "[a](" + goneURL + ") [b](https://example.com/two) [c](https://example.com/three)"},
			}
			if name == "over the cap" {
				m.artifacts[2], m.artifacts[1] = bad, bad
			}
			rc := &config.Repository{Name: "Smana/cloud-native-ref", Maintainers: []string{"Smana"}}
			got, err := deadLinks(t.Context(), m, rc, c)
			if len(got) != 0 || !errors.Is(err, errFindingsRefused) {
				t.Fatalf("a refused run finds nothing, and says why: %+v %v", got, err)
			}
		})
	}
}

// The same finding repeated is one; a run of another workflow's actor or a pull request is ignored.
func TestDeadLinksDedupes(t *testing.T) {
	f := LinkFinding{File: "docs/a.md", Line: 3, URL: goneURL, Status: "404"}
	m := &linksMerger{
		runs:      []forge.WorkflowRun{{ID: 2, Event: "schedule", Branch: "main", Conclusion: "success"}, {ID: 1, Event: "schedule", Branch: "main", Conclusion: "success"}},
		artifacts: map[int64][]LinkFinding{2: {f, f, f}, 1: {f}},
		files:     map[string]string{"docs/a.md": "[a](" + goneURL + ")"},
	}
	cl := linksClass()
	cl.Oracle.MaxFindings = 1
	got, err := deadLinks(t.Context(), m, &config.Repository{Name: "r"}, cl)
	if err != nil || len(got) != 1 {
		t.Fatalf("three copies are one, within a cap of one: %+v %v", got, err)
	}
}
