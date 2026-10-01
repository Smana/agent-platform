// SPDX-License-Identifier: Apache-2.0

// Package runwatch mirrors the AgentRuns that name a room (SP2 §1, S4): liveness
// for every request, and the run's lifecycle into the log.
package runwatch

import (
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Smana/agent-platform/internal/envelope"
)

const (
	// Namespace is the only namespace AgentRun claims live in.
	Namespace = "agents"
	// RevokedAnnotation carries why a run was revoked; any value makes it not live.
	RevokedAnnotation = "agents.ogenki.io/revoked"

	defaultMaxMinutes  = 120
	deadlineToleration = 30 * time.Second

	// RoomBusy is the end reason of a run the room never admitted (F15), the
	// same string as the bridge API's refusal.
	RoomBusy = "room_busy"
	// BusyScope prefixes the idempotency scope of a run's refusal,
	// broker:busy:<runId>, which the bridge API appends at its first room_busy.
	BusyScope = "broker:busy:"
)

// Run is the part of an AgentRun claim (C3) that SP2 relies on.
type Run struct {
	ID, Room, Role, Principal, Phase, Revoked, DataClass, Branch, Repository, TaskURL string
	StartedAt, FinishedAt                                                             time.Time
	MaxMinutes                                                                        int64
	EgressProfiles                                                                    []string
	Deleted                                                                           bool // the claim was deleted before a terminal phase (review M15)
}

// Terminal reports whether an AgentRun phase is final.
func Terminal(phase string) bool {
	switch phase {
	case "Succeeded", "Failed", "BudgetExhausted", "Revoked":
		return true
	}
	return false
}

// Live is the broker's check on every bridge request (S4): not terminal, not revoked.
func (r Run) Live() bool { return !Terminal(r.Phase) && r.Revoked == "" }

// FromUnstructured reads exactly the C3 fields SP2 relies on. ok is false for a
// claim that is not a run: not named xplane-run-<C2 id>, or outside Namespace.
func FromUnstructured(u *unstructured.Unstructured) (Run, bool) {
	id, ok := strings.CutPrefix(u.GetName(), "xplane-run-")
	if !ok || !envelope.ValidID(id) || u.GetNamespace() != Namespace {
		return Run{}, false
	}
	str := func(path ...string) string { s, _, _ := unstructured.NestedString(u.Object, path...); return s }
	r := Run{ID: id, Room: str("spec", "roomRef"), Role: str("spec", "role"), Principal: str("spec", "principal"),
		Phase: str("status", "phase"), DataClass: str("spec", "dataClass"), Repository: str("spec", "repository"),
		Branch: str("status", "branch"), Revoked: u.GetAnnotations()[RevokedAnnotation], TaskURL: str("spec", "task", "url")}
	if r.Phase == "" {
		r.Phase = "Pending"
	}
	if r.Branch == "" {
		r.Branch = str("spec", "branch")
	}
	r.StartedAt, _ = time.Parse(time.RFC3339, str("status", "startedAt"))
	r.FinishedAt, _ = time.Parse(time.RFC3339, str("status", "finishedAt"))
	if r.FinishedAt.IsZero() {
		r.FinishedAt = readyTransition(u)
	}
	r.MaxMinutes, _, _ = unstructured.NestedInt64(u.Object, "spec", "budget", "maxMinutes")
	if r.MaxMinutes == 0 {
		r.MaxMinutes = defaultMaxMinutes
	}
	r.EgressProfiles, _, _ = unstructured.NestedStringSlice(u.Object, "spec", "egress", "profiles")
	return r, true
}

// readyTransition is the Ready condition's lastTransitionTime: a stand-in for a
// missing finishedAt that, unlike the broker's clock, does not move with when the
// broker happens to observe the claim (after a restart, say). Ready only: a
// provider hiccup that flips Synced hours later would turn a lost pod into a deadline.
func readyTransition(u *unstructured.Unstructured) time.Time {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != "Ready" {
			continue
		}
		s, _ := m["lastTransitionTime"].(string)
		t, _ := time.Parse(time.RFC3339, s)
		return t
	}
	return time.Time{}
}

// EndReason says why a run ended (ruling P15). The AgentRun only ever says
// Failed/PodFailed; the room knows whether the agent itself ended its conversation.
// refused is that the broker answered the run's bridge room_busy (F15): with no
// harness status mirrored, the run never held the room, so whatever its phase
// says, it ended room_busy. It is pure: a run with no end time (no finishedAt,
// no Ready transition) is never judged past its deadline.
func EndReason(r Run, harnessStatus string, refused bool) string {
	if r.Deleted {
		return "deleted"
	}
	switch r.Phase {
	case "BudgetExhausted":
		if r.Revoked == "" {
			return "budget-run"
		}
		return r.Revoked
	case "Revoked":
		return "revoked"
	}
	if refused && harnessStatus == "" {
		return RoomBusy
	}
	switch harnessStatus {
	case "finished":
		return "agent_finished"
	case "error":
		return "agent_error"
	case "stuck":
		return "agent_stuck"
	}
	if !r.StartedAt.IsZero() && !r.FinishedAt.IsZero() &&
		r.FinishedAt.Sub(r.StartedAt) >= time.Duration(r.MaxMinutes)*time.Minute-deadlineToleration {
		return "deadline"
	}
	if r.Phase == "Succeeded" {
		return "agent_finished"
	}
	return "pod_lost"
}
