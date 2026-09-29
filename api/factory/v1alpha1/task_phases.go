// SPDX-License-Identifier: Apache-2.0

package v1alpha1

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
)

// AnnotationStop asks the factory to stop one task (§6.1): "true" set by hand, "label" set by
// the poller from factory/stop, "superseded" when a re-label replaces an escalated task (R4).
const AnnotationStop = "agents.ogenki.io/stop"

// LabelIssue carries the number of the issue a task narrates on.
const LabelIssue = "agents.ogenki.io/issue"

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
