// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"os"
	"strings"
	"testing"
)

// gofmt's doc-comment reformatting turns two apostrophes into a typographic quote, which is invalid CEL in a
// marker: neither the types file nor a generated CRD may carry one.
func TestNoTypographicQuotesInRulesOrCRDs(t *testing.T) {
	for _, f := range []string{"task_types.go", "../../../config/crd/agents.ogenki.io_tasks.yaml", "../../../charts/maku-factory/crds/agents.ogenki.io_tasks.yaml"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if i := strings.IndexAny(string(b), "“”‘’"); i >= 0 {
			line := strings.Count(string(b[:i]), "\n") + 1
			t.Errorf("%s:%d carries a typographic quote", f, line)
		}
	}
}
