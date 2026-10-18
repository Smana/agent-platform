// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// AgentRun is one agent run, for the run-controller (adoption 3a). Its spec is SP1's XRD's
// (crossplane-configuration apis/agentrun/definition.yaml), so a run manifest suits both
// runtimes. The name prefix is the controller's config, so only the id shape is checked here.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.role`
// +kubebuilder:printcolumn:name="Room",type=string,JSONPath=`.spec.roomRef`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.reason`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9][a-z0-9-]*-[a-z2-7]{8}$')",message="an AgentRun is named <prefix><runId>, runId being 8 characters of [a-z2-7]"
type AgentRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentRunSpec   `json:"spec"`
	Status            AgentRunStatus `json:"status,omitempty"`
}

// AgentRunList is a list of AgentRuns.
// +kubebuilder:object:root=true
type AgentRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentRun `json:"items"`
}

// AgentRunSpec is what the run does, on which repository, for whom.
// +kubebuilder:validation:XValidation:rule="self.role != 'reviewer' || (has(self.task.url) && self.task.url.matches('^https://github\\\\.com/[^/]+/[^/]+/pull/[0-9]+$'))",message="a reviewer run needs a pull request URL as its task"
// +kubebuilder:validation:XValidation:rule="!has(self.task.url) || self.task.url.startsWith('https://github.com/' + self.repository + '/')",message="task.url must point into spec.repository"
// +kubebuilder:validation:XValidation:rule="self.dataClass != 'internal' || !has(self.egress) || !has(self.egress.profiles) || size(self.egress.profiles) == 0",message="an internal run takes no egress profiles"
// +kubebuilder:validation:XValidation:rule="has(self.branch) == has(oldSelf.branch) && (!has(self.branch) || self.branch == oldSelf.branch)",message="branch is immutable and cannot be added or removed after creation"
// +kubebuilder:validation:XValidation:rule="has(self.roomRef) == has(oldSelf.roomRef) && (!has(self.roomRef) || self.roomRef == oldSelf.roomRef)",message="roomRef is immutable and cannot be added or removed after creation"
// +kubebuilder:validation:XValidation:rule="has(self.queueName) == has(oldSelf.queueName) && (!has(self.queueName) || self.queueName == oldSelf.queueName)",message="queueName is immutable and cannot be added or removed after creation"
// +kubebuilder:validation:XValidation:rule="has(self.suspendable) == has(oldSelf.suspendable) && (!has(self.suspendable) || self.suspendable == oldSelf.suspendable)",message="suspendable is immutable and cannot be added or removed after creation"
type AgentRunSpec struct {
	// +kubebuilder:validation:Enum=implementer;reviewer;tester;triager
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="role is immutable"
	Role string `json:"role"`
	// +kubebuilder:validation:MaxLength=140
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="repository is immutable"
	Repository string `json:"repository"`
	// +kubebuilder:default=main
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9]+(?:(?:[_/-]+(?:\.[_/-]*)?|\.[_/-]*)[A-Za-z0-9]+)*$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="baseRef is immutable"
	// +optional
	BaseRef string `json:"baseRef,omitempty"`
	// +kubebuilder:validation:Pattern=`^agent/[a-z0-9][a-z0-9._/-]{0,100}$`
	// +optional
	Branch string       `json:"branch,omitempty"`
	Task   AgentRunTask `json:"task"`
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^(human:[A-Za-z0-9@._-]+|system:[a-z0-9-]+)$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="principal is immutable"
	Principal string `json:"principal"`
	// A gateway tier on cloud-native-ref; the run-controller ignores it (3a R7).
	// +kubebuilder:validation:Enum=agent-default;tier-light;tier-standard;tier-frontier
	// +kubebuilder:default=agent-default
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="model is immutable"
	// +optional
	Model string `json:"model,omitempty"`
	// +kubebuilder:validation:Enum=public;internal
	DataClass string `json:"dataClass"`
	// +kubebuilder:default={}
	// +optional
	Budget AgentRunBudget `json:"budget,omitzero"`
	// +kubebuilder:validation:Enum=openhands
	// +kubebuilder:default=openhands
	// +optional
	Harness string `json:"harness,omitempty"`
	// +kubebuilder:validation:Enum=small;medium;large
	// +kubebuilder:default=small
	// +optional
	Size string `json:"size,omitempty"`
	// +kubebuilder:default={}
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="egress is immutable"
	// +optional
	Egress AgentRunEgress `json:"egress,omitzero"`
	// +kubebuilder:validation:Pattern=`^[a-z2-7]{8}$`
	// +optional
	RoomRef string `json:"roomRef,omitempty"`
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	QueueName string `json:"queueName,omitempty"`
	// +optional
	Suspendable bool `json:"suspendable,omitempty"`
}

// AgentRunTask is exactly one of text or url.
// +kubebuilder:validation:XValidation:rule="(has(self.text) ? 1 : 0) + (has(self.url) ? 1 : 0) == 1",message="set exactly one of task.text or task.url"
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="task is immutable"
type AgentRunTask struct {
	// +kubebuilder:validation:MaxLength=16384
	// +optional
	Text string `json:"text,omitempty"`
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^https://github\.com/[A-Za-z0-9-]+/[A-Za-z0-9._-]+/(issues|pull)/[0-9]+$`
	// +optional
	URL string `json:"url,omitempty"`
}

// AgentRunBudget bounds the run. Without a gateway, only maxMinutes is enforced.
type AgentRunBudget struct {
	// +kubebuilder:default=2000000
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5000000
	// +optional
	MaxTokens int64 `json:"maxTokens,omitempty"`
	// +kubebuilder:default=120
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=480
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="budget.maxMinutes is immutable"
	// +optional
	MaxMinutes int64 `json:"maxMinutes,omitempty"`
}

// AgentRunEgress adds package registries to the always-on github profile.
type AgentRunEgress struct {
	// +kubebuilder:default={}
	// +kubebuilder:validation:MaxItems=4
	// +listType=set
	// +optional
	Profiles []AgentRunEgressProfile `json:"profiles,omitempty"`
}

// AgentRunEgressProfile is one registry profile; github is not one, it is always on.
// +kubebuilder:validation:Enum=pypi;npm;golang;crates
type AgentRunEgressProfile string

// AgentRunStatus is what runwatch and the factory read (C3), as the composition writes it.
type AgentRunStatus struct {
	// +kubebuilder:validation:Enum=Pending;Running;Suspended;Succeeded;Failed;BudgetExhausted;Revoked
	// +optional
	Phase          string `json:"phase,omitempty"`
	RunID          string `json:"runId,omitempty"`
	Branch         string `json:"branch,omitempty"`
	ConversationID string `json:"conversationId,omitempty"`
	StartedAt      string `json:"startedAt,omitempty"`
	FinishedAt     string `json:"finishedAt,omitempty"`
	Reason         string `json:"reason,omitempty"`
	PullRequest    string `json:"pullRequest,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
