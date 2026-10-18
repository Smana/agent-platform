// SPDX-License-Identifier: Apache-2.0

package runctl

import (
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/runwatch"
)

// The composition's revoke values: a budget one ends the run BudgetExhausted, manual ends it
// Revoked; anything else is ignored (crossplane-configuration v0.9.15, _phaseOf).
var budgetReasons = []string{"budget-run", "budget-principal", "budget-fleet"}

// RevokeEnds reports whether the revoke annotation's value ends the run.
func RevokeEnds(revoked string) bool {
	return revoked == "manual" || slices.Contains(budgetReasons, revoked)
}

// Observe is the composition's _phaseOf and _reasonOf over a plain pod: a terminal status is
// final; then revocation; then the harness's exit; then the pod's failure; then a pod lost
// after the run started. finishedAt is always set at a terminal phase, so runwatch never falls
// back to the Ready condition.
func Observe(prev v1alpha1.AgentRunStatus, revoked string, pod *corev1.Pod, podGone bool, now time.Time) v1alpha1.AgentRunStatus {
	if runwatch.Terminal(prev.Phase) {
		return prev
	}
	st := v1alpha1.AgentRunStatus{Phase: "Pending", StartedAt: prev.StartedAt}
	if pod != nil && st.StartedAt == "" && pod.Status.Phase != corev1.PodPending {
		st.StartedAt = pod.CreationTimestamp.UTC().Format(time.RFC3339)
	}
	end := func(phase, reason string, at time.Time) v1alpha1.AgentRunStatus {
		st.Phase, st.Reason, st.FinishedAt = phase, reason, at.UTC().Format(time.RFC3339)
		return st
	}
	switch {
	case slices.Contains(budgetReasons, revoked):
		return end("BudgetExhausted", revoked, now)
	case revoked == "manual":
		return end("Revoked", revoked, now)
	}
	started := prev.Phase == "Running"
	if pod == nil {
		if started || podGone && prev.StartedAt != "" {
			return end("Failed", "PodLost", now)
		}
		return st
	}
	// 137 and 143 are the shutdown's own SIGKILL and SIGTERM, not a crash (the composition's
	// _crashed): they defer to the pod's disruption or deletion below.
	if t := harnessTerminated(pod); t != nil {
		switch t.ExitCode {
		case 0:
			return end("Succeeded", "", t.FinishedAt.Time)
		case 137, 143:
		default:
			return end("Failed", "PodFailed", t.FinishedAt.Time)
		}
	}
	if pod.Status.Phase == corev1.PodFailed {
		if podCondition(pod, corev1.DisruptionTarget) {
			return end("Failed", "Disrupted", now)
		}
		return end("Failed", "PodFailed", now)
	}
	if pod.DeletionTimestamp != nil && started {
		return end("Failed", "PodLost", now)
	}
	if started || podCondition(pod, corev1.PodReady) {
		st.Phase = "Running"
	}
	return st
}

func harnessTerminated(p *corev1.Pod) *corev1.ContainerStateTerminated {
	for _, s := range p.Status.ContainerStatuses {
		if s.Name == "harness" && s.State.Terminated != nil {
			return s.State.Terminated
		}
	}
	return nil
}

func podCondition(p *corev1.Pod, t corev1.PodConditionType) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == t {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
