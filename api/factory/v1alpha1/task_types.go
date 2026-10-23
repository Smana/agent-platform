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
// +kubebuilder:validation:XValidation:rule="has(self.parent) == has(oldSelf.parent) && (!has(self.parent) || self.parent == oldSelf.parent)",message="spec.parent is fixed at creation"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.scope) || (has(self.scope) && self.scope == oldSelf.scope)",message="spec.scope never changes once set"
// +kubebuilder:validation:XValidation:rule="self.source.kind != 'api' || (has(self.maxMicroUSD) && self.maxMicroUSD > 0)",message="an api task carries maxMicroUSD"
// +kubebuilder:validation:XValidation:rule="self.source.kind != 'api' || !has(self.budget) || (has(self.maxMicroUSD) && (!has(self.budget.runMicroUSD) || self.budget.runMicroUSD <= self.maxMicroUSD) && (!has(self.budget.taskMicroUSD) || self.budget.taskMicroUSD <= self.maxMicroUSD))",message="an api task's budget stays under its ceiling"
type TaskSpec struct {
	Source Source `json:"source"`
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`
	// +kubebuilder:validation:MaxLength=140
	Repository string `json:"repository"`
	// The issue the factory narrates on; 0 for a task without one (R28).
	// +kubebuilder:validation:Minimum=0
	// +optional
	Issue int `json:"issue,omitempty"`
	// The issue's author on GitHub, for the room list's "mine" filter.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	IssueAuthor string `json:"issueAuthor,omitempty"`
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
	// The loop policy its implementer runs get as spec.loop, resolved from loops.<class> at
	// triage so a config change never moves a task already triaged (loops design L2, L3). Unset:
	// the loop is off for the task's class.
	// +optional
	Loop *Loop `json:"loop,omitempty"`
	// The findings an oracle class's probe made this task of (merge gate classes MC6): what the
	// run is asked to fix, and what the gate's oracle checks the head no longer holds.
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Findings []Finding `json:"findings,omitempty"`
	// The class's scope mode (scoper design SC2), fixed at triage so a config change never moves a
	// task already triaged. Empty: triaged before the scoper existed, which reads as off.
	// +kubebuilder:validation:Enum=off;steering;scoper
	// +optional
	ScopeMode string `json:"scopeMode,omitempty"`
	// The class's steering note, read on the default branch at triage (SC3); unset for none.
	// +optional
	Steering *Steering `json:"steering,omitempty"`
	// The request's ceiling in micro-USD (F4, R5): triage caps the task and each run under it. Set
	// on api tasks only; 0 elsewhere.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000000
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="maxMicroUSD never changes"
	// +optional
	MaxMicroUSD int64 `json:"maxMicroUSD,omitempty"`
	// Search offers its implementer runs the search tool (v3 feature 5, ST9), read from
	// loops.<class>.search at triage so a config change never moves a task already triaged; every
	// implementer claim of the task carries spec.loop.search.
	// +optional
	Search bool `json:"search,omitempty"`
	// The split the scoper may offer (splitter design SL3), recorded at triage when the class's row
	// has one and C7's own tier is frontier; unset: no split offered.
	// +optional
	Split *SplitSpec `json:"split,omitempty"`
	// The parent this task is a part of (SL5), fixed at creation; unset for any other task.
	// +optional
	Parent *Parent `json:"parent,omitempty"`
	// This child's part of its parent's task (SL5): one scope of the approved split, fixed once set.
	// +optional
	Scope *TaskScope `json:"scope,omitempty"`
}

// Finding is one dead external link two consecutive detector runs on main reported (MC5).
type Finding struct {
	// The Markdown file on main that holds the link.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	File string `json:"file"`
	// The link target, an https URL.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://`
	URL string `json:"url"`
	// The Wayback Machine's closest snapshot of URL, an https://web.archive.org/web/ URL; empty
	// when it has none.
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://web\.archive\.org/web/`
	// +optional
	Archive string `json:"archive,omitempty"`
}

// Steering is a steering note as triage read it: every run of the task sees these bytes.
type Steering struct {
	// +kubebuilder:validation:MaxLength=256
	Path string `json:"path"`
	// The note's git blob sha on the default branch.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	// +kubebuilder:validation:MaxLength=40
	SHA string `json:"sha"`
	// +kubebuilder:validation:MaxLength=2048
	Text string `json:"text"`
}

