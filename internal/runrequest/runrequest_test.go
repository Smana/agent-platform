// SPDX-License-Identifier: Apache-2.0

package runrequest

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestManifestIsAValidClaim(t *testing.T) {
	res, err := Manifest{}.Request(t.Context(), Request{Role: "implementer", Repository: "Smana/cloud-native-ref",
		BaseRef: "4be1c9d", Branch: "agent/3kq7x2ma", TaskText: "brief", DataClass: "public", RoomRef: "3kq7x2ma",
		Principal: "human:291", EgressProfiles: []string{"pypi"}})
	if err != nil || res.Via != "manifest" || !regexp.MustCompile(`^[a-z2-7]{8}$`).MatchString(res.RunID) {
		t.Fatal(res, err)
	}
	var claim struct {
		Metadata struct{ Name, Namespace string }
		Spec     map[string]any
	}
	_ = json.Unmarshal(res.Manifest, &claim)
	if claim.Metadata.Name != "xplane-run-"+res.RunID || claim.Metadata.Namespace != "agents" ||
		claim.Spec["roomRef"] != "3kq7x2ma" || claim.Spec["branch"] != "agent/3kq7x2ma" || claim.Spec["principal"] != "human:291" {
		t.Fatalf("%s", res.Manifest)
	}
	if !strings.Contains(string(res.Manifest), `"egress":{"profiles":["pypi"]}`) || !strings.Contains(string(res.Manifest), `"task":{"text":"brief"}`) {
		t.Fatalf("egress and task: %s", res.Manifest)
	}
	plain, _ := Manifest{}.Request(t.Context(), Request{Role: "reviewer", TaskURL: "https://github.com/x/y/pull/1"})
	if strings.Contains(string(plain.Manifest), "egress") || !strings.Contains(string(plain.Manifest), `"task":{"url":"https://github.com/x/y/pull/1"}`) {
		t.Fatalf("no egress unless asked; a URL task: %s", plain.Manifest)
	}
	if plain.RunID == res.RunID {
		t.Fatal("every request gets its own run id")
	}
}

// C4: the human's own token goes to the factory, never an asserted sub.
func TestFactoryForwardsTheHumansToken(t *testing.T) {
	var auth, key string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, key = r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key")
		if r.URL.Path != "/v1/runs" || r.Method != http.MethodPost {
			w.WriteHeader(404)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"runId":"7f3cq2xz","via":"forged","manifest":{"x":1}}`))
	}))
	defer srv.Close()
	res, err := Factory{URL: srv.URL}.Request(t.Context(), Request{Role: "reviewer", TaskURL: "https://github.com/x/y/pull/1",
		Principal: "human:291", AccessToken: "tok", IdempotencyKey: "3kq7x2ma:human:291:s1:7"})
	if err != nil || res.RunID != "7f3cq2xz" || auth != "Bearer tok" {
		t.Fatal(res, err, auth)
	}
	if key != "3kq7x2ma:human:291:s1:7" {
		t.Fatalf("Idempotency-Key = %q (review 4.4 I1)", key)
	}
	if res.Via != "factory" || res.Manifest != nil {
		t.Fatalf("the factory's answer names only the run: %+v", res)
	}
	if _, asserted := body["principal"]; asserted || body["role"] != "reviewer" {
		t.Fatalf("the factory reads the principal from the token, never the body: %v", body)
	}
	over := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }))
	defer over.Close()
	if _, err := (Factory{URL: over.URL}).Request(t.Context(), Request{AccessToken: "tok", IdempotencyKey: "k"}); !errors.Is(err, ErrBudget) {
		t.Fatal(err)
	}
}

func TestFactoryRefusals(t *testing.T) {
	calls := 0
	code := http.StatusForbidden
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(code)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	f := Factory{URL: srv.URL, HC: srv.Client()}
	if _, err := f.Request(t.Context(), Request{AccessToken: "tok", IdempotencyKey: "k"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("403: %v", err)
	}
	code = http.StatusBadGateway
	if _, err := f.Request(t.Context(), Request{AccessToken: "tok", IdempotencyKey: "k"}); err == nil || errors.Is(err, ErrBudget) || errors.Is(err, ErrForbidden) {
		t.Fatalf("502 is unavailable, not a refusal: %v", err)
	}
	code = http.StatusCreated
	if _, err := f.Request(t.Context(), Request{AccessToken: "tok", IdempotencyKey: "k"}); err == nil {
		t.Fatal("an unreadable answer is an error")
	}
	n := calls
	if _, err := f.Request(t.Context(), Request{}); !errors.Is(err, ErrForbidden) || calls != n {
		t.Fatalf("no token, no call: %v, %d calls", err, calls-n)
	}
	if _, err := f.Request(t.Context(), Request{AccessToken: "tok"}); !errors.Is(err, ErrNoKey) || calls != n {
		t.Fatalf("no idempotency key, no call: %v, %d calls", err, calls-n)
	}
}

// Review 4.4 I1, before SP3: a retried act renders the same claim name, so the
// owner applying both creates one run; another act gets another run.
func TestManifestDedupesOnTheKey(t *testing.T) {
	r := Request{Role: "implementer", IdempotencyKey: "3kq7x2ma:human:291:s1:7"}
	a, _ := Manifest{}.Request(t.Context(), r)
	b, _ := Manifest{}.Request(t.Context(), r)
	r.IdempotencyKey = "3kq7x2ma:human:291:s1:8"
	c, _ := Manifest{}.Request(t.Context(), r)
	if a.RunID != b.RunID || a.RunID == c.RunID || !regexp.MustCompile(`^[a-z2-7]{8}$`).MatchString(a.RunID) {
		t.Fatalf("%s %s %s", a.RunID, b.RunID, c.RunID)
	}
}
