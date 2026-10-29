// SPDX-License-Identifier: Apache-2.0

package fmetrics

import (
	"context"
	"strings"
	"testing"
)

// SI9: the sponsor of each principal from the config, each admitted ceiling, each intake outcome.
func TestSponsoredSeries(t *testing.T) {
	s, scrape := rig(t, nil, func() bool { return true })
	s.Sponsors(map[string]string{"system:slack-app": "github:smana"})
	ctx := WithPrincipal(context.Background(), "system:slack-app")
	s.MaxCost(ctx, 2_000_000)
	s.APIIntake(ctx, "admitted")
	s.APIIntake(ctx, "not_linked")
	s.APIIntake(ctx, "a code nobody wrote")
	s.APIIntake(WithPrincipal(ctx, "system:<script>"), "admitted")
	out := scrape()
	for _, want := range []string{
		`maku_factory_principal_sponsor_info{principal="system:slack-app",sponsor="github:smana"} 1` + "\n",
		`maku_factory_max_cost_usd_total{principal="system:slack-app"} 2` + "\n",
		`maku_factory_api_intake_total{outcome="admitted",principal="system:slack-app"} 1` + "\n",
		`maku_factory_api_intake_total{outcome="not_linked",principal="system:slack-app"} 1` + "\n",
		`maku_factory_api_intake_total{outcome="other",principal="system:slack-app"} 1` + "\n",
		`maku_factory_api_intake_total{outcome="admitted",principal="other"} 1` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
}
