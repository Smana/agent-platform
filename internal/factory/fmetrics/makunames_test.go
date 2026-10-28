// SPDX-License-Identifier: Apache-2.0

package fmetrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/maku/internal/metrics"
)

// v0.30.0 serves every agent_factory_ series twice, the second as maku_factory_ (the rebrand):
// the factory's dashboards switch names while both carry the same values.
func TestMakuNamesMirrorEveryFactorySeries(t *testing.T) {
	ctx := t.Context()
	exp, err := metrics.NewExporter(metrics.FactoryBuildInfo, "v0.30.0", metrics.WithMakuNames(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exp.Shutdown(context.Background()) })
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(tk("a", "Running")).Build()
	s, err := New(exp.Meter(), c, "agent-system", func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	s.PROpened(ctx)
	s.PROpened(ctx)
	s.TimeToPR(ctx, 90*time.Second, "issue", "standard", "default")

	rec := httptest.NewRecorder()
	exp.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/metrics", nil))
	b, _ := io.ReadAll(rec.Body)
	body := string(b)
	for _, want := range []string{
		`agent_factory_prs_opened_total{principal="other",repository="other"} 2`,
		`maku_factory_prs_opened_total{principal="other",repository="other"} 2`,
		`maku_factory_build_info{version="v0.30.0"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape lacks %q", want)
		}
	}
	got := map[string]string{}
	for _, l := range strings.Split(body, "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			i := strings.LastIndexByte(l, ' ')
			got[l[:i]] = l[i+1:]
		}
	}
	n := 0
	for k, v := range got {
		rest, ok := strings.CutPrefix(k, "agent_factory_")
		if !ok {
			continue
		}
		n++
		if m := got["maku_factory_"+rest]; m != v {
			t.Errorf("%s = %s, maku_factory_%s = %q", k, v, rest, m)
		}
	}
	if n < 3 {
		t.Fatalf("scraped %d agent_factory_ series, want at least 3", n)
	}
}
