// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

const (
	gone    = "https://example.com/gone"
	archive = "https://web.archive.org/web/20261011195309/https://example.com/gone"
	page    = "[a](https://example.com/gone) and [b](https://other.example/kept)."
)

// oracleInputs: a dead-links task of one finding, fixed on the head by a retarget to its archive.
func oracleInputs(headText string) ArmInputs {
	in := gateInputs()
	cl := declaredClass()
	cl.Oracle = &config.Oracle{Probe: "dead-links", Workflow: "dead-links.yaml", MaxFindings: 10}
	in.Cfg.Classes["dead-links"] = cl
	in.Task.Spec.Findings = []v1alpha1.Finding{{File: testFile, URL: gone, Archive: archive}}
	in.Files = changed(page, headText)
	in.Stats = []forge.FileStat{{Path: testFile, Status: "modified", Additions: 1, Deletions: 1}}
	return in
}

// Offline proof 9: the oracle condition on fixtures, each with its reason.
func TestTheOracle(t *testing.T) {
	for name, c := range map[string]struct {
		head, reason string
	}{
		"to the snapshot given":     {"[a](" + archive + ") and [b](https://other.example/kept).", ""},
		"within the host":           {"[a](https://example.com/moved) and [b](https://other.example/kept).", ""},
		"the finding left":          {page, "oracle_finding_open"},
		"another target moved":      {"[a](" + archive + ") and [b](https://other.example/new).", "oracle_unrequested"},
		"another snapshot":          {"[a](https://web.archive.org/web/2020/https://example.com/gone) and [b](https://other.example/kept).", "oracle_unrequested"},
		"the finding's file unread": {"", "oracle_finding_open"},
	} {
		t.Run(name, func(t *testing.T) {
			in := oracleInputs(c.head)
			if c.head == "" {
				in.Files = changed("x", "x")
				in.Files.Base, in.Files.Head = map[string]string{"docs/other.md": "x"}, map[string]string{"docs/other.md": "x"}
			}
			d := DecideArm(in)
			if (c.reason == "" && d.Verdict != "arm") || (c.reason != "" && d.Reason != c.reason) {
				t.Fatalf("got %+v, want %q", d, c.reason)
			}
		})
	}
	in := oracleInputs(page)
	in.Task.Spec.Findings = nil
	if d := DecideArm(in); d.Reason != "oracle_finding_open" {
		t.Fatalf("an oracle task without findings proves nothing: %+v", d)
	}
}
