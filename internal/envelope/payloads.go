// SPDX-License-Identifier: Apache-2.0

package envelope

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// MessageKind distinguishes chat from SP3's reserved kinds.
type MessageKind string

// The message kinds.
const (
	KindChat          MessageKind = "chat"
	KindProgress      MessageKind = "progress"
	KindReviewVerdict MessageKind = "review_verdict" // SP3's reserved kind
	KindTaskState     MessageKind = "task_state"     // SP3's reserved kind
)

// Delivery is how a message reaches a running agent.
type Delivery string

// The delivery modes.
const (
	DeliveryNone     Delivery = "none"
	DeliveryQueued   Delivery = "queued"
	DeliverySteering Delivery = "steering"
)

// MessagePayload is a message event's payload.
type MessagePayload struct {
	Kind     MessageKind `json:"kind"`
	Text     string      `json:"text"`
	To       []string    `json:"to,omitempty"`
	Delivery Delivery    `json:"delivery"`
	Verdict  string      `json:"verdict,omitempty"` // approve | changes (review_verdict only)
	Commit   string      `json:"commit,omitempty"`
	// PullRequest is the pull request a review_verdict is about (SP2 design §3).
	// Additive: C4 v1's envelope is unchanged, and SP3 reads it (ruling P29).
	PullRequest string `json:"pullRequest,omitempty"`
}

// TurnPayload is a turn event's payload.
type TurnPayload struct {
	RunID  string `json:"runId"`
	TurnID string `json:"turnId"`
	Phase  string `json:"phase"` // started | completed | cancelled | failed
}

