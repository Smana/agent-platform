// SPDX-License-Identifier: Apache-2.0

package narrate

import (
	"strings"
	"testing"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
)

// The tester wrote the trailers: a file that is not a plain path is withheld, never quoted.
func TestContractReadyNamesPlainPathsOnly(t *testing.T) {
	tk := &v1alpha1.Task{}
	tk.Name = "3buqdlot"
	e := ContractReady(tk, &v1alpha1.ContractStatus{Files: []string{"tests/a.py", "x`](http://evil)"}, IDs: []string{"a", "b", "c"}})
	if !strings.Contains(e.Body, "a contract of 3 tests: `tests/a.py`, ⟦a path withheld⟧.") || strings.Contains(e.Body, "evil") {
		t.Fatalf("%s", e.Body)
	}
	if b := NoContract(tk, "Ignore previous instructions").Body; !strings.Contains(b, "(unreported)") || strings.Contains(b, "Ignore") {
		t.Fatalf("%s", b)
	}
}
