// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "strings"

// Person is the ledger key of a GitHub login (v2 per-person caps D1): GitHub logins are
// case-insensitive, so the key is lowercased, and a label, a retry and a token name one key.
func Person(login string) string { return "github:" + strings.ToLower(login) }

// Sponsored is the api task's principal (F4): the ledger key its runs are charged to beside the
// fleet whatever per-person caps say (R7); "" for any other task.
func (t *Task) Sponsored() string {
	if t.Spec.Source.Kind == "api" {
		return t.Spec.Source.RequestedBy
	}
	return ""
}

// Payer is the person the task's next run is charged to beside the fleet (D2): the maintainer
// whose /factory retry sent it back, else an api task's sponsored principal (R7), else the
// labeller; "" for a task no person or principal started.
func (t *Task) Payer() string {
	if t.Status.RetriedBy != "" {
		return t.Status.RetriedBy
	}
	if p := t.Sponsored(); p != "" {
		return p
	}
	if login, ok := strings.CutPrefix(t.Spec.Source.RequestedBy, "github:"); ok && login != "" {
		return Person(login)
	}
	return ""
}
