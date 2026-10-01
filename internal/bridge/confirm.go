// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/wire"
)

const (
	statusWaiting = "waiting_for_confirmation"
	// defaultApprovalWait bounds the wait for a decision when the broker named
	// no expiry.
	defaultApprovalWait = 30 * time.Minute
	// decisionGrace is how long past an approval's expiry the bridge still
	// waits for the broker's own expired decision before rejecting the step.
	decisionGrace = time.Minute
	// maxHeldDecisions bounds the decisions held for approvals no pending
	// action names yet: after a restart, the stream replays a decision before
	// the bridge has asked for its approval again.
	maxHeldDecisions = 64
	// maxApproverReason caps the approver's reason the agent is shown.
	maxApproverReason = 1 << 10
)

// The texts the agent is shown when its step is rejected. They are fixed and
// never quote the action, which is the model's own and may carry anything.
const (
	rejected = " Every action in this step was rejected and none ran; retry the others on their own."
	// shapes names the forms the classifier allows, so an unattended run can
	// recover (ap5-5.1 concerns 1 and 2).
	shapes = " Allowed shapes: a PR body through `gh pr create --body-file -` with a heredoc, or" +
		" `--body-file <file>`, never `--body \"$(...)\"`; a force-push only as" +
		" `git push --force-with-lease origin <your branch>`; `git --no-pager` instead of pager variables;" +
		" no GIT_* variables, `-c` or config overrides."
	textEgress = "Denied (egress.new): this run's egress does not reach that registry, and no approval can widen it." +
		" Ask in the room for a fork with the egressProfiles it needs." + rejected
	textBrokerDown = "The room could not record the approval request, so this step was rejected and none of it ran." +
		" Retry it later."
	textTimeout  = "No approver decided in time, so this step was rejected and none of it ran. Ask in the room, then retry."
	textInternal = "The bridge failed while checking this step, so it was rejected and none of it ran. Retry it."
	textDeclined = "An approver declined this step, so none of it ran."
)

// denial is the text for a step that policy rejected because of class.
func denial(p wire.ApprovalPolicy, class Class) string {
	profile := "attended"
	if p.Profile == "unattended" {
		profile = "unattended"
	}
	switch class {
	case EgressNew:
		return textEgress
	case ForgePush, ForgePR, ForgeOther:
		return fmt.Sprintf("Denied by the room's %s policy (%s).%s Retry each action on its own, in an allowed shape.%s",
			profile, class, rejected, shapes)
	case MCPWrite:
		return fmt.Sprintf("Denied by the room's %s policy (mcp.write): it refuses MCP tools that write.%s", profile, rejected)
	}
	return fmt.Sprintf("Denied by the room's %s policy.%s", profile, rejected)
}

// declined is the text for a step an approver rejected, with their reason.
func declined(reason string) string {
	if reason == "" {
		return textDeclined
	}
	return textDeclined + " Their reason: " + cut(reason, maxApproverReason)
}

// ApprovalRequester opens an approval in the room (Broker.RequestApproval).
type ApprovalRequester interface {
	RequestApproval(ctx context.Context, r wire.ApprovalRequest) (wire.ApprovalAck, int, error)
}

// pendingAction is an action the harness wrote and has no result for yet. Its
// mutable fields are guarded by Confirmer.mu.
type pendingAction struct {
	callID, tool, risk string
	action             json.RawMessage
	unreadable         bool // an ActionEvent whose fields did not decode: forge.other
	order              int64

	answered   bool // the harness was answered for it; it waits for its result
	logged     bool // its policy_decision is in the log
	approvalID string
	deadline   time.Time
}

// Confirmer answers the harness's confirmations (§6). Under AlwaysConfirm every
// action waits; the bridge allows or denies it locally, or escalates it to the
// room's approvers. OpenHands answers all pending actions at once (spec open
// item), so one deny rejects the whole step and one human class holds its
// siblings.
//
// Approvals are oversight, not a boundary (S9), and the loop fails closed: a
// classifier panic is forge.other, and a broker outage, a missing decision or
// any other failure rejects the step or leaves it waiting, never accepts it.
//
// Its methods are safe for concurrent use: the bridge's loop calls Observe and
// OnStatus, the stream calls Decision.
type Confirmer struct {
	Harness    *Harness
	Broker     ApprovalRequester
	Classifier Classifier
	RunID      string
	Push       func(wire.Item)
	Logger     *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// classifyFn replaces Classifier.Classify in tests.
	classifyFn func(tool string, action json.RawMessage, risk string) Class

	settling sync.Mutex // one answer at a time, the loop's or a decision's

	mu      sync.Mutex
	policy  wire.ApprovalPolicy
	order   int64
	pending map[string]*pendingAction
	decided map[string]wire.Decision // by approval id, until applied
	// stale is set when a decision answered the harness outside the loop: the
	// loop's next status was read before that answer, so it is skipped.
	stale bool
}