// Loop is a task's loop policy, every value resolved at triage.
type Loop struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=500
	StallSteps int32 `json:"stallSteps"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=5
	Continuations int32 `json:"continuations"`
	// The judge's alias; empty: no judge.
	// +kubebuilder:validation:Enum=agent-default;tier-light;tier-standard;tier-frontier
	// +optional
	JudgeModel string `json:"judgeModel,omitempty"`
}

// SplitSpec is the split a task's scoper is offered (SL3), resolved at triage.
type SplitSpec struct {
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=5
	MaxChildren int32 `json:"maxChildren"`
	// The family's cap in micro-dollars (SL10): the parent's and every child's runs together.
	// +kubebuilder:validation:Minimum=1
	FamilyMicroUSD int64 `json:"familyMicroUSD"`
	// +kubebuilder:validation:Enum=light;standard;frontier
	ChildTier string `json:"childTier"`
}

// Parent names a child's parent task and issue, and which part of how many it is (SL5).
// +kubebuilder:validation:XValidation:rule="self.index <= self.of",message="a part's index is at most its count"
type Parent struct {
	// +kubebuilder:validation:Pattern=`^[a-z2-7]{8}$`
	// +kubebuilder:validation:MaxLength=8
	Task string `json:"task"`
	// +kubebuilder:validation:Minimum=1
	Issue int `json:"issue"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5
	Index int32 `json:"index"`
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=5
	Of int32 `json:"of"`
}

// TaskScope mirrors envelope.Scope, with its bounds, in state ready (a part never asks): the CRD
// cannot import the envelope.
type TaskScope struct {
	// +kubebuilder:validation:Enum=ready
	State string `json:"state"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1000
	Task string `json:"task"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=10
	// +kubebuilder:validation:items:MaxLength=200
	Touches []string `json:"touches"`
	// +kubebuilder:validation:MaxItems=10
	// +kubebuilder:validation:items:MaxLength=200
	// +optional
	Leave []string `json:"leave,omitempty"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=7
	// +kubebuilder:validation:items:MaxLength=300
	Plan []string `json:"plan"`
	// +kubebuilder:validation:MaxItems=5
	// +kubebuilder:validation:items:MaxLength=300
	// +optional
	Assumptions []string `json:"assumptions,omitempty"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=5
	Acceptance []TaskAcceptance `json:"acceptance"`
}

// TaskAcceptance is one acceptance check of a part.
type TaskAcceptance struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=300
	Check string `json:"check"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=200
	Proves string `json:"proves"`
}

// Source is where the task came from and how far its text is trusted. No rule holds the whole
// source: the splitter's inherit() rewrites contentSHA256 of a re-labelled part, so only the
// fields F4 adds, and the kind, are held (C1).
// +kubebuilder:validation:XValidation:rule="self.kind != 'api' || (has(self.sponsor) && size(self.sponsor) > 0)",message="an api task names its sponsor"
// +kubebuilder:validation:XValidation:rule="self.kind == 'api' || (!has(self.sponsor) && !has(self.onBehalfOf))",message="only an api task has a sponsor"
// +kubebuilder:validation:XValidation:rule="self.kind == oldSelf.kind",message="a task's source kind never changes"
// +kubebuilder:validation:XValidation:rule="has(self.sponsor) == has(oldSelf.sponsor) && (!has(self.sponsor) || self.sponsor == oldSelf.sponsor)",message="source.sponsor never changes"
// +kubebuilder:validation:XValidation:rule="has(self.onBehalfOf) == has(oldSelf.onBehalfOf) && (!has(self.onBehalfOf) || self.onBehalfOf == oldSelf.onBehalfOf)",message="source.onBehalfOf never changes"
type Source struct {
	// adopt: another author's pull request the factory decides (merge gate MC10).
	// +kubebuilder:validation:Enum=issue;runlore;schedule;api;adopt
	Kind string `json:"kind"`
	// Smana/cloud-native-ref#2112 (owner/name#<n>), runlore:<alert>/<resource>, or the schedule name.
	// +kubebuilder:validation:MaxLength=512
	Ref string `json:"ref"`
	// +kubebuilder:validation:MaxLength=512
	Key string `json:"key"`
	// github:<login> (an adopted pull request's author too), system:runlore or system:scheduler.
	// +kubebuilder:validation:MaxLength=128
	RequestedBy string `json:"requestedBy"`
	// untrusted text is fenced as data for the harness (T1).
	// +kubebuilder:validation:Enum=untrusted;trusted
	Trust string `json:"trust"`
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	// +kubebuilder:validation:MaxLength=64
	ContentSHA256 string `json:"contentSHA256"`
	// The maintainer accountable for an api task, as a ledger key (F4, R8): github:<login>.
	// +kubebuilder:validation:Pattern=`^github:[a-z0-9][a-z0-9-]{0,38}$`
	// +kubebuilder:validation:MaxLength=46
	// +optional
	Sponsor string `json:"sponsor,omitempty"`
	// The external user a link made the sponsor (R9): slack:<team>/<user>.
	// +kubebuilder:validation:Pattern=`^slack:T[A-Z0-9]{2,20}/[UW][A-Z0-9]{2,20}$`
	// +kubebuilder:validation:MaxLength=64
	// +optional
	OnBehalfOf string `json:"onBehalfOf,omitempty"`
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
	// The USD caps in micro-dollars (OD-6 (b)): a run's, and the task's in place of TaskTokens
	// while every run of the task is priced. 0: the token caps.
	// +kubebuilder:validation:Minimum=0
	// +optional
	RunMicroUSD int64 `json:"runMicroUSD,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	TaskMicroUSD int64 `json:"taskMicroUSD,omitempty"`
}

