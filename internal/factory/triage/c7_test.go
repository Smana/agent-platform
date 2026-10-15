// SPDX-License-Identifier: Apache-2.0

package triage

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/agent-platform/internal/httpx"
)

func TestHTTPClassifier(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tier":"` + strings.Repeat("a", 2<<20) + `"}`))
	}))
	defer big.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tier":"standard","confidence":0.9}`))
	}))
	defer ok.Close()

	t.Run("no client is an error naming the client", func(t *testing.T) {
		_, err := HTTPClassifier{URL: ok.URL}.Classify(t.Context(), C7Request{})
		if err == nil || !strings.Contains(err.Error(), "no HTTP client") {
			t.Fatalf("err = %v, want one naming the missing client", err)
		}
	})
	t.Run("a 2 MiB reply is refused", func(t *testing.T) {
		_, err := HTTPClassifier{URL: big.URL, HC: big.Client()}.Classify(t.Context(), C7Request{})
		if !errors.Is(err, httpx.ErrBodyTooLarge) {
			t.Fatalf("err = %v, want httpx.ErrBodyTooLarge", err)
		}
	})
	t.Run("a small reply decodes", func(t *testing.T) {
		got, err := HTTPClassifier{URL: ok.URL, HC: ok.Client()}.Classify(t.Context(), C7Request{})
		if err != nil || got.Tier != "standard" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
}