func (c *Confirmer) log() *slog.Logger {
	if c.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Logger
}

func (c *Confirmer) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// SetPolicy sets the room's approval profile, handed over at hello.
func (c *Confirmer) SetPolicy(p wire.ApprovalPolicy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.policy = p
}

// Ready switches the conversation to AlwaysConfirm (ruling P5). The bridge
// calls it until it succeeds.
func (c *Confirmer) Ready(ctx context.Context) error { return c.Harness.AlwaysConfirm(ctx) }

// ClassOf is the class a tool_call is logged with: the one the confirmation
// loop decides by.
func (c *Confirmer) ClassOf(tool string, action json.RawMessage, risk string) string {
	return string(c.classify(tool, action, risk))
}

// classify never panics: a classifier bug is forge.other, the class of what it
// cannot read.
func (c *Confirmer) classify(tool string, action json.RawMessage, risk string) (class Class) {
	defer func() {
		if r := recover(); r != nil {
			c.log().Error("the classifier panicked; the action is forge.other", "tool", cut(tool, 64), "panic", fmt.Sprint(r))
			class = ForgeOther
		}
	}()
	if c.classifyFn != nil {
		return c.classifyFn(tool, action, risk)
	}
	return c.Classifier.Classify(tool, action, risk)
}

// Observe tracks which actions still wait for a result. An action it cannot
// read is still tracked, as forge.other: the harness answers every pending
// action at once, so one left out would run unclassified.
func (c *Confirmer) Observe(e RawEvent) {
	var f struct {
		ToolName     string          `json:"tool_name"`
		ToolCallID   string          `json:"tool_call_id"`
		SecurityRisk string          `json:"security_risk"`
		Action       json.RawMessage `json:"action"`
	}
	readable := !e.Malformed && json.Unmarshal(e.Raw, &f) == nil
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == nil {
		c.pending = map[string]*pendingAction{}
	}
	switch e.Kind {
	case "ActionEvent":
		key := f.ToolCallID
		if !readable || key == "" {
			key = "event:" + e.ID
			if e.ID == "" {
				key = fmt.Sprintf("event#%d", c.order+1)
			}
		}
		if _, ok := c.pending[key]; ok {
			return // mapped again after a restart: keep what it has
		}
		c.order++
		c.pending[key] = &pendingAction{callID: key, tool: f.ToolName, risk: f.SecurityRisk, action: f.Action,
			unreadable: !readable, order: c.order}
	case "ObservationEvent", "UserRejectObservation", "AgentErrorEvent":
		if p, ok := c.pending[f.ToolCallID]; ok && readable {
			delete(c.decided, p.approvalID)
			delete(c.pending, f.ToolCallID)
		}
	}
}

// OnStatus answers the step the harness waits on, when the bridge has read
// every event it wrote before status. It runs on the bridge's loop.
func (c *Confirmer) OnStatus(ctx context.Context, status string) {
	c.settling.Lock()
	defer c.settling.Unlock()
	c.mu.Lock()
	stale := c.stale
	c.stale = false
	c.mu.Unlock()
	if stale || status != statusWaiting {
		return
	}
	c.settle(ctx, false)
}

// Decision records an approver's decision from the broker's stream, and
// answers the step if it completes it. A decision no pending action names is
// held, a few at most, for a request re-sent after a restart. It never fails
// the stream: an answer the harness refuses is retried at the next status.
func (c *Confirmer) Decision(ctx context.Context, d wire.Decision) error {
	c.settling.Lock()
	defer c.settling.Unlock()
	c.mu.Lock()
	if c.decided == nil {
		c.decided = map[string]wire.Decision{}
	}
	named := false
	for _, p := range c.pending {
		named = named || (!p.answered && d.ApprovalID != "" && p.approvalID == d.ApprovalID)
	}
	if named || len(c.decided) < maxHeldDecisions {
		c.decided[d.ApprovalID] = d
	}
	c.mu.Unlock()
	if named {
		c.settle(ctx, true)
	}
	return nil
}

func (c *Confirmer) record(kind string, fields map[string]any) {
	c.Push(wire.Item{Stream: wire.StreamStatus, Type: envelope.StateChanged, Payload: envelope.StatePayload(kind, fields)})
}

// step is the actions the harness waits on and the bridge has not answered,
// in the order they were written.
func (c *Confirmer) step() ([]*pendingAction, wire.ApprovalPolicy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*pendingAction
	for _, p := range c.pending {
		if !p.answered {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b *pendingAction) int { return cmp.Compare(a.order, b.order) })
	return out, c.policy
}

