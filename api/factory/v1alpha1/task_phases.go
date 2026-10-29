// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"strconv"
	"strings"
	"time"
)

// Task phases, the §4 state diagram.
const (
	PhaseReceived      = "Received"
	PhaseRejected      = "Rejected"
	PhaseTriaged       = "Triaged"
	PhaseQueued        = "Queued"
	PhaseImplementing  = "Implementing"
	PhaseNoOp          = "NoOp"
	PhaseReviewing     = "Reviewing"
	PhaseAwaitingCI    = "AwaitingCI"
	PhaseAutoMerging   = "AutoMerging"
	PhaseAwaitingHuman = "AwaitingHuman"
	PhaseMerged        = "Merged"
	PhaseVerifying     = "Verifying"
	PhaseDone          = "Done"
	PhaseReverted      = "Reverted"
	PhaseEscalated     = "Escalated"
	PhaseClosed        = "Closed"
	PhaseStopped       = "Stopped"
	// a parent waiting on its children: neither active nor terminal (splitter design SL12)
	PhaseSplit = "Split"
)

// AnnotationStop asks the factory to stop one task (§6.1): "true" set by hand, "label" set by
// the poller from factory/stop, "superseded" when a re-label replaces an escalated task (R4).
const AnnotationStop = "agents.ogenki.io/stop"

// AnnotationRevert records a maintainer's factory/revert on an auto-merged PR, within the window (§6.4).
const AnnotationRevert = "agents.ogenki.io/revert"

// LabelIssue carries the number of the issue a task narrates on.
const LabelIssue = "agents.ogenki.io/issue"

// LabelPublishedIssue carries the number of the issue the factory filed from an investigate task's
// proposal (RunLore agent-to-agent R9): the poller finds the origin of a started issue by it, in
// one List, as it finds a task by LabelIssue.
const LabelPublishedIssue = "agents.ogenki.io/published-issue"

// LabelParent carries a child task's parent's name (splitter design SL14): a family's cost is a
// selector sum, and the parent finds its children by it.
const LabelParent = "agents.ogenki.io/parent"

// childOfPrefix is the GitHub label a child's issue carries, with its parent issue's number (SL8).
const childOfPrefix = "factory/child-of:#"

// ChildOfLabel is the GitHub label on a child's issue naming its parent's (SL8): the ledger a
// rebuild reads, as a follow-up's factory/follow-up-of is.
func ChildOfLabel(parentIssue int) string { return childOfPrefix + strconv.Itoa(parentIssue) }

// ParseChildOf is the parent issue a child-of label names, in its canonical form only.
func ParseChildOf(label string) (int, bool) {
	s, ok := strings.CutPrefix(label, childOfPrefix)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || strconv.Itoa(n) != s {
		return 0, false
	}
	return n, true
}

// TerminalPhase is a task that has ended: nothing moves it again.
func TerminalPhase(p string) bool {
	switch p {
	case PhaseRejected, PhaseNoOp, PhaseDone, PhaseReverted, PhaseClosed, PhaseStopped:
		return true
	}
	return false
}

// ActivePhase is a task in motion: it counts against the active-task cap (§6.2).
func ActivePhase(p string) bool {
	switch p {
	case PhaseImplementing, PhaseReviewing, PhaseAwaitingCI, PhaseAutoMerging, PhaseVerifying:
		return true
	}
	return false
}

// SpendsDailyCap reports whether t counts against the factory's daily task cap (§6.2) on now's UTC day:
// created that day, past its first reconcile (a phase is set) and not rejected, and not an adopted pull
// request (R39), which starts no run. The reconciler's admission and the API's refusal both read it.
func SpendsDailyCap(t *Task, now time.Time) bool {
	return t.Spec.Source.Kind != "adopt" && t.Status.Phase != "" && t.Status.Phase != PhaseRejected &&
		t.CreationTimestamp.UTC().Format(time.DateOnly) == now.UTC().Format(time.DateOnly)
}
