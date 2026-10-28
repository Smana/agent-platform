// SPDX-License-Identifier: Apache-2.0

package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/Smana/maku/internal/factory/fmetrics"
	"github.com/Smana/maku/internal/metrics"
)

// scraped gives s a Set on a real exporter and returns its scrape.
func scraped(t *testing.T, s *Server) func() string {
	t.Helper()
	exp, err := metrics.NewExporter(metrics.FactoryBuildInfo, "v0.0.0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exp.Shutdown(t.Context()) })
	if s.Metrics, err = fmetrics.New(exp.Meter(), nil, "agent-system", func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	s.Metrics.ListRepositories("Smana/cloud-native-ref")
	return func() string {
		rec := httptest.NewRecorder()
		exp.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
		b, _ := io.ReadAll(rec.Body)
		return string(b)
	}
}

// Spend per identity SI4: the API's refusals name the requester as authenticated, and a refusal on
// a person's day the person whose day it is, the key over.
func TestARefusalNamesItsRequesterOrThePersonOverTheirDay(t *testing.T) {
	people := personLinks{by: map[string]string{"human:291": "alice"}}
	spent := func() *corev1.ConfigMap { return ledgerCM("20260927", map[string]int64{alice: 4_000_000}) }
	s := personServer(t, &store{}, true, true, people, spent())
	scrape := scraped(t, s)
	if w := post(s.Handler(), "alice", base()); w.Code != http.StatusTooManyRequests {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	s.Owned = func(string) bool { return false }
	if w := post(s.Handler(), "alice", base()); w.Code != http.StatusConflict {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	body := scrape()
	for _, want := range []string{
		`agent_factory_admission_refusals_total{principal="github:alice",reason="person_budget_exhausted",repository="Smana/cloud-native-ref"} 1`,
		`agent_factory_admission_refusals_total{principal="human:291",reason="repository_not_owned",repository="Smana/cloud-native-ref"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s\n%s", want, body)
		}
	}
}
