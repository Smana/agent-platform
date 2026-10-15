// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/httpx"
)

func vmServer(t *testing.T, code int, body string) *VM {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" || r.URL.Query().Get("query") != "Q" {
			t.Errorf("%s %q", r.URL.Path, r.URL.RawQuery)
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	v, err := NewVM(srv.URL, "Q", httpx.New(5*time.Second, nil))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVMParsesTheVector(t *testing.T) {
	v := vmServer(t, http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"ar_agent":"system:serviceaccount:agents:xplane-run-7f3cq2xz"},"value":[1727430000,"12345"]},
		{"metric":{"ar_agent":"system:serviceaccount:agents:xplane-run-aaaaaaaa"},"value":[1727430000,"1.5e3"]},
		{"metric":{"ar_agent":"system:serviceaccount:agents:agent-probe"},"value":[1727430000,"99"]},
		{"metric":{"ar_agent":"system:serviceaccount:other:xplane-run-bbbbbbbb"},"value":[1727430000,"99"]},
		{"metric":{"ar_agent":"system:serviceaccount:agents:xplane-run-cccccccc"},"value":[1727430000,"NaN"]},
		{"metric":{"ar_agent":"system:serviceaccount:agents:xplane-run-dddddddd"},"value":[1727430000,"-5"]},
		{"metric":{"ar_agent":"system:serviceaccount:agents:xplane-run-eeeeeeee"},"value":[1727430000,"+Inf"]},
		{"metric":{"ar_agent":"system:serviceaccount:agents:xplane-run-ffffffff"},"value":[1727430000,"1e30"]},
		{"metric":{"ar_agent":"system:serviceaccount:agents:xplane-run-gggggggg"},"value":[1727430000,42]},
		{"metric":{"ar_agent":"system:serviceaccount:agents:xplane-run-hhhhhhhhh"},"value":[1727430000,"7"]}]}}`)
	got, err := v.RunUsage(t.Context())
	if err != nil || len(got) != 2 || got["7f3cq2xz"] != (Usage{Tokens: 12345}) || got["aaaaaaaa"] != (Usage{Tokens: 1500}) {
		t.Fatalf("only runs' service accounts, with a finite, non-negative count that fits: %v %v", got, err)
	}
}

// A query grouped by gen_ai_token_type reads each type; the token caps still count input + output.
func TestVMReadsTokenTypes(t *testing.T) {
	const run = `"ar_agent":"system:serviceaccount:agents:xplane-run-7f3cq2xz"`
	v := vmServer(t, http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{`+run+`,"gen_ai_token_type":"input"},"value":[1727430000,"1631496"]},
		{"metric":{`+run+`,"gen_ai_token_type":"cached_input"},"value":[1727430000,"1552384"]},
		{"metric":{`+run+`,"gen_ai_token_type":"cache_creation_input"},"value":[1727430000,"7"]},
		{"metric":{`+run+`,"gen_ai_token_type":"output"},"value":[1727430000,"30304"]},
		{"metric":{`+run+`,"gen_ai_token_type":"reasoning"},"value":[1727430000,"999"]}]}}`)
	got, err := v.RunUsage(t.Context())
	want := Usage{Tokens: 1_661_800, Input: 1_631_496, CacheRead: 1_552_384, CacheWrite: 7, Output: 30_304, Typed: true}
	if err != nil || got["7f3cq2xz"] != want {
		t.Fatalf("got %+v %v, want %+v", got, err, want)
	}
}

func TestVMRefusesWhatIsNotAnAnswer(t *testing.T) {
	for name, c := range map[string]struct {
		code int
		body string
	}{
		"a 5xx":            {http.StatusServiceUnavailable, `{"status":"success","data":{"resultType":"vector","result":[]}}`},
		"an error status":  {http.StatusOK, `{"status":"error","data":{"resultType":"vector","result":[]}}`},
		"a matrix":         {http.StatusOK, `{"status":"success","data":{"resultType":"matrix","result":[]}}`},
		"not JSON":         {http.StatusOK, `<html>`},
		"an oversize body": {http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[` + strings.Repeat(" ", maxVMReply) + `]}}`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := vmServer(t, c.code, c.body).RunUsage(t.Context()); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	_, err := vmServer(t, http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[`+strings.Repeat(" ", maxVMReply)+`]}}`).RunUsage(t.Context())
	if !errors.Is(err, httpx.ErrBodyTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

// meter.url is VictoriaMetrics in the cluster: http or https with a host, nothing else.
func TestNewVMRefuses(t *testing.T) {
	hc := httpx.New(time.Second, nil)
	for name, c := range map[string]struct {
		url, query string
		hc         *http.Client
	}{
		"another scheme": {"ftp://vmsingle:8428", "Q", hc},
		"no host":        {"http://", "Q", hc},
		"userinfo":       {"http://u:p@vmsingle:8428", "Q", hc},
		"a query string": {"http://vmsingle:8428?x=1", "Q", hc},
		"no query":       {"http://vmsingle:8428", "", hc},
		"no HTTP client": {"http://vmsingle:8428", "Q", nil},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewVM(c.url, c.query, c.hc); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	for _, u := range []string{"http://vmsingle-victoria-metrics-k8s-stack.observability.svc:8428", "https://vm.example/select/0/prometheus"} {
		if _, err := NewVM(u, "Q", hc); err != nil {
			t.Fatalf("%s: %v", u, err)
		}
	}
}
