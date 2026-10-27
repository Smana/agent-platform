// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"slices"
	"testing"
)

func TestFiles(t *testing.T) {
	files, err := Files()
	if err != nil {
		t.Fatal(err)
	}
	var names, versions, noTx []string
	for _, f := range files {
		names = append(names, f.Name)
		versions = append(versions, f.Version)
		if f.NoTx {
			noTx = append(noTx, f.Name)
		}
	}
	wantNames := []string{
		"20260927120000_rooms.sql", "20260929120000_verdicts.sql", "20261001120000_driver_queue.sql",
		"20261001130000_delivery_indexes.sql", "20261001140000_brief_index.sql", "20261005120000_approvals.sql",
		"20261005130000_approval_indexes.sql", "20261008120000_task_facts_index.sql", "20261023120000_webhooks.sql",
	}
	if !slices.Equal(names, wantNames) {
		t.Fatalf("files = %q, want %q", names, wantNames)
	}
	if !slices.IsSorted(versions) || versions[0] != "20260927120000" {
		t.Fatalf("versions = %q", versions)
	}
	// The txmode-none files are the ones with CREATE INDEX CONCURRENTLY; a change here changes how
	// they are applied.
	wantNoTx := []string{"20260929120000_verdicts.sql", "20261001130000_delivery_indexes.sql",
		"20261001140000_brief_index.sql", "20261005130000_approval_indexes.sql", "20261008120000_task_facts_index.sql"}
	if !slices.Equal(noTx, wantNoTx) {
		t.Fatalf("txmode none = %q, want %q", noTx, wantNoTx)
	}
}
