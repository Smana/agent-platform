// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/Smana/agent-platform/internal/factory/config"
)

// TC5: each token day a file still writes is named at startup, with the key that replaced it.
func TestEachIgnoredConfigKeyIsWarnedAbout(t *testing.T) {
	var buf bytes.Buffer
	warnIgnored(slog.New(slog.NewTextHandler(&buf, nil)), &config.Config{Ignored: []config.Ignored{
		{Key: "budgets.factoryDaily", Use: "budgets.factoryDailyUSD"},
		{Key: "budgets.perPerson.dailyTokens", Use: "budgets.perPerson.dailyUSD"},
	}})
	out := buf.String()
	if strings.Count(out, "level=WARN") != 2 ||
		!strings.Contains(out, "key=budgets.factoryDaily use=budgets.factoryDailyUSD") ||
		!strings.Contains(out, "key=budgets.perPerson.dailyTokens use=budgets.perPerson.dailyUSD") {
		t.Fatalf("want one warning per key, naming its replacement:\n%s", out)
	}
	buf.Reset()
	warnIgnored(slog.New(slog.NewTextHandler(&buf, nil)), &config.Config{})
	if buf.Len() != 0 {
		t.Fatalf("a file without them warns nothing: %s", buf.String())
	}
}

// TO1, TO5: each sentence the config loaded with is one warning at start.
func TestEachConfigWarningIsLogged(t *testing.T) {
	var buf bytes.Buffer
	warnConfig(slog.New(slog.NewTextHandler(&buf, nil)), &config.Config{Warnings: []string{
		"template trio: tester is the test writer", "class docs-links of o/r is live without holdoutChecks",
	}}, nil)
	out := buf.String()
	if strings.Count(out, "level=WARN") != 2 || !strings.Contains(out, "template trio: tester is the test writer") ||
		!strings.Contains(out, "class docs-links of o/r is live without holdoutChecks") {
		t.Fatalf("want one warning per sentence:\n%s", out)
	}
}
