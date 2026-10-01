// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
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
	statusPaused  = "paused"
	statusIdle    = "idle"
	// attendedWait bounds the wait for a decision when the broker named no
	// expiry in an attended room (§6: 30 min); an unattended room waits its
	// policy's ttl, defaultTTL when that is unreadable.
	attendedWait = 30 * time.Minute
	defaultTTL   = 4 * time.Hour
	// decisionGrace is how long past an approval's expiry the bridge still
	// waits for the broker's own expired decision before rejecting the step.
	decisionGrace = time.Minute
	// askBudget bounds all the approval requests of one step together, so a
	// slow broker cannot hold the bridge's loop for 15 s per action (review M3).
	askBudget = 10 * time.Second
	// maxHeldDecisions bounds the decisions held for approvals no pending
	// action names yet: after a restart, the stream replays a decision before
	// the bridge has asked for its approval again.
	maxHeldDecisions = 64
	// maxSpent bounds the approval ids remembered as spent (review I2).
	maxSpent = 4096
	// maxApproverReason caps the approver's reason the agent is shown.
	maxApproverReason = 1 << 10
	// reasonExpired is the reason the broker puts on an expiry (5.3 contract).
	reasonExpired = "expired"
)

// The texts the agent is shown when its step is rejected. They are fixed and
// never quote the action, which is the model's own and may carry anything. The
// harness hands them to the model in the rejection's observation, and the
// bridge resumes the conversation so the model reads them (review I1).
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
	textUnreadable = "The bridge could not read an action of this step, so it was rejected and none of it ran." +
		" Retry each action on its own, as a plain tool call."
	textBrokerDown = "The room could not record the approval request, so this step was rejected and none of it ran." +
		" Retry it later."
	textTimeout    = "No approver decided in time, so this step was rejected and none of it ran. Ask in the room, then retry."
	textInternal   = "The bridge failed while checking this step, so it was rejected and none of it ran. Retry it."
	textDeclined   = "An approver declined this step, so none of it ran."
	textSuperseded = "A room message arrived while this step waited, so it was rejected and none of it ran." +
		" Read the message, then retry the step if it is still needed."
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

// approvalWait is how long an approval waits when the broker named no expiry.
func approvalWait(p wire.ApprovalPolicy) time.Duration {
	if p.Profile != "unattended" {
		return attendedWait
	}
	if d, err := time.ParseDuration(p.TTL); err == nil && d > 0 {
		return d
	}
	return defaultTTL
}

// ApprovalRequester opens an approval in the room (Broker.RequestApproval).
type ApprovalRequester interface {
	RequestApproval(ctx context.Context, r wire.ApprovalRequest) (wire.ApprovalAck, int, error)
}

// pendingAction is an action the harness wrote and has no result for yet,
// keyed by its event id: tool call ids come from the model provider and may
// repeat (review I2). Its mutable fields are guarded by Confirmer.mu.
type pendingAction struct {
	key, callID, tool, risk string
	action                  json.RawMessage
	unreadable              bool // an ActionEvent whose fields did not decode: denied
	order                   int64

	answered   bool // the harness was answered for it; it waits for its result
	approvalID string
}

// judged is an action of a step and what policy made of it.
type judged struct {
	p     *pendingAction
	class Class
	v     Verdict
}

