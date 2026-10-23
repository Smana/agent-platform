// SPDX-License-Identifier: Apache-2.0

package runrequest

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateTask(t *testing.T) {
	var auth, key, path string
	var body map[string]any
	status, answer := http.StatusCreated, `{"task":"abcd2345","room":"abcd2345","issue":42,"duplicate":false}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, key, path = r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"), r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()
	f := Factory{URL: srv.URL}
	req := TaskRequest{Repository: "Smana/x", Text: "t", DataClass: "public", MaxCostUSD: 2, AccessToken: "tok", IdempotencyKey: "k1"}
	res, err := f.CreateTask(t.Context(), req)
	if err != nil || res.Task != "abcd2345" || res.Room != "abcd2345" || res.Issue != 42 || res.Duplicate ||
		auth != "Bearer tok" || key != "k1" || path != "/v1/tasks" || body["maxCostUSD"] != float64(2) || body["repository"] != "Smana/x" {
		t.Fatal(res, err, auth, key, path, body)
	}
	if _, leaked := body["AccessToken"]; leaked {
		t.Fatal("the token is never in the body")
	}
	status, answer = http.StatusOK, `{"task":"abcd2345","room":"abcd2345","issue":42,"duplicate":true}`
	if res, err = f.CreateTask(t.Context(), req); err != nil || !res.Duplicate {
		t.Fatal(res, err)
	}
	for _, c := range []struct {
		status int
		body   string
		reason string
	}{
		{403, `{"error":"not_linked"}`, "not_linked"}, {409, `{"error":"idempotency_conflict"}`, "idempotency_conflict"},
		{503, `{"error":"tasks_unavailable"}`, "tasks_unavailable"}, {400, `{"error":"leak: secret"}`, "refused"},
		{500, `not json`, "refused"}, {403, `{"error":"admitted"}`, "refused"},
	} {
		status, answer = c.status, c.body
		_, err := f.CreateTask(t.Context(), req)
		var refused ErrRefused
		if !errors.As(err, &refused) || refused.Status != c.status || refused.Reason != c.reason {
			t.Errorf("%d %s: %v", c.status, c.body, err)
		}
	}
	if _, err := f.CreateTask(t.Context(), TaskRequest{IdempotencyKey: "k"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("no token: %v", err)
	}
	if _, err := f.CreateTask(t.Context(), TaskRequest{AccessToken: "t"}); !errors.Is(err, ErrNoKey) {
		t.Fatalf("no key: %v", err)
	}
}
