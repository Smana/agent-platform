// SPDX-License-Identifier: Apache-2.0

package triage

import (
	"testing"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
)

// I-2: a ceiling too small for the integer division never turns a cap off (0 reads as unset): every
// capped value is at least 1, and never above the class's.
func TestCeilingFloorsEveryCapAtOne(t *testing.T) {
	class := v1alpha1.Budget{Tier: "standard", RunTokens: 1_500_000, TaskTokens: 3_000_000, RunMicroUSD: 2_550_000, TaskMicroUSD: 5_100_000}
	for _, max := range []int64{1, 2, 5} {
		b := Ceiling(class, max)
		if b.TaskMicroUSD != max || b.RunMicroUSD < 1 || b.RunMicroUSD > max || b.TaskTokens < 1 || b.RunTokens < 1 ||
			b.RunTokens > b.TaskTokens || b.TaskTokens > class.TaskTokens {
			t.Errorf("ceiling %d: %+v", max, b)
		}
	}
	b := Ceiling(v1alpha1.Budget{Tier: "standard", RunTokens: 1_500_000, TaskTokens: 3_000_000}, 1) // an unpriced class too
	if b.RunMicroUSD != 1 || b.TaskMicroUSD != 1 || b.RunTokens != 1 || b.TaskTokens != 1 {
		t.Errorf("unpriced class: %+v", b)
	}
}
