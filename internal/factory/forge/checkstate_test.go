// SPDX-License-Identifier: Apache-2.0

package forge

import "testing"

// A cancelled job is its own state, not a failure (#40); a timed-out one still is.
func TestCheckStateTellsCancelledFromFailed(t *testing.T) {
	for conclusion, want := range map[string]string{"SUCCESS": "SUCCESS", "SKIPPED": "SUCCESS", "CANCELLED": "CANCELLED", "FAILURE": "FAILURE", "TIMED_OUT": "FAILURE"} {
		if got := checkState("COMPLETED", conclusion); got != want {
			t.Errorf("%s: %s, want %s", conclusion, got, want)
		}
	}
	if got := checkState("IN_PROGRESS", ""); got != "PENDING" {
		t.Errorf("running: %s", got)
	}
}