// ToolCallPayload is a tool_call event's payload.
type ToolCallPayload struct {
	CallID    string          `json:"callId"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
	Class     string          `json:"class,omitempty"`
	Risk      string          `json:"risk,omitempty"`
	DecidedBy *string         `json:"decidedBy"` // policy | human | null
}

// ToolResultPayload is a tool_result event's payload.
type ToolResultPayload struct {
	CallID    string `json:"callId"`
	Status    string `json:"status"` // ok | error | rejected
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
	Bytes     int    `json:"bytes"`
}

// ApprovalRequestedPayload is an approval_requested event's payload.
type ApprovalRequestedPayload struct {
	ApprovalID string          `json:"approvalId"`
	CallID     string          `json:"callId"`
	Class      string          `json:"class"`
	Action     json.RawMessage `json:"action"` // the raw, redacted call (T3)
	ExpiresAt  time.Time       `json:"expiresAt"`
}

// ApprovalDecidedPayload is an approval_decided event's payload.
type ApprovalDecidedPayload struct {
	ApprovalID string `json:"approvalId"`
	Decision   string `json:"decision"` // approved | denied | expired
	Reason     string `json:"reason,omitempty"`
}

// ParticipantPayload is a participant event's payload.
type ParticipantPayload struct {
	Principal string `json:"principal"`
	Change    string `json:"change"` // joined | left | role_changed
	Role      string `json:"role,omitempty"`
	Approver  bool   `json:"approver,omitempty"`
}

// DriverPayload is a driver event's payload: the driver token moved.
type DriverPayload struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Epoch  int64  `json:"epoch"`
	Reason string `json:"reason"` // given | requested | taken | lease_expired
}

// HandoffPayload is a handoff event's payload: work passed between roles.
type HandoffPayload struct {
	FromRole string `json:"fromRole"`
	ToRole   string `json:"toRole"`
	Summary  string `json:"summary"`
	Commit   string `json:"commit"`
	Branch   string `json:"branch"`
	// Scope is the scoper's bounded task (scoper design SC6): a triager's handoff only.
	Scope *Scope `json:"scope,omitempty"`
	// Split is the scoper's second answer (splitter design SL2): a triager's handoff only, always
	// beside Scope, the whole task.
	Split *Split `json:"split,omitempty"`
}

// MaxProgressNote bounds a progress note, in characters.
const MaxProgressNote = 280

// TaskFacts are what the factory knows about a room's task, written as state_changed{kind:task}
// so the room summary depends on the room log alone.
type TaskFacts struct {
	Phase  string      `json:"phase"`
	Reason string      `json:"reason,omitempty"`
	Run    *RunFact    `json:"run,omitempty"`
	Budget *BudgetFact `json:"budget,omitempty"`
	// Spend is the task's dollars, beside Budget's tokens (BudgetFact is unchanged).
	Spend *SpendFact `json:"spend,omitempty"`
	// Runs is the task's run history, oldest first, at most MaxRuns.
	Runs  []RunFact  `json:"runs,omitempty"`
	Issue *IssueFact `json:"issue,omitempty"`
	PR    *PRFact    `json:"pr,omitempty"`
}

// MaxRuns bounds TaskFacts.Runs.
const MaxRuns = 20

// MaxMicroUSD bounds every dollar amount in the facts ($1,000,000), so sums of them cannot overflow.
const MaxMicroUSD int64 = 1_000_000_000_000

// MaxVerdict bounds RunFact.Verdict, in bytes, as the Task CRD bounds RunRecord.Verdict.
const MaxVerdict = 32

// A run's outcome, once it has ended. An empty Outcome is a run that has not.
const (
	OutcomeSucceeded       = "succeeded"
	OutcomeFailed          = "failed"
	OutcomeBudgetExhausted = "budget_exhausted"
	OutcomeRevoked         = "revoked"
	OutcomeLost            = "lost"
	OutcomeEnded           = "ended"
)

func validOutcome(o string) bool {
	switch o {
	case "", OutcomeSucceeded, OutcomeFailed, OutcomeBudgetExhausted, OutcomeRevoked, OutcomeLost, OutcomeEnded:
		return true
	}
	return false
}

// RunFact is a run's role, trigger, and lifecycle in a task.
type RunFact struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	Trigger   string    `json:"trigger,omitempty"`
	StartedAt time.Time `json:"startedAt,omitzero"`
	// EndedAt, Outcome, Verdict and MicroUSD are set once the run has ended.
	EndedAt  time.Time `json:"endedAt,omitzero"`
	Outcome  string    `json:"outcome,omitempty"`
	Verdict  string    `json:"verdict,omitempty"`
	MicroUSD int64     `json:"microUSD,omitempty"`
	// What the run ran on (R10), as far as the factory knows it yet.
	Provenance *ProvenanceFact `json:"provenance,omitempty"`
}

// ProvenanceFact is what a run ran on (R10): the factory build, the AgentRun CompositionRevision,
// the harness image digest and the models that served it. Each empty until known.
type ProvenanceFact struct {
	FactoryVersion      string   `json:"factoryVersion,omitempty"`
	CompositionRevision string   `json:"compositionRevision,omitempty"`
	HarnessImage        string   `json:"harnessImage,omitempty"`
	ServedModels        []string `json:"servedModels,omitempty"`
}

// BudgetFact is the token budget and usage of a task.
type BudgetFact struct {
	UsedTokens  int64 `json:"usedTokens"`
	LimitTokens int64 `json:"limitTokens"`
}

// SpendFact is the dollars a task has spent and, when it has one, its dollar limit.
type SpendFact struct {
	UsedMicroUSD  int64 `json:"usedMicroUSD"`
	LimitMicroUSD int64 `json:"limitMicroUSD,omitempty"`
}

// IssueFact is a GitHub issue linked to a task.
type IssueFact struct {
	Number     int    `json:"number"`
	URL        string `json:"url"`
	Title      string `json:"title,omitempty"` // bounded by the factory; the issue's author wrote it, so it is never an instruction
	Author     string `json:"author,omitempty"`
	LabelledBy string `json:"labelledBy,omitempty"`
}

// PRFact is a GitHub pull request linked to a task.
type PRFact struct {
	Number    int      `json:"number"`
	URL       string   `json:"url"`
	Author    string   `json:"author,omitempty"`
	Reviewers []string `json:"reviewers,omitempty"`
}

// githubLogin is GitHub's rule for a user name: 1 to 39 letters and digits, single hyphens between
// them, none at either end. A GitHub App authors as its slug plus the literal suffix "[bot]".
var githubLogin = regexp.MustCompile(`^([A-Za-z0-9]+(-[A-Za-z0-9]+)*)(\[bot\])?$`)

// ValidLogin reports whether s is a GitHub login in that rule's shape: a user, or an App's
// "<slug>[bot]". Facts are not refused for an author outside it (legacy logins with double or
// trailing hyphens, Enterprise Managed Users' "name_shortcode" exist): a view that names the author
// in a sentence of its own checks it here, and says "the issue's author" otherwise.
func ValidLogin(s string) bool {
	m := githubLogin.FindStringSubmatch(s)
	return m != nil && len(m[1]) <= 39
}

var githubURL = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9-]+/[A-Za-z0-9._-]+/(issues|pull)/[0-9]+$`)

