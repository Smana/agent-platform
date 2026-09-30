// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Bounds (review I3). etcd stores every Task, and the API server costs each CEL rule against
// the declared bounds, so every free-form string carries a maxLength and every list a maxItems.
// Each bound sits well above the largest legitimate value:
//
//   - 8, 64: a C2 id; a sha256 in hex, which also holds a SHA-256 Git object id (SHA-1 is 40).
//   - 16, 32, 64: short vocabularies (confidence, fit, verdict, tier); names from the config or
//     C5 (predicted class, model, AgentRun phase); GitHub logins (39) with a "[bot]" suffix.
//   - 128: a classifier name, a GitHub node id (~30 today), requestedBy (github:<login>).
//   - 140: owner/name, from GitHub's own limits: 39 for an owner, 100 for a repository.
//   - 253: a room name, a Kubernetes object name.
//   - 512: source.key and source.ref, which can hold an alert and a namespaced resource.
//   - 1024: a reason, one human-readable sentence; the log of record is the room.
//   - 2048: a URL, the de-facto limit browsers and proxies accept.
//   - 65536: the text snapshot (R6 caps it far lower at admission).
//   - Lists: narrated and handled grow with every comment over a task's life, so they hold 512
//     and their writers trim the oldest (narrate, for narrated); runs holds 256, far above
//     review rounds, fix runs and retries combined; shadow holds 16 classifiers.

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
	// +kubebuilder:validation:MaxLength=140
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
	// +kubebuilder:validation:MaxLength=64
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
	// +kubebuilder:validation:MaxLength=512
	Ref string `json:"ref"`
	// +kubebuilder:validation:MaxLength=512
	Key string `json:"key"`
	// github:<login>, system:runlore or system:scheduler.
	// +kubebuilder:validation:MaxLength=128
	RequestedBy string `json:"requestedBy"`
	// untrusted text is fenced as data for the harness (T1).
	// +kubebuilder:validation:Enum=untrusted;trusted
	Trust string `json:"trust"`
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	// +kubebuilder:validation:MaxLength=64
	ContentSHA256 string `json:"contentSHA256"`
}

// Budget is the task's tier and caps, resolved at triage.
type Budget struct {
	// +kubebuilder:validation:Enum=light;standard;frontier
	// +optional
	Tier string `json:"tier,omitempty"`
	// +kubebuilder:validation:MaxLength=64
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
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	PhaseSince *metav1.Time `json:"phaseSince,omitempty"`
	// The C7 answer, recorded as-is (§2).
	// +optional
	Classification *Classification `json:"classification,omitempty"`
	// +kubebuilder:validation:MaxItems=256
	// +optional
	Runs []RunRecord `json:"runs,omitempty"`
	// +kubebuilder:validation:MaxLength=253
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
	// +kubebuilder:validation:MaxLength=32
	// +optional
	Verdict string `json:"verdict,omitempty"`
	// Idempotency keys of the comments already posted (R22), and of the room messages
	// (room/<seq>/<key>). narrate trims the oldest: a display list, not a ledger (ruling SK).
	// +listType=set
	// +kubebuilder:validation:MaxItems=512
	// +kubebuilder:validation:items:MaxLength=512
	// +optional
	Narrated []string `json:"narrated,omitempty"`
	// Comments the task owes its issue or PR (ruling SO): written with the transition that caused
	// them, removed once posted. A task with any left is reconciled even after it ends.
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Outbox []Narration `json:"outbox,omitempty"`
	// The last clientSeq the task took for a task_state message in its room (ruling SK). The
	// broker keeps one message per clientSeq, so the ledger is its own field, never trimmed like
	// narrated, and only rises: the next message is roomSeq + 1, persisted before it is posted.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:XValidation:rule="self >= oldSelf",message="roomSeq never goes down: the broker would drop a reused clientSeq"
	// +optional
	RoomSeq int64 `json:"roomSeq,omitempty"`
	// GitHub review and comment ids already acted on (Δ5, commands). The reconciler trims the oldest.
	// +listType=set
	// +kubebuilder:validation:MaxItems=512
	// +optional
	Handled []int64 `json:"handled,omitempty"`
	// The config that triaged the task: the circuit breaker resets with a new one (§6.4).
	// +kubebuilder:validation:MaxLength=64
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
	// +kubebuilder:validation:MaxLength=16
	// +optional
	Confidence string `json:"confidence,omitempty"`
	// +kubebuilder:validation:MaxLength=128
	Classifier string `json:"classifier"`
	// +kubebuilder:validation:Enum=none;default;static
	Fallback string `json:"fallback"`
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Shadow []ShadowVerdict `json:"shadow,omitempty"`
	// OD-14: forced to tier-frontier whatever the classifier said.
	// +optional
	Control bool `json:"control,omitempty"`
	// Scored when the task ends (§7): under, over or fit.
	// +kubebuilder:validation:MaxLength=16
	// +optional
	Fit string `json:"fit,omitempty"`
}

// ShadowVerdict is a classifier consulted in shadow: recorded, never acted on.
type ShadowVerdict struct {
	// +kubebuilder:validation:MaxLength=128
	Classifier string `json:"classifier"`
	// +kubebuilder:validation:MaxLength=16
	Tier string `json:"tier"`
	// +kubebuilder:validation:MaxLength=16
	// +optional
	Confidence string `json:"confidence,omitempty"`
}

// RunRecord is one AgentRun the task started.
type RunRecord struct {
	// +kubebuilder:validation:Pattern=`^[a-z2-7]{8}$`
	// +kubebuilder:validation:MaxLength=8
	ID string `json:"id"`
	// +kubebuilder:validation:Enum=implementer;reviewer;tester;triager
	Role string `json:"role"`
	// Why the run exists.
	// +kubebuilder:validation:Enum=initial;review;human;ci;retry
	Trigger string `json:"trigger"`
	// +optional
	Round int32 `json:"round,omitempty"`
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Phase string `json:"phase,omitempty"`
	// The room's end reason (SP2 P15), else the AgentRun's.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Reason string `json:"reason,omitempty"`
	// +kubebuilder:validation:MaxLength=32
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
	Number int `json:"number"`
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`
	// +kubebuilder:validation:MaxLength=128
	// +optional
	NodeID string `json:"nodeID,omitempty"`
	// +kubebuilder:validation:MaxLength=64
	// +optional
	HeadSHA string `json:"headSHA,omitempty"`
	// +kubebuilder:validation:MaxLength=64
	// +optional
	MergeCommitSHA string `json:"mergeCommitSHA,omitempty"`
	// +kubebuilder:validation:MaxLength=64
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

// Narration is one pending comment. Its key is the comment's idempotency key: in status.narrated
// once posted, and in the comment's marker, which a retry finds when a post landed but failed.
type Narration struct {
	// +kubebuilder:validation:MaxLength=128
	Key string `json:"key"`
	// The issue or PR it goes to.
	// +kubebuilder:validation:Minimum=1
	Number int `json:"number"`
	// The factory's own words, never text a user wrote.
	// +kubebuilder:validation:MaxLength=4096
	Body string `json:"body"`
}
