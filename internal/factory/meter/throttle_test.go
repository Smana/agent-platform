// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"testing"

	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/runs"
)

// The gateway charges B1 a priced call at the meter's own prices, in reference tokens
// (ai-native-ref#32), and an unpriced one its raw tokens. A 429 is the run's own bucket when the
// run's spend in that unit reaches B1, whatever its raw token count; else it is the fleet's.
func TestA429IsJudgedInTheUnitB1Charges(t *testing.T) {
	for name, tc := range map[string]struct {
		u           Usage
		reason, lim string
	}{
		// 6.01 M raw tokens, 98 % cache reads: $1.718, 1.01 M reference tokens. Its B1 bucket is a
		// fifth full, so the 429 is B2's.
		"cache-heavy past 5M raw tokens": {Usage{Input: 6_000_000, CacheRead: 5_900_000, Output: 10_000},
			ReasonBudgetFleet, LimitFleet},
		// 3.5 M raw tokens, none cached: $9.40, 5.53 M reference tokens, past B1's 5 M.
		"dear under 5M raw tokens": {Usage{Input: 2_000_000, Output: 1_500_000},
			ReasonBudgetRun, LimitGatewayCeiling},
	} {
		t.Run(name, func(t *testing.T) {
			st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{standardRun("aaaaaaaa")}}
			var revoked []revocation
			var fell []string
			m := usdMeter(st, typed{"aaaaaaaa": typedUsage(tc.u)}, &revoked, &fell)
			m.RunUSD = map[string]float64{"standard": 100} // the run cap stays out of the way
			m.Throttle, m.B1Ceiling = throttled{"aaaaaaaa": true}, config.RunTokenCeiling
			if err := m.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := st.patches["aaaaaaaa"][runs.AnnRevoked]; got != tc.reason {
				t.Errorf("revoked %q, want %q", got, tc.reason)
			}
			if len(revoked) != 1 || revoked[0] != (revocation{tc.reason, tc.lim}) {
				t.Errorf("revocations %v, want one %s/%s", revoked, tc.reason, tc.lim)
			}
		})
	}
}