// settle answers the step when it can: a deny rejects it at once, all allows
// accept it, and a human class waits until every approval of the step is
// decided, one is declined, or one is past its deadline. Under settling.
func (c *Confirmer) settle(ctx context.Context, fromDecision bool) {
	step, policy := c.step()
	if len(step) == 0 {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			c.log().Error("the confirmation loop panicked; the step is rejected", "panic", fmt.Sprint(r))
			c.answer(ctx, step, false, textInternal, nil, fromDecision)
		}
	}()
	type judged struct {
		p     *pendingAction
		class Class
		v     Verdict
	}
	js := make([]judged, 0, len(step))
	for _, p := range step {
		class := ForgeOther
		if !p.unreadable {
			class = c.classify(p.tool, p.action, p.risk)
		}
		v := Decide(policy, class)
		if v != Allow && v != Deny {
			v = Human // Decide's contract: anything else asks a human
		}
		if v != Human {
			c.logOnce(p, class, v)
		}
		js = append(js, judged{p, class, v})
	}
	// A deny rejects the step before any approver is asked for nothing.
	for _, j := range js {
		if j.v == Deny {
			c.answer(ctx, step, false, denial(policy, j.class), nil, fromDecision)
			return
		}
	}
	var humans []*pendingAction
	for _, j := range js {
		if j.v != Human {
			continue
		}
		if !c.ask(ctx, j.p, j.class) {
			c.answer(ctx, step, false, textBrokerDown, nil, fromDecision)
			return
		}
		humans = append(humans, j.p)
	}
	accept, reason, refs, done := c.decisions(humans)
	if done {
		c.answer(ctx, step, accept, reason, refs, fromDecision)
	}
}

// logOnce records an action's local verdict once, however often its step is
// looked at again.
func (c *Confirmer) logOnce(p *pendingAction, class Class, v Verdict) {
	c.mu.Lock()
	logged := p.logged
	p.logged = true
	c.mu.Unlock()
	if !logged {
		c.record("policy_decision", map[string]any{"callId": p.callID, "class": string(class), "decision": string(v)})
	}
}

// ask opens p's approval once. It reports false when the broker did not open
// it, or ctx ended.
func (c *Confirmer) ask(ctx context.Context, p *pendingAction, class Class) bool {
	c.mu.Lock()
	asked := p.approvalID != ""
	c.mu.Unlock()
	if asked {
		return true
	}
	action := p.action
	if len(action) == 0 {
		action = json.RawMessage(`{}`)
	}
	ack, code, err := c.Broker.RequestApproval(ctx, wire.ApprovalRequest{CallID: p.callID, Class: string(class), Action: action})
	if err != nil || code != http.StatusOK || ack.ApprovalID == "" {
		c.log().Warn("the room did not open an approval; the step is rejected", "callId", p.callID, "code", code, "err", err)
		return false
	}
	deadline := ack.ExpiresAt
	if deadline.IsZero() {
		deadline = c.now().Add(defaultApprovalWait)
	}
	c.mu.Lock()
	p.approvalID, p.deadline = ack.ApprovalID, deadline.Add(decisionGrace)
	c.mu.Unlock()
	return true
}

// decisions reads the step's approvals: done once all are decided, one is
// declined, or one is past its deadline undecided. refs are the decisions
// applied by the answer.
func (c *Confirmer) decisions(humans []*pendingAction) (accept bool, reason string, refs []int64, done bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	accept, done = true, true
	late := false
	for _, p := range humans {
		d, ok := c.decided[p.approvalID]
		if !ok {
			done = false
			late = late || !c.now().Before(p.deadline)
			continue
		}
		refs = append(refs, d.Ref)
		if !d.Allow && accept {
			accept, reason = false, declined(d.Reason)
		}
	}
	switch {
	case !accept:
		return false, reason, refs, true
	case late:
		return false, textTimeout, nil, true
	}
	return accept, "", refs, done
}

// answer responds to the harness for the whole step. On success the step's
// actions are answered and the decisions applied are acknowledged; on failure
// nothing changes, and the next waiting status tries again.
func (c *Confirmer) answer(ctx context.Context, step []*pendingAction, accept bool, reason string, refs []int64, fromDecision bool) {
	if err := c.Harness.Respond(ctx, accept, reason); err != nil {
		c.log().Warn("the harness did not take the answer; retrying at the next status", "accept", accept, "err", err)
		return
	}
	c.mu.Lock()
	for _, p := range step {
		p.answered = true
		delete(c.decided, p.approvalID)
	}
	c.stale = c.stale || fromDecision
	c.mu.Unlock()
	for _, ref := range refs {
		c.record("decision_applied", map[string]any{"ref": ref, "runId": c.RunID})
	}
}
