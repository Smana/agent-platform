// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"strings"
	"testing"

	"github.com/Smana/agent-platform/internal/factory/config"
)

// scoperRig is a scoper-mode task with the review note, past its first run's start.
func scoperRig(t *testing.T) *rig {
	t.Helper()
	g := scopeRig(t, config.ScopeScoper)
	g.r.Cfg.Tiers["light"] = config.Tier{Model: "agent-default", RunTokens: 300_000, TaskTokens: 600_000, RunMinutes: 20}
	g.f.SetFile(".github/agent-factory/steering/review.md", reviewNote)
	g.reconcile(t, "3buqdlot", 3)
	return g
}

// Offline proof 4: mode scoper starts a triager run first, at the light tier's caps, with the
// scoping brief carrying the note and the snapshot.
func TestTheScoperRunsFirstAtTheLightCaps(t *testing.T) {
	g := scoperRig(t)
	s := g.runs.specs[rid(0)]
	if s.Role != "triager" || s.Tier != "light" || s.MaxTokens != 300_000 || s.MaxMinutes != 20 || s.Model != "agent-default" {
		t.Fatalf("the scoper's claim %+v", s)
	}
	for _, want := range []string{"You are the scoper", "STEERING-n0nce234", "TASK-DATA-n0nce234", "room_handoff", "one question"} {
		if !strings.Contains(s.TaskText, want) {
			t.Errorf("the scoping brief lacks %q", want)
		}
	}
	if s.Loop != nil {
		t.Fatal("the loop policy closes implementer runs only (part 2 R5)")
	}
}
