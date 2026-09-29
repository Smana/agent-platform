// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Task is one unit of factory work (SP3 §4). The factory creates it at runtime and never
// commits it to Git. Its name derives from the idempotency key, so AlreadyExists is the dedup.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source.ref`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.predictedClass`
// +kubebuilder:printcolumn:name="Template",type=string,JSONPath=`.spec.template`
// +kubebuilder:printcolumn:name="PR",type=string,JSONPath=`.status.pullRequest.url`
// +kubebuilder:printcolumn:name="Tokens",type=integer,JSONPath=`.status.usage.tokens`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z2-7]{8}$')",message="a Task is named with a C2 id: 8 characters of [a-z2-7]"
type Task struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TaskSpec   `json:"spec"`
	Status            TaskStatus `json:"status,omitempty"`
}

// TaskList is a list of Tasks.
// +kubebuilder:object:root=true
type TaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Task `json:"items"`
}

// TaskSpec is what the task was asked to do, fixed at admission.
type TaskSpec struct {
	Source Source `json:"source"`
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`
	Repository string `json:"repository"`
	// The issue the factory narrates on; 0 for a task without one (R28).
	// +kubebuilder:validation:Minimum=0
	// +optional
	Issue int `json:"issue,omitempty"`
	// The snapshot. Admission caps it at caps.maxTextBytes (R6); this bound only protects etcd.
	// +kubebuilder:validation:MaxLength=65536
	Text string `json:"text"`
	// Every run of the task gets it as spec.dataClass (C3, OD-13).
	// +kubebuilder:validation:Enum=public;internal
	DataClass string `json:"dataClass"`
	// Intent, not authority: it picks team and budget; only policy-bot merges (§2).
	// +optional
	PredictedClass string `json:"predictedClass,omitempty"`
	// +kubebuilder:validation:Enum=solo;pair;trio;investigate
	// +optional
	Template string `json:"template,omitempty"`
	// +optional
	Budget Budget `json:"budget,omitempty"`
}

// Source is where the task came from and how far its text is trusted.
type Source struct {
	// +kubebuilder:validation:Enum=issue;runlore;schedule
	Kind string `json:"kind"`
	// Smana/cloud-native-ref#2112, runlore:<alert>/<resource>, or the schedule name.
	Ref string `json:"ref"`
	// +kubebuilder:validation:MaxLength=512
	Key string `json:"key"`
	// github:<login>, system:runlore or system:scheduler.
	RequestedBy string `json:"requestedBy"`
	// untrusted text is fenced as data for the harness (T1).
	// +kubebuilder:validation:Enum=untrusted;trusted
	Trust string `json:"trust"`
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	ContentSHA256 string `json:"contentSHA256"`
}

// Budget is the task's tier and caps, resolved at triage.
type Budget struct {
	// +kubebuilder:validation:Enum=light;standard;frontier
	// +optional
	Tier string `json:"tier,omitempty"`
	// +optional
	Model string `json:"model,omitempty"`
	// The per-run cap; SP1's XRD refuses more than the gateway ceiling (C3).
	// +kubebuilder:validation:Maximum=5000000
	// +optional
	RunTokens int64 `json:"runTokens,omitempty"`
	// +optional
	TaskTokens int64 `json:"taskTokens,omitempty"`
	// +kubebuilder:validation:Maximum=480
	// +optional
	RunMinutes int64 `json:"runMinutes,omitempty"`
}

// TaskStatus is the task's audit record: the whole §4 record, including the fields later
// phases fill.
type TaskStatus struct {
	// +kubebuilder:validation:Enum=Received;Rejected;Triaged;Queued;Implementing;NoOp;Reviewing;AwaitingCI;AutoMerging;AwaitingHuman;Merged;Verifying;Done;Reverted;Escalated;Closed;Stopped
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	PhaseSince *metav1.Time `json:"phaseSince,omitempty"`
	// The C7 answer, recorded as-is (§2).
	// +optional
	Classification *Classification `json:"classification,omitempty"`
	// +optional
	Runs []RunRecord `json:"runs,omitempty"`
	// +optional
	RoomRef string `json:"roomRef,omitempty"`
	// +optional
	PullRequest *PullRequestRef `json:"pullRequest,omitempty"`
	// +optional
	Usage Usage `json:"usage,omitempty"`
	// +optional
	ReviewRounds int32 `json:"reviewRounds,omitempty"`
	// +optional
	FixRuns int32 `json:"fixRuns,omitempty"`
	// +optional
	Retries int32 `json:"retries,omitempty"`
	// The effective verdict of the last review: approve, changes or none.
	// +optional
	Verdict string `json:"verdict,omitempty"`
	// Idempotency keys of the comments already posted (R22).
	// +listType=set
	// +optional
	Narrated []string `json:"narrated,omitempty"`
	// GitHub review and comment ids already acted on (Δ5, commands).
	// +listType=set
	// +optional
	Handled []int64 `json:"handled,omitempty"`
	// The config that triaged the task: the circuit breaker resets with a new one (§6.4).
	// +optional
	ConfigHash string `json:"configHash,omitempty"`
	// The last room event seen for the running run (stuck detection, §6.3).
	// +optional
	LastActivity *metav1.Time `json:"lastActivity,omitempty"`
}

// Classification is the complexity classifier's verdict on the task (C7).
type Classification struct {
	// +kubebuilder:validation:Enum=light;standard;frontier
	Tier string `json:"tier"`
	// 0.0–1.0, as a string: CRDs avoid floats.
	// +optional
	Confidence string `json:"confidence,omitempty"`
	Classifier string `json:"classifier"`
	// +kubebuilder:validation:Enum=none;default;static
	Fallback string `json:"fallback"`
	// +optional
	Shadow []ShadowVerdict `json:"shadow,omitempty"`
	// OD-14: forced to tier-frontier whatever the classifier said.
	// +optional
	Control bool `json:"control,omitempty"`
	// Scored when the task ends (§7): under, over or fit.
	// +optional
	Fit string `json:"fit,omitempty"`
}

// ShadowVerdict is a classifier consulted in shadow: recorded, never acted on.
type ShadowVerdict struct {
	Classifier string `json:"classifier"`
	Tier       string `json:"tier"`
	// +optional
	Confidence string `json:"confidence,omitempty"`
}

// RunRecord is one AgentRun the task started.
type RunRecord struct {
	// +kubebuilder:validation:Pattern=`^[a-z2-7]{8}$`
	ID string `json:"id"`
	// +kubebuilder:validation:Enum=implementer;reviewer;tester;triager
	Role string `json:"role"`
	// Why the run exists.
	// +kubebuilder:validation:Enum=initial;review;human;ci;retry
	Trigger string `json:"trigger"`
	// +optional
	Round int32 `json:"round,omitempty"`
	// +optional
	Phase string `json:"phase,omitempty"`
	// The room's end reason (SP2 P15), else the AgentRun's.
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Verdict string `json:"verdict,omitempty"`
	// +optional
	Tokens int64 `json:"tokens,omitempty"`
	// The room's lastSeq when the run was created: verdicts are read after it.
	// +optional
	StartSeq int64 `json:"startSeq,omitempty"`
	// +optional
	Started *metav1.Time `json:"started,omitempty"`
	// +optional
	Finished *metav1.Time `json:"finished,omitempty"`
}

// PullRequestRef is the task's pull request and what happened to it.
type PullRequestRef struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	// +optional
	NodeID string `json:"nodeID,omitempty"`
	// +optional
	HeadSHA string `json:"headSHA,omitempty"`
	// +optional
	MergeCommitSHA string `json:"mergeCommitSHA,omitempty"`
	// +optional
	MergedBy string `json:"mergedBy,omitempty"`
	// +optional
	AutoMerged bool `json:"autoMerged,omitempty"`
	// +optional
	ArmedAt *metav1.Time `json:"armedAt,omitempty"`
	// +optional
	MergedAt *metav1.Time `json:"mergedAt,omitempty"`
	// +optional
	RevertNumber int `json:"revertNumber,omitempty"`
}

// Usage is the task's token spend.
type Usage struct {
	// Σ of the task's run usage; it never drops (SP1 §2).
	// +optional
	Tokens int64 `json:"tokens,omitempty"`
}
