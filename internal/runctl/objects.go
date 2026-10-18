// SPDX-License-Identifier: Apache-2.0

package runctl

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/Smana/agent-platform/api/v1alpha1"
)

// Label keys the composition sets; the CNP selects the pod by run-id.
const (
	LabelRunID     = "agents.ogenki.io/run-id"
	LabelRole      = "agents.ogenki.io/role"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelTask      = "agents.ogenki.io/task"
	managedBy      = "run-controller"
)

// Labels are every owned object's.
func Labels(r *v1alpha1.AgentRun, runID string) map[string]string {
	l := map[string]string{LabelRunID: runID, LabelRole: r.Spec.Role, LabelManagedBy: managedBy}
	if v := r.Labels[LabelTask]; v != "" {
		l[LabelTask] = v
	}
	return l
}

// Branch is spec.branch, else agent/<runId>, as the composition defaults it.
func Branch(r *v1alpha1.AgentRun, runID string) string {
	if r.Spec.Branch != "" {
		return r.Spec.Branch
	}
	return "agent/" + runID
}

func meta(r *v1alpha1.AgentRun, runID, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: r.Namespace, Labels: Labels(r, runID),
		Annotations: map[string]string{"agents.ogenki.io/principal": r.Spec.Principal}}
}

// ServiceAccount is named exactly like the run: the broker's run issuer maps
// system:serviceaccount:<ns>:<run name> to the run (C2). It holds no RBAC.
func ServiceAccount(r *v1alpha1.AgentRun) *corev1.ServiceAccount {
	no := false
	sa := &corev1.ServiceAccount{ObjectMeta: meta(r, "", r.Name), AutomountServiceAccountToken: &no}
	delete(sa.Labels, LabelRunID)
	return sa
}

// TaskConfigMap is <run>-task: task.md, rules.md and run.json, as the composition writes them.
func TaskConfigMap(r *v1alpha1.AgentRun, runID string) (*corev1.ConfigMap, error) {
	task := r.Spec.Task.Text
	if task == "" {
		task = fmt.Sprintf("Work on %s.\n", r.Spec.Task.URL)
	}
	runJSON, err := json.Marshal(map[string]string{"runId": runID, "role": r.Spec.Role, "repository": r.Spec.Repository,
		"baseRef": r.Spec.BaseRef, "branch": Branch(r, runID), "dataClass": r.Spec.DataClass, "principal": r.Spec.Principal})
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{ObjectMeta: meta(r, runID, r.Name+"-task"),
		Data: map[string]string{"task.md": task, "rules.md": Rules(r, runID), "run.json": string(runJSON)}}, nil
}

// CAConfigMap is <run>-ca: the broker's CA, which the bridge verifies :8443 against.
func CAConfigMap(r *v1alpha1.AgentRun, ca []byte) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{ObjectMeta: meta(r, "", r.Name+"-ca"), Data: map[string]string{"ca.crt": string(ca)}}
	delete(cm.Labels, LabelRunID)
	return cm
}
