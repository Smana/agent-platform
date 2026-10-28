// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/fmetrics"
	"github.com/Smana/agent-platform/internal/metrics"
)

// Offline proof 4, startup: the deployed config lists the tester in `trio` and two live classes without
// holdoutChecks. It loads without the tester, the warnings are logged and the gauge reads 1.
func TestStartupWithATesterTemplateAndALiveClassWithoutHoldouts(t *testing.T) {
	raw := strings.Replace(factoryCfg, "classes: {docs-links: {shadow: true}, revert: {shadow: true}",
		"classes: {docs-links: {live: true}, revert: {live: true}", 1)
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if roles := cfg.Templates["trio"].Roles; len(roles) != 2 || roles[0] != "implementer" || roles[1] != "reviewer" {
		t.Fatalf("trio still lists the tester: %v", roles)
	}
	exp, err := metrics.NewExporter(metrics.FactoryBuildInfo, "v0.0.0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exp.Shutdown(t.Context()) })
	m, err := fmetrics.New(exp.Meter(), nil, "agent-system", func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	m.ListRepositories("Smana/cloud-native-ref")
	var logs bytes.Buffer
	warnConfig(slog.New(slog.NewTextHandler(&logs, nil)), cfg, m)
	for _, want := range []string{"template trio: tester is the test writer", "class docs-links of Smana/cloud-native-ref is live without holdoutChecks",
		"class revert of Smana/cloud-native-ref is live without holdoutChecks"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("no warning %q:\n%s", want, logs.String())
		}
	}
	rec := httptest.NewRecorder()
	exp.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	for _, class := range []string{"docs-links", "revert"} {
		if want := `agent_factory_class_without_holdout{class="` + class + `",repository="Smana/cloud-native-ref"} 1`; !strings.Contains(string(body), want) {
			t.Errorf("missing %s", want)
		}
	}
}
