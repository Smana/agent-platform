// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"slices"
	"testing"

	"github.com/Smana/agent-platform/internal/factory/runs"
)

// R10: the served models are the model labels with tokens in a typed reading; a reading without
// that label, or an untyped one, names none.
func TestServedModelsAreTheReadingsModelLabels(t *testing.T) {
	for name, c := range map[string]struct {
		u    Usage
		want []string
	}{
		"two models":     {byModel(map[string]ModelUsage{"glm-5.3": {Input: 10, Output: 1}, "claude-opus-5-5": {Input: 5, Output: 1}}), []string{"claude-opus-5-5", "glm-5.3"}},
		"a zero series":  {byModel(map[string]ModelUsage{"glm-5.3": {Input: 10, Output: 1}, "glm-5.3-flash": {}}), []string{"glm-5.3"}},
		"no model label": {typedUsage(Usage{Input: 10, Output: 1}), nil},
		"untyped":        {Usage{Tokens: 11}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{standardRun("aaaaaaaa")}}
			var revoked []revocation
			var fell []string
			m := usdMeter(st, typed{"aaaaaaaa": c.u}, &revoked, &fell)
			m.Prices, m.Costs = served, &Costs{}
			if err := m.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got, _ := m.Costs.Get("aaaaaaaa"); !slices.Equal(got.Served, c.want) {
				t.Errorf("served %v, want %v", got.Served, c.want)
			}
		})
	}
}