// Confirmer answers the harness's confirmations (§6). Under AlwaysConfirm every
// action waits; the bridge allows or denies it locally, or escalates it to the
// room's approvers. OpenHands answers all pending actions at once (spec open
// item), so one deny rejects the whole step and one human class holds its
// siblings. A message sent with run:true confirms every pending action
// implicitly, so steering goes through Gate (review C1).
//
// Approvals are oversight, not a boundary (S9), and the loop fails closed: a
// classifier panic is forge.other, an unreadable action is denied, and a broker
// outage, a missing decision or any other failure rejects the step or leaves it
// waiting, never accepts it.
//
// Its methods are safe for concurrent use: the bridge's loop calls Observe and
// OnStatus, the stream calls Decision and, through steering, Gate.
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

	settling sync.Mutex // one answer at a time: the loop's, a decision's or steering's

	mu      sync.Mutex
	policy  wire.ApprovalPolicy
	order   int64
	pending map[string]*pendingAction
	decided map[string]wire.Decision // by approval id, until applied
	// spent are the approval ids of answered steps, oldest first: a decision
	// for one is never applied again, nor an ack naming one (review I2).
	spent     map[string]bool
	spentFIFO []string
	// deadline is when the current step's approvals are given up on.
	deadline time.Time
	// stale is set when a decision answered the harness outside the loop: the
	// loop's next status was read before that answer, so it is skipped.
	stale bool
	// resume is set while a rejected conversation still has to be resumed.
	resume bool
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
// calls it until it succeeds, before and whatever the broker answers.
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
// read is still tracked, and denied: the harness answers every pending action
// at once, so one left out would run unclassified.
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
		key := e.ID
		if key == "" {
			key = fmt.Sprintf("#%d", c.order+1)
		}
		if _, ok := c.pending[key]; ok {
			return // mapped again after a restart: keep what it has
		}
		callID := f.ToolCallID
		if !readable || callID == "" {
			callID = "event:" + key
		}
		c.order++
		c.pending[key] = &pendingAction{key: key, callID: callID, tool: f.ToolName, risk: f.SecurityRisk,
			action: f.Action, unreadable: !readable, order: c.order}
	case "ObservationEvent", "UserRejectObservation", "AgentErrorEvent":
		if !readable || f.ToolCallID == "" {
			return
		}
		// A result settles the oldest action with its call id.
		var oldest *pendingAction
		for _, p := range c.pending {
			if p.callID == f.ToolCallID && (oldest == nil || p.order < oldest.order) {
				oldest = p
			}
		}
		if oldest != nil {
			delete(c.decided, oldest.approvalID)
			delete(c.pending, oldest.key)
		}
	}
}

// OnStatus answers the step the harness waits on, when the bridge has read
// every event it wrote before status. It runs on the bridge's loop.
func (c *Confirmer) OnStatus(ctx context.Context, status string) {
	c.settling.Lock()
	defer c.settling.Unlock()
	c.mu.Lock()
	stale, resume := c.stale, c.resume
	c.stale = false
	if !stale && resume && status != statusIdle {
		// Something else resumed the run, or a new step waits: a /run now
		// would confirm that step (re-review I1).
		c.resume, resume = false, false
	}
	c.mu.Unlock()
	if stale {
		return // read before the answer: neither waiting nor idle means anything yet
	}
	if resume {
		c.resumeRun(ctx)
	}
	if status != statusWaiting {
		return
	}
	c.settle(ctx, false)
}

// Decision records an approver's decision from the broker's stream, and
// answers the step if it completes it. A decision no pending action names is
// held, a few at most, for a request re-sent after a restart; one for an
// answered step is dropped. It never fails the stream: an answer the harness
// refuses is retried at the next status.
func (c *Confirmer) Decision(ctx context.Context, d wire.Decision) error {
	if d.ApprovalID == "" {
		return nil // names no approval: never held, never acknowledged
	}
	c.settling.Lock()
	defer c.settling.Unlock()
	c.mu.Lock()
	if c.decided == nil {
		c.decided = map[string]wire.Decision{}
	}
	named := false
	for _, p := range c.pending {
		named = named || (!p.answered && p.approvalID == d.ApprovalID)
	}
	if !c.spent[d.ApprovalID] && (named || len(c.decided) < maxHeldDecisions) {
		c.decided[d.ApprovalID] = d
	}
	c.mu.Unlock()
	if named {
		c.settle(ctx, true)
	}
	return nil
}