// TaskStatus is the task's audit record: the whole §4 record, including the fields later
// phases fill.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.trace) || has(self.trace)",message="a task's trace is never removed: its runs are parented on it"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.usageSettled) || !oldSelf.usageSettled || (has(self.usageSettled) && self.usageSettled)",message="settled usage stays settled"
type TaskStatus struct {
	// +kubebuilder:validation:Enum=Received;Rejected;Triaged;Queued;Implementing;NoOp;Reviewing;AwaitingCI;AutoMerging;AwaitingHuman;Merged;Verifying;Done;Reverted;Escalated;Closed;Stopped;Split
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
	// The post-merge monitor (merge gate classes MC7), for a class that declares one: each signal's
	// reading and the verdict, the worst of them. Unset until the task ends merged.
	// +optional
	Monitor *MonitorStatus `json:"monitor,omitempty"`
	// +optional
	Usage Usage `json:"usage,omitempty"`
	// The task's usage has settled (R49): the meter's late readings have had their window, and
	// the total is final. Written once, with the one TaskTokens the settle records.
	// +optional
	UsageSettled bool `json:"usageSettled,omitempty"`
	// +optional
	ReviewRounds int32 `json:"reviewRounds,omitempty"`
	// +optional
	FixRuns int32 `json:"fixRuns,omitempty"`
	// Re-runs of a cancelled required check, capped by merge.checkReruns. A cancelled check is
	// not a failure, so it never spends a fix run.
	// +optional
	CheckReruns int32 `json:"checkReruns,omitempty"`
	// The head CheckReruns counts against: a new head resets it.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	CheckRerunsHead string `json:"checkRerunsHead,omitempty"`
	// +optional
	Retries int32 `json:"retries,omitempty"`
	// RetriedBy is the maintainer whose /factory retry sent the task back, github:<login>: the
	// person its later runs are charged to (v2 per-person caps D2). Empty: the labeller.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	RetriedBy string `json:"retriedBy,omitempty"`
	// Automatic resumes of runs lost to their infrastructure (disruption design §4), every role
	// counted; capped by the config's resume.maxPerTask.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Resumes int32 `json:"resumes,omitempty"`
	// Returns from a transient AwaitingHuman hand-off to the merge gate (N1), every cause
	// counted; capped by the reconciler, so a flapping input cannot loop the gate.
	// +kubebuilder:validation:Minimum=0
	// +optional
	GateReentries int32 `json:"gateReentries,omitempty"`
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
	// The last clientSeq the task took for a task_state message or its facts in its room (ruling
	// SK). The broker keeps one message per clientSeq, so the ledger is its own field, never trimmed
	// like narrated, and only rises: the next message is roomSeq + 1, persisted before it is posted.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:XValidation:rule="self >= oldSelf",message="roomSeq never goes down: the broker would drop a reused clientSeq"
	// +optional
	RoomSeq int64 `json:"roomSeq,omitempty"`
	// The task facts last written to the room: their seq comes from the roomSeq ledger (ruling SK).
	// +optional
	Facts *FactsLedger `json:"facts,omitempty"`
	// The trigger of the next implementer run, set when the task goes back to Queued (Δ5) and
	// cleared once that run exists.
	// +kubebuilder:validation:Enum=initial;review;human;ci;retry;resume
	// +optional
	NextTrigger string `json:"nextTrigger,omitempty"`
	// The scoper's outcome (scoper design SC12), set once when its run ends and kept when the task
	// later stops: ready, question, assumed, missing or failed, and the seq of the handoff that
	// carries the scope (0 for none). Unset: no scoper ran, or it has not ended.
	// +optional
	Scope *ScopeStatus `json:"scope,omitempty"`
	// The split's record on its parent (splitter design SL14): set when the scoper hands one off.
	// +optional
	Split *SplitStatus `json:"split,omitempty"`
	// The verifier the task went back to Queued for (§3): the next reviewer or tester run starts
	// there, behind the caps and the human-driver rule (C4). Cleared once that run exists.
	// +kubebuilder:validation:Enum=reviewer;tester
	// +optional
	NextRole string `json:"nextRole,omitempty"`
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
	// The task's root span (R46): minted at acceptance, exported once when the task ends.
	// +optional
	Trace *TraceRef `json:"trace,omitempty"`
}

