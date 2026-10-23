// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/Smana/agent-platform/internal/factory/config"
)

const reviewNote = "# Steering: review\n\n## Verify\n- `task check` (CI: check): every gate passes.\n"

// scopeRig is the rig with loops.review.scope set to mode (review is the rig's default class).
func scopeRig(t *testing.T, mode config.ScopeMode) *rig {
	t.Helper()
	g := newRig(t, issueTask("3buqdlot", 7, "# Fix the link"))
	g.r.Cfg.Loops = map[string]config.Loop{"review": {Scope: mode}}
	return g
}

// Scoper design SC2, D4: no row is off, and off reads no note.
func TestNoScopeRowIsOff(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "# Fix the link"))
	g.f.SetFile(".github/agent-factory/steering/review.md", reviewNote)
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Spec.ScopeMode != "off" || tk.Spec.Steering != nil ||
		slices.ContainsFunc(g.metrics.recorded, func(m string) bool { return strings.HasPrefix(m, "steering") }) {
		t.Fatalf("mode %q steering %+v metrics %v", tk.Spec.ScopeMode, tk.Spec.Steering, g.metrics.recorded)
	}
}

// SC3, offline proof 2: a valid note lands on spec.steering with its blob sha; an absent note gives
// none; an invalid one is not used and counted; a read error retries the triage.
func TestTriageReadsTheSteeringNote(t *testing.T) {
	for name, c := range map[string]struct {
		note, read string
		used       bool
	}{
		"valid":     {reviewNote, "used", true},
		"absent":    {"", "absent", false},
		"no Verify": {"## Leave alone\n- `docs/`\n", "invalid", false},
	} {
		t.Run(name, func(t *testing.T) {
			g := scopeRig(t, config.ScopeSteering)
			if c.note != "" {
				g.f.SetFile(".github/agent-factory/steering/review.md", c.note)
			}
			tk := g.reconcile(t, "3buqdlot", 1)
			if tk.Spec.ScopeMode != "steering" || (tk.Spec.Steering != nil) != c.used {
				t.Fatalf("mode %q steering %+v", tk.Spec.ScopeMode, tk.Spec.Steering)
			}
			if c.used && (tk.Spec.Steering.Text != c.note || len(tk.Spec.Steering.SHA) != 40 ||
				tk.Spec.Steering.Path != ".github/agent-factory/steering/review.md") {
				t.Fatalf("steering %+v", tk.Spec.Steering)
			}
			if !slices.Contains(g.metrics.recorded, "steering review "+c.read) {
				t.Fatalf("metrics %v", g.metrics.recorded)
			}
		})
	}
	g := scopeRig(t, config.ScopeSteering)
	g.f.SetFile(".github/agent-factory/steering/review.md", "!")
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "agent-system", Name: "3buqdlot"}}
	if _, err := g.r.Reconcile(t.Context(), req); err == nil {
		t.Fatal("a failed read triaged the task")
	}
	g.f.SetFile(".github/agent-factory/steering/review.md", reviewNote)
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Spec.Steering == nil || tk.Status.Phase != "Triaged" {
		t.Fatalf("the retry: %s %+v", tk.Status.Phase, tk.Spec.Steering)
	}
}

// A config change after triage never moves the task; an investigate task is never scoped.
func TestTheModeIsFixedAtTriage(t *testing.T) {
	g := scopeRig(t, config.ScopeSteering)
	g.reconcile(t, "3buqdlot", 1)
	g.r.Cfg.Loops = nil
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Spec.ScopeMode != "steering" {
		t.Fatalf("mode %q after the config changed", tk.Spec.ScopeMode)
	}
	inv := investigateRig(t)
	inv.r.Cfg.Loops = map[string]config.Loop{"default": {Scope: config.ScopeSteering}}
	if tk := inv.reconcile(t, "3buqdlot", 1); tk.Spec.ScopeMode != "off" || tk.Spec.Steering != nil {
		t.Fatalf("investigate: mode %q", tk.Spec.ScopeMode)
	}
}

// The fence (v2 D7) comes first: a repository this factory does not own (or whose fence cache went
// stale) is not read for a note, so nothing it keeps reaches a brief. It is warned and counted
// "unowned", and the task proceeds unsteered without a retry.
func TestAnUnownedRepositoryIsNotRead(t *testing.T) {
	g := scopeRig(t, config.ScopeSteering)
	var logs bytes.Buffer
	g.r.Log = slog.New(slog.NewTextHandler(&logs, nil))
	g.r.Owned = func(string) bool { return false }
	g.f.SetFile(".github/agent-factory/steering/review.md", reviewNote)
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Spec.Steering != nil || tk.Status.Phase != "Triaged" || !slices.Contains(g.metrics.recorded, "steering review unowned") {
		t.Fatalf("steering %+v phase %s metrics %v", tk.Spec.Steering, tk.Status.Phase, g.metrics.recorded)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "review") || !strings.Contains(out, tk.Spec.Repository) {
		t.Fatalf("log %q", out)
	}
}
