// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"cmp"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
)

// The kinds of a charge (spend per identity SI2): a run of a Task, or one started through
// POST /v1/runs, a room's.
const (
	KindTask = "task"
	KindRoom = "room"
)

// Payer is the one ledger key the run's charge is attributed to (spend per identity SI1): the
// person it is charged to beside the fleet, else its own principal. It is the last key the meter
// settles the run to (a ChargedTo run is also charged to system:factory in the ledger).
func (r Run) Payer() string { return cmp.Or(r.ChargedTo, r.Principal) }

// Kind is KindTask for a run of a Task, KindRoom for any other.
func (r Run) Kind() string {
	if r.TaskID != "" {
		return KindTask
	}
	return KindRoom
}

// TaskPayer is the key the task's next run is charged to beside the fleet: an api task's principal
// always (F4, R7), the person while per-person caps are on, else the fleet's own (SI1).
func TaskPayer(t *v1alpha1.Task, perPerson bool) string {
	if p := t.Sponsored(); p != "" && t.Status.RetriedBy == "" {
		return p
	}
	if perPerson {
		if p := t.Payer(); p != "" {
			return p
		}
	}
	return PrincipalFactory
}