// ScopeStatus is the scoper's outcome on its task.
type ScopeStatus struct {
	// +kubebuilder:validation:Enum=ready;question;assumed;missing;failed
	State string `json:"state"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	Seq int64 `json:"seq,omitempty"`
}

// SplitStatus is a split as its parent keeps it (SL14).
type SplitStatus struct {
	// +kubebuilder:validation:Enum=proposed;approved;declined;superseded;expired;ignored
	State string `json:"state"`
	// The room seq of the handoff that carries the split.
	// +kubebuilder:validation:Minimum=1
	Seq int64 `json:"seq"`
	// How many parts the proposed split holds; 0 for an ignored one.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=5
	// +optional
	Of int32 `json:"of,omitempty"`
	// The maintainer whose /factory split approved it, github:<login>: every child's payer (SL11).
	// +kubebuilder:validation:MaxLength=64
	// +optional
	ApprovedBy string `json:"approvedBy,omitempty"`
	// The parts opened so far, in order, each with its newest generation's task.
	// +kubebuilder:validation:MaxItems=5
	// +optional
	Children []SplitChild `json:"children,omitempty"`
}

// SplitChild is one opened part.
type SplitChild struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5
	Index int32 `json:"index"`
	// +kubebuilder:validation:Minimum=1
	Issue int `json:"issue"`
	// +kubebuilder:validation:Pattern=`^[a-z2-7]{8}$`
	// +kubebuilder:validation:MaxLength=8
	Task string `json:"task"`
	// +kubebuilder:validation:Enum=landed;dropped;reverted
	// +optional
	Outcome string `json:"outcome,omitempty"`
}

// FactsLedger is which facts a room seq carries, and whether the broker has them.
type FactsLedger struct {
	// +kubebuilder:validation:Minimum=1
	Seq int64 `json:"seq"`
	// +kubebuilder:validation:MaxLength=16
	Hash string `json:"hash"`
	// +optional
	Posted bool `json:"posted,omitempty"`
}

// TraceRef names the task's root span; the factory exports it from these ids at the end (R46).
// The ids never change once minted: every run of the task is parented on them.
// +kubebuilder:validation:XValidation:rule="self.traceID == oldSelf.traceID && self.spanID == oldSelf.spanID",message="a task's trace ids never change"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.exported) || !oldSelf.exported || (has(self.exported) && self.exported)",message="an exported span stays exported"
type TraceRef struct {
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{32}$`
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="self != '00000000000000000000000000000000'",message="an all-zero trace id is invalid W3C"
	TraceID string `json:"traceID"`
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{16}$`
	// +kubebuilder:validation:MaxLength=16
	// +kubebuilder:validation:XValidation:rule="self != '0000000000000000'",message="an all-zero span id is invalid W3C"
	SpanID string `json:"spanID"`
	// +optional
	Exported bool `json:"exported,omitempty"`
	// The collector refused the span for a day after the task ended: given up, never retried (ST2).
	// +optional
	ExportAbandoned bool `json:"exportAbandoned,omitempty"`
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
	// +kubebuilder:validation:Enum=initial;review;human;ci;retry;resume
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
	// The run's cost in micro-dollars, cache reads at their own rate, as the meter priced it.
	// +optional
	CostMicroUSD int64 `json:"costMicroUSD,omitempty"`
	// Why the meter could not price the run: its task is then held to TaskTokens.
	// +kubebuilder:validation:Enum=unpriced;untyped
	// +optional
	Unpriced string `json:"unpriced,omitempty"`
	// The person the run was charged to beside the fleet (v2 per-person caps D2), github:<login>;
	// empty when per-person caps were off or no person caused it.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	ChargedTo string `json:"chargedTo,omitempty"`
	// The room's lastSeq when the run was created: verdicts are read after it.
	// +optional
	StartSeq int64 `json:"startSeq,omitempty"`
	// The pull request's head a reviewer or tester run was given: its approve counts for that
	// commit only, and only while the head has not moved.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	HeadSHA string `json:"headSHA,omitempty"`
	// +optional
	Started *metav1.Time `json:"started,omitempty"`
	// +optional
	Finished *metav1.Time `json:"finished,omitempty"`
	// What the run ran on (R10), each field filled once as it becomes known.
	// +optional
	Provenance RunProvenance `json:"provenance,omitempty"`
}

// RunProvenance is what a run ran on (R10). The reconciler fills each field once, from the
// run's claim or the meter, and never changes it; ServedModels only grows. Empty: not known
// yet, or the claim predates the field — never a guess.
type RunProvenance struct {
	// The factory build that created the claim (version.Version), from its CREATE-only annotation.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	FactoryVersion string `json:"factoryVersion,omitempty"`
	// The CompositionRevision Crossplane bound the claim to, as first read.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	CompositionRevision string `json:"compositionRevision,omitempty"`
	// The harness container's imageID, <repository>@sha256:<digest>, as the composition read it
	// from the run's pod.
	// +kubebuilder:validation:MaxLength=320
	// +optional
	HarnessImage string `json:"harnessImage,omitempty"`
	// The models that served the run, as the meter read gen_ai_request_model; sorted.
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=64
	// +optional
	ServedModels []string `json:"servedModels,omitempty"`
}

// IsZero reports whether no fact is known yet.
func (p RunProvenance) IsZero() bool {
	return p.FactoryVersion == "" && p.CompositionRevision == "" && p.HarnessImage == "" && len(p.ServedModels) == 0
}

// MonitorStatus is one merge's watch. A signal is "" while open, then healthy, regression or
// inconclusive; Verdict is set at the first regression, or when every signal is final.
type MonitorStatus struct {
	Since metav1.Time `json:"since"`
	// +kubebuilder:validation:Enum="";healthy;regression;inconclusive
	// +optional
	Main string `json:"main,omitempty"`
	// +kubebuilder:validation:Enum="";healthy;regression;inconclusive
	// +optional
	Oracle string `json:"oracle,omitempty"`
	// +kubebuilder:validation:Enum="";healthy;regression;inconclusive
	// +optional
	Flux string `json:"flux,omitempty"`
	// +kubebuilder:validation:Enum="";healthy;regression;inconclusive
	// +optional
	Verdict string `json:"verdict,omitempty"`
	// Later pull requests that fixed this merge (MC8).
	// +kubebuilder:validation:MaxItems=16
	// +optional
	FollowUps []int `json:"followUps,omitempty"`
	// The last follow-up read could not say (a failed read, no blame, an omitted patch, a
	// truncated search): the watch cannot end healthy while it holds.
	// +optional
	FollowUpsUnread bool `json:"followUpsUnread,omitempty"`
	// The detector runs read after the merge (R37).
	// +kubebuilder:validation:MaxItems=8
	// +optional
	OracleRuns []int64 `json:"oracleRuns,omitempty"`
	// The Flux source's fetch time the first pass that saw it hold the merge commit (R33): an upper
	// bound on when the merge was applied. The blame window and the settle time end `within` later.
	// +optional
	Applied *metav1.Time `json:"applied,omitempty"`
	// Where the Flux blame window starts: the source's fetch time when the first observed revision
	// is the merge commit, else the merge's own time (the merge was fetched somewhere before Applied).
	// +optional
	BlameFrom *metav1.Time `json:"blameFrom,omitempty"`
	// The Flux objects found not Ready in the window, kind/namespace/name, the first five.
	// +kubebuilder:validation:MaxItems=5
	// +kubebuilder:validation:items:MaxLength=320
	// +optional
	FluxBlamed []string `json:"fluxBlamed,omitempty"`
	// The verdict comment is on the pull request.
	// +optional
	Posted bool `json:"posted,omitempty"`
}

// PullRequestRef is the task's pull request and what happened to it.
type PullRequestRef struct {
	Number int `json:"number"`
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`
	// +kubebuilder:validation:MaxLength=128
	// +optional
	NodeID string `json:"nodeID,omitempty"`
	// The head when the pull request was found, then the head the merge gate last acted on: merged,
	// or handed to a human (N1: a push after the hand-off keeps it with the human).
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
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Author string `json:"author,omitempty"`
	// Everyone who has submitted a review, oldest first.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=64
	// +optional
	Reviewers []string `json:"reviewers,omitempty"`
}

// Usage is the task's token spend.
type Usage struct {
	// Σ of the task's run usage; it never drops (SP1 §2).
	// +optional
	Tokens int64 `json:"tokens,omitempty"`
	// Σ of the task's run costs, in micro-dollars.
	// +optional
	CostMicroUSD int64 `json:"costMicroUSD,omitempty"`
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