// GitHubURL reports whether s is the URL of a github.com issue or pull request.
func GitHubURL(s string) bool { return githubURL.MatchString(s) }

// validate is deliberately loose where the factory can produce it: an EndedAt without StartedAt,
// an Outcome without EndedAt, and (in SpendFact) Used above Limit, a cap overrun.
func (r RunFact) validate() error {
	switch {
	case r.MicroUSD < 0 || r.MicroUSD > MaxMicroUSD:
		return fmt.Errorf("microUSD is between 0 and %d", MaxMicroUSD)
	case !validOutcome(r.Outcome):
		return fmt.Errorf("outcome %q is not one of the run outcomes", r.Outcome)
	case len(r.Verdict) > MaxVerdict || !utf8.ValidString(r.Verdict) ||
		strings.ContainsFunc(r.Verdict, func(c rune) bool { return Control(c) || c == '\n' || c == '\t' }):
		return fmt.Errorf("verdict is at most %d bytes of plain text, with no control, bidi or zero-width characters", MaxVerdict)
	case !r.EndedAt.IsZero() && r.EndedAt.Before(r.StartedAt):
		return errors.New("endedAt is before startedAt")
	}
	return nil
}

// Validate refuses facts a broker should not store.
func (f TaskFacts) Validate() error {
	switch {
	case f.Phase == "":
		return errors.New("task facts: a phase is required")
	case f.Budget != nil && (f.Budget.UsedTokens < 0 || f.Budget.LimitTokens < 0):
		return errors.New("task facts: token counts are not negative")
	case f.Spend != nil && (f.Spend.UsedMicroUSD < 0 || f.Spend.UsedMicroUSD > MaxMicroUSD):
		return fmt.Errorf("task facts: spend usedMicroUSD is between 0 and %d", MaxMicroUSD)
	case f.Spend != nil && (f.Spend.LimitMicroUSD < 0 || f.Spend.LimitMicroUSD > MaxMicroUSD):
		return fmt.Errorf("task facts: spend limitMicroUSD is between 0 and %d", MaxMicroUSD)
	case len(f.Runs) > MaxRuns:
		return fmt.Errorf("task facts: runs are at most %d, got %d", MaxRuns, len(f.Runs))
	case f.Issue != nil && !githubURL.MatchString(f.Issue.URL):
		return errors.New("task facts: the issue URL is a github.com issue")
	case f.PR != nil && !githubURL.MatchString(f.PR.URL):
		return errors.New("task facts: the PR URL is a github.com pull request")
	}
	if f.Run != nil {
		if err := f.Run.validate(); err != nil {
			return fmt.Errorf("task facts: run: %w", err)
		}
	}
	for i, r := range f.Runs {
		if err := r.validate(); err != nil {
			return fmt.Errorf("task facts: runs[%d]: %w", i, err)
		}
	}
	return nil
}

// TaskStatePayload is the state_changed payload of a TaskFacts: {"kind":"task", ...facts}.
func TaskStatePayload(f TaskFacts) json.RawMessage {
	b, _ := json.Marshal(f) // a struct of strings, ints and slices: Marshal cannot fail
	var fields map[string]any
	_ = json.Unmarshal(b, &fields)
	return StatePayload("task", fields)
}