// Gate delivers a steering message without ever confirming a step (review C1,
// re-reviews C1 and SAW). A message sent with run:true, or a /run, on a
// conversation waiting for a confirmation confirms every pending action; and
// the bridge's own view lags the harness. So the message goes in with run
// false first, then the harness is asked:
//
//   - waiting or paused: reject every pending action, read by the bridge or
//     not, then /run;
//   - anything else: /run alone. A step running when the message arrived turns
//     its park into a rejection itself, so no step parks between the status
//     read and the /run.
//
// A send that fails returns its error and the delivery is replayed. Once the
// message is in, a failed status read or rejection only skips the /run: the
// loop answers the step as usual, and the resume retry runs the conversation
// from idle.
func (c *Confirmer) Gate(ctx context.Context, send func(run bool) error) error {
	c.settling.Lock()
	defer c.settling.Unlock()
	if err := send(false); err != nil {
		return err
	}
	status, err := c.Harness.Status(ctx)
	if err != nil {
		c.log().Warn("a steering message is in, but the harness status is unreadable; the loop runs it", "err", err)
		return nil
	}
	if status == statusWaiting || status == statusPaused {
		step, _ := c.step()
		_, refs := c.decisions(step)
		if err := c.respond(ctx, step, false, textSuperseded, refs, true); err != nil {
			c.log().Warn("a steering message is in, but the waiting step was not rejected; the loop answers it", "err", err)
			return nil
		}
	}
	c.resumeRun(ctx)
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
// decided, one is declined, or the step's deadline passes. Under settling.
func (c *Confirmer) settle(ctx context.Context, fromDecision bool) {
	step, policy := c.step()
	if len(step) == 0 {
		c.mu.Lock()
		c.deadline = time.Time{}
		c.mu.Unlock()
		return
	}
	defer func() {
		if r := recover(); r != nil {
			c.log().Error("the confirmation loop panicked; the step is rejected", "panic", fmt.Sprint(r))
			if s, _ := c.step(); len(s) > 0 {
				_ = c.answer(ctx, s, false, textInternal, nil, fromDecision)
			}
		}
	}()
	js := make([]judged, 0, len(step))
	for _, p := range step {
		if p.unreadable {
			js = append(js, judged{p, ForgeOther, Deny})
			continue
		}
		class := c.classify(p.tool, p.action, p.risk)
		v := Decide(policy, class)
		if v != Allow && v != Deny {
			v = Human // Decide's contract: anything else asks a human
		}
		js = append(js, judged{p, class, v})
	}
	// A deny rejects the step before any approver is asked for nothing.
	for _, j := range js {
		if j.v != Deny {
			continue
		}
		text := denial(policy, j.class)
		if j.p.unreadable {
			text = textUnreadable
		}
		if c.answer(ctx, step, false, text, nil, fromDecision) == nil {
			c.logVerdicts(js, Deny)
		}
		return
	}
	var humans []judged
	var asked []*pendingAction
	for _, j := range js {
		if j.v == Human {
			humans, asked = append(humans, j), append(asked, j.p)
		}
	}
	if !c.askAll(ctx, humans, policy) {
		_ = c.answer(ctx, step, false, textBrokerDown, nil, fromDecision)
		return
	}
	outcome, refs := c.decisions(asked)
	switch outcome {
	case "":
		return // still waiting
	case "accept":
		if c.answer(ctx, step, true, "", refs, fromDecision) == nil {
			c.logVerdicts(js, Allow)
		}
	default:
		_ = c.answer(ctx, step, false, outcome, refs, fromDecision)
	}
}

// logVerdicts records the local verdicts v of a step the harness took: allows
// only when it was accepted, denies when one rejected it (review M4).
func (c *Confirmer) logVerdicts(js []judged, v Verdict) {
	for _, j := range js {
		if j.v == v {
			c.record("policy_decision", map[string]any{"callId": j.p.callID, "class": string(j.class), "decision": string(v)})
		}
	}
}

// askAll opens the approval of each human action not asked yet, all within
// one askBudget. It reports false when the broker did not open one, named a
// spent approval, or ctx ended.
func (c *Confirmer) askAll(ctx context.Context, humans []judged, policy wire.ApprovalPolicy) bool {
	ctx, cancel := context.WithTimeout(ctx, askBudget)
	defer cancel()
	for _, j := range humans {
		if !c.ask(ctx, j.p, j.class, policy) {
			return false
		}
	}
	return true
}

func (c *Confirmer) ask(ctx context.Context, p *pendingAction, class Class, policy wire.ApprovalPolicy) bool {
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
	ack, code, err := c.Broker.RequestApproval(ctx, wire.ApprovalRequest{EventID: p.key, CallID: p.callID,
		Class: string(class), Action: action})
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil || code != http.StatusOK || ack.ApprovalID == "" || c.spent[ack.ApprovalID] {
		c.log().Warn("the room did not open an approval; the step is rejected", "eventId", p.key, "code", code, "err", err)
		return false
	}
	expires := ack.ExpiresAt
	if expires.IsZero() {
		expires = c.now().Add(approvalWait(policy))
	}
	p.approvalID = ack.ApprovalID
	if d := expires.Add(decisionGrace); c.deadline.IsZero() || d.Before(c.deadline) {
		c.deadline = d
	}
	return true
}

// decisions reads the step's approvals: "accept" once all allow, a rejection
// text once one is declined or expired, or the step's deadline passed with one
// undecided, and "" while it waits. refs are the decisions received, which
// the answer acknowledges whatever it is (review M6).
func (c *Confirmer) decisions(humans []*pendingAction) (outcome string, refs []int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	undecided := false
	for _, p := range humans {
		d, ok := c.decided[p.approvalID]
		if !ok {
			undecided = true
			continue
		}
		refs = append(refs, d.Ref)
		if !d.Allow && outcome == "" {
			outcome = declined(d.Reason)
			if d.Reason == reasonExpired {
				outcome = textTimeout
			}
		}
	}
	switch {
	case outcome != "":
		return outcome, refs
	case !undecided:
		return "accept", refs
	case !c.now().Before(c.deadline):
		return textTimeout, refs
	}
	return "", refs
}

// answer responds to the harness for the whole step, and resumes a rejected
// conversation, which OpenHands leaves idle (review I1). Under settling.
func (c *Confirmer) answer(ctx context.Context, step []*pendingAction, accept bool, reason string, refs []int64,
	fromDecision bool,
) error {
	if err := c.respond(ctx, step, accept, reason, refs, fromDecision); err != nil {
		return err
	}
	if !accept {
		c.resumeRun(ctx)
	}
	return nil
}

// respond answers the step. On success its actions are answered, its
// approvals spent and the decisions received acknowledged; on failure nothing
// changes, and the next waiting status tries again.
func (c *Confirmer) respond(ctx context.Context, step []*pendingAction, accept bool, reason string, refs []int64,
	fromDecision bool,
) error {
	if err := c.Harness.Respond(ctx, accept, reason); err != nil {
		c.log().Warn("the harness did not take the answer; retrying at the next status", "accept", accept, "err", err)
		return err
	}
	c.mu.Lock()
	for _, p := range step {
		p.answered = true
		c.spend(p.approvalID)
	}
	c.deadline = time.Time{}
	c.stale = c.stale || fromDecision
	c.mu.Unlock()
	for _, ref := range refs {
		c.record("decision_applied", map[string]any{"ref": ref, "runId": c.RunID})
	}
	return nil
}

// spend remembers an answered step's approval id. Under mu.
func (c *Confirmer) spend(id string) {
	delete(c.decided, id)
	if id == "" || c.spent[id] {
		return
	}
	if c.spent == nil {
		c.spent = map[string]bool{}
	}
	if len(c.spentFIFO) >= maxSpent {
		delete(c.spent, c.spentFIFO[0])
		c.spentFIFO = c.spentFIFO[1:]
	}
	c.spent[id] = true
	c.spentFIFO = append(c.spentFIFO, id)
}

// resumeRun lets a rejected conversation take its next step, in which the model
// reads the rejection. A run already in progress (409) is resumed already. It
// runs right after the harness took a rejection, or again only from a fresh
// idle status: run() on a waiting conversation confirms its step. Under settling.
func (c *Confirmer) resumeRun(ctx context.Context) {
	err := c.Harness.Run(ctx)
	if se, ok := errors.AsType[*StatusError](err); ok && se.Code == http.StatusConflict {
		err = nil
	}
	c.mu.Lock()
	c.resume = err != nil
	c.mu.Unlock()
	if err != nil {
		c.log().Warn("the rejected conversation did not resume; retrying at the next status", "err", err)
	}
}
