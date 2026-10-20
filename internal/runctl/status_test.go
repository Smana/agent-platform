// SPDX-License-Identifier: Apache-2.0

package runctl

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/Smana/agent-platform/api/v1alpha1"
)

var (
	t0  = time.Date(2026, 10, 18, 7, 0, 0, 0, time.UTC)
	now = t0.Add(10 * time.Minute)
)

func pod(phase corev1.PodPhase, mut ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(t0)}, Status: corev1.PodStatus{Phase: phase, StartTime: &metav1.Time{Time: t0}}}
	for _, m := range mut {
		m(p)
	}
	return p
}

func harnessExit(code int32) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "harness", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: code, FinishedAt: metav1.NewTime(t0.Add(5 * time.Minute))}}}}
	}
}

func ready(p *corev1.Pod) {
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
}

func TestObserve(t *testing.T) {
	running := v1alpha1.AgentRunStatus{Phase: "Running", StartedAt: t0.Format(time.RFC3339)}
	five := t0.Add(5 * time.Minute).Format(time.RFC3339)
	for name, tc := range map[string]struct {
		prev    v1alpha1.AgentRunStatus
		revoked string
		pod     *corev1.Pod
		gone    bool
		phase   string
		reason  string
		fin     string
	}{
		"no pod yet":     {pod: nil, phase: "Pending"},
		"pod pending":    {pod: pod(corev1.PodPending), phase: "Pending"},
		"pod ready":      {pod: pod(corev1.PodRunning, ready), phase: "Running"},
		"harness exit 0": {prev: running, pod: pod(corev1.PodSucceeded, harnessExit(0)), phase: "Succeeded", fin: five},
		"harness exit 1": {prev: running, pod: pod(corev1.PodFailed, harnessExit(1)), phase: "Failed", reason: "PodFailed", fin: five},
		"harness exit 2": {prev: running, pod: pod(corev1.PodFailed, harnessExit(2)), phase: "Failed", reason: "PodFailed", fin: five},
		"exit 143 evicted": {prev: running, pod: pod(corev1.PodFailed, harnessExit(143), func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue}}
		}), phase: "Failed", reason: "Disrupted", fin: now.Format(time.RFC3339)},
		"exit 143 deleted":           {prev: running, pod: pod(corev1.PodRunning, harnessExit(143), func(p *corev1.Pod) { d := metav1.NewTime(now); p.DeletionTimestamp = &d }), phase: "Failed", reason: "PodLost", fin: now.Format(time.RFC3339)},
		"exit 137 pod failed":        {prev: running, pod: pod(corev1.PodFailed, harnessExit(137)), phase: "Failed", reason: "PodFailed", fin: now.Format(time.RFC3339)},
		"exit 143 pod still running": {prev: running, pod: pod(corev1.PodRunning, harnessExit(143)), phase: "Running"},
		"deadline":                   {prev: running, pod: pod(corev1.PodFailed, func(p *corev1.Pod) { p.Status.Reason = "DeadlineExceeded" }), phase: "Failed", reason: "PodFailed", fin: now.Format(time.RFC3339)},
		"disrupted": {prev: running, pod: pod(corev1.PodFailed, func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue}}
		}), phase: "Failed", reason: "Disrupted", fin: now.Format(time.RFC3339)},
		"pod gone while running":     {prev: running, pod: nil, gone: true, phase: "Failed", reason: "PodLost", fin: now.Format(time.RFC3339)},
		"pod gone while pending":     {prev: v1alpha1.AgentRunStatus{Phase: "Pending"}, pod: nil, gone: true, phase: "Failed", reason: "PodLost", fin: now.Format(time.RFC3339)},
		"pod deleting while running": {prev: running, pod: pod(corev1.PodRunning, func(p *corev1.Pod) { d := metav1.NewTime(now); p.DeletionTimestamp = &d }), phase: "Failed", reason: "PodLost", fin: now.Format(time.RFC3339)},
		"manual revoke":              {prev: running, revoked: "manual", pod: pod(corev1.PodRunning, ready), phase: "Revoked", reason: "manual", fin: now.Format(time.RFC3339)},
		"budget revoke":              {prev: running, revoked: "budget-run", pod: pod(corev1.PodRunning, ready), phase: "BudgetExhausted", reason: "budget-run", fin: now.Format(time.RFC3339)},
		"unknown revoke ignored":     {prev: running, revoked: "because", pod: pod(corev1.PodRunning, ready), phase: "Running"},
		"terminal is final":          {prev: v1alpha1.AgentRunStatus{Phase: "Succeeded", FinishedAt: five}, revoked: "manual", pod: nil, gone: true, phase: "Succeeded", fin: five},
	} {
		got := Observe(tc.prev, tc.revoked, tc.pod, tc.gone, now)
		if got.Phase != tc.phase || got.Reason != tc.reason || got.FinishedAt != tc.fin {
			t.Errorf("%s: got phase %q reason %q finishedAt %q, want %q %q %q", name, got.Phase, got.Reason, got.FinishedAt, tc.phase, tc.reason, tc.fin)
		}
		if tc.pod != nil && tc.phase != "Pending" && got.StartedAt == "" && tc.prev.StartedAt == "" {
			t.Errorf("%s: startedAt unset", name)
		}
	}
}

func TestObserveStartedAtUsesPodStartTime(t *testing.T) {
	start := t0.Add(3 * time.Minute)
	p := pod(corev1.PodRunning, ready, func(p *corev1.Pod) { p.Status.StartTime = &metav1.Time{Time: start} })
	if got := Observe(v1alpha1.AgentRunStatus{}, "", p, false, now); got.StartedAt != start.Format(time.RFC3339) {
		t.Errorf("startedAt %q, want the pod's StartTime %q", got.StartedAt, start.Format(time.RFC3339))
	}
	p.Status.StartTime = nil
	if got := Observe(v1alpha1.AgentRunStatus{}, "", p, false, now); got.StartedAt != t0.Format(time.RFC3339) {
		t.Errorf("startedAt %q, want the creationTimestamp %q without a StartTime", got.StartedAt, t0.Format(time.RFC3339))
	}
}
