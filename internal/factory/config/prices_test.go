// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// TC1: a price row may say where it was read and when. Both are validated when present and
// change no price.
func TestAPriceRowsSourceAndCheckedAt(t *testing.T) {
	row := func(extra string) string {
		return strings.Replace(good, "  logsURL:",
			"  prices: {agent-default: {input: 1.40, cacheRead: 0.26, output: 4.40"+extra+"}}\n  logsURL:", 1)
	}
	t.Run("both, parsed", func(t *testing.T) {
		cfg, err := Parse([]byte(row(`, source: "https://docs.z.ai/guides/overview/pricing", checkedAt: "2026-10-16"`)))
		if err != nil {
			t.Fatal(err)
		}
		want := Price{Input: 1.40, CacheRead: 0.26, Output: 4.40,
			Source: "https://docs.z.ai/guides/overview/pricing", CheckedAt: "2026-10-16"}
		if got := cfg.Meter.Prices["agent-default"]; got != want {
			t.Fatalf("%+v, want %+v", got, want)
		}
	})
	t.Run("neither, parsed", func(t *testing.T) {
		if _, err := Parse([]byte(row(""))); err != nil {
			t.Fatal(err)
		}
	})
	for name, c := range map[string]struct{ extra, want string }{
		"plain http":     {`, source: "http://docs.z.ai/pricing"`, `meter.prices.agent-default: source "http://docs.z.ai/pricing" is not an https:// URL`},
		"no host":        {`, source: "https:///pricing"`, `source "https:///pricing" is not an https:// URL`},
		"credentials":    {`, source: "https://u:p@docs.z.ai/pricing"`, `is not an https:// URL`},
		"not a URL":      {`, source: "docs.z.ai pricing"`, `is not an https:// URL`},
		"no such month":  {`, checkedAt: "2026-13-01"`, `meter.prices.agent-default: checkedAt "2026-13-01" is not a YYYY-MM-DD date`},
		"another layout": {`, checkedAt: "16/10/2026"`, `checkedAt "16/10/2026" is not a YYYY-MM-DD date`},
		"a timestamp":    {`, checkedAt: "2026-10-16T00:00:00Z"`, `is not a YYYY-MM-DD date`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(row(c.extra)))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want it to name %q", err, c.want)
			}
		})
	}
}
