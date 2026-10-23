// SPDX-License-Identifier: Apache-2.0

package slackapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/sponsored"
)

type rpc struct {
	Method string `json:"method"`
	Params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"params"`
}

// fakeBroker answers every tools/call with result, recording what it was sent. accept is the one
// bearer it takes; "" accepts any.
type fakeBroker struct {
	mu      sync.Mutex
	calls   []rpc
	headers []http.Header
	accept  string
	result  string
	srv     *httptest.Server
}

func newFakeBroker(t *testing.T, accept, result string) *fakeBroker {
	t.Helper()
	f := &fakeBroker{accept: accept, result: result}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var c rpc
		_ = json.Unmarshal(b, &c)
		f.mu.Lock()
		f.calls = append(f.calls, c)
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()
		if r.URL.Path != "/mcp" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if f.accept != "" && r.Header.Get("Authorization") != "Bearer "+f.accept {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":%s}`, f.result)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func staticTokens(tok string) *Tokens {
	return &Tokens{Fetch: func(context.Context) (*oauth2.Token, error) {
		return &oauth2.Token{AccessToken: tok, Expiry: time.Now().Add(time.Hour)}, nil
	}}
}

func TestBrokerIssueDraft(t *testing.T) {
	f := newFakeBroker(t, "tok", `{"structuredContent":{"repo":"Smana/x","title":"t2","body":"b2"}}`)
	b := Broker{URL: f.srv.URL, Tokens: staticTokens("tok"), HC: f.srv.Client()}
	d, err := b.IssueDraft(context.Background(), Draft{Repo: "Smana/x", Title: "t", Body: "b"})
	if err != nil || d != (Draft{Repo: "Smana/x", Title: "t2", Body: "b2"}) {
		t.Fatalf("%+v %v", d, err)
	}
	c := f.calls[0]
	if c.Method != "tools/call" || c.Params.Name != "issue_draft" ||
		!maps.Equal(c.Params.Arguments, map[string]any{"repo": "Smana/x", "title": "t", "body": "b"}) {
		t.Fatalf("%+v", c)
	}
	if h := f.headers[0]; h.Get("MCP-Protocol-Version") != "2025-11-25" || h.Get("Authorization") != "Bearer tok" {
		t.Fatalf("%v", h)
	}
}

func TestBrokerRefusalIsRefused(t *testing.T) {
	f := newFakeBroker(t, "", `{"isError":true,"content":[{"type":"text","text":"rejected: template"}]}`)
	b := Broker{URL: f.srv.URL, Tokens: staticTokens("tok"), HC: f.srv.Client()}
	_, err := b.IssueDraft(context.Background(), Draft{Repo: "Smana/x", Title: "t"})
	var r Refused
	if !errors.As(err, &r) || string(r) != "rejected: template" || err.Error() != "rejected: template" {
		t.Fatalf("%T %v", err, err)
	}
}

func TestBrokerRunCreate(t *testing.T) {
	f := newFakeBroker(t, "", `{"structuredContent":{"room":"r-1","issue":42,"duplicate":true}}`)
	b := Broker{URL: f.srv.URL, Tokens: staticTokens("tok"), HC: f.srv.Client()}
	body := "## What is wrong\nx\n## What it should be\ny\n## Acceptance check\nz"
	ref := "slack/T01ABC/C03/1729.0001"
	res, err := b.RunCreate(context.Background(), RunCreate{
		Draft:      Draft{Repo: "Smana/ai-native-ref", Title: "docs(agents): the link is dead", Body: body},
		MaxCostUSD: 2, OnBehalfOf: "slack:T01ABC/U02DEF", Ref: ref, Key: "T01ABC/C03/1729.0002"})
	if err != nil || res != (RunResult{Room: "r-1", Issue: 42, Duplicate: true}) {
		t.Fatalf("%+v %v", res, err)
	}
	want := map[string]any{"repo": "Smana/ai-native-ref", "title": "docs(agents): the link is dead",
		"text": body, "dataClass": "public",
		"max_cost": 2.0, "on_behalf_of": "slack:T01ABC/U02DEF", "ref": ref,
		"idempotency_key": "T01ABC/C03/1729.0002"}
	c := f.calls[0]
	if c.Params.Name != "run_create" || !maps.Equal(c.Params.Arguments, want) {
		t.Fatalf("%+v", c.Params)
	}
	if !sponsored.ValidRef(ref) {
		t.Fatalf("broker would refuse ref %q", ref)
	}
}

func TestBrokerNeverSendsTasks(t *testing.T) {
	f := newFakeBroker(t, "", `{"structuredContent":{}}`)
	b := Broker{URL: f.srv.URL, Tokens: staticTokens("tok"), HC: f.srv.Client()}
	_, _ = b.IssueDraft(context.Background(), Draft{})
	_, _ = b.RunCreate(context.Background(), RunCreate{})
	for _, c := range f.calls {
		if strings.HasPrefix(c.Method, "tasks/") {
			t.Fatalf("sent %s", c.Method)
		}
	}
}

// L9: a 401 drops the token and retries once on a new one; a second 401 fails.
func TestBrokerRefreshesOn401(t *testing.T) {
	f := newFakeBroker(t, "t2", `{"structuredContent":{"repo":"Smana/x","title":"t","body":"b"}}`)
	var n atomic.Int32
	tokens := &Tokens{Fetch: func(context.Context) (*oauth2.Token, error) {
		return &oauth2.Token{AccessToken: fmt.Sprintf("t%d", n.Add(1)), Expiry: time.Now().Add(time.Hour)}, nil
	}}
	b := Broker{URL: f.srv.URL, Tokens: tokens, HC: f.srv.Client()}
	if _, err := b.IssueDraft(context.Background(), Draft{Repo: "Smana/x", Title: "t"}); err != nil || n.Load() != 2 {
		t.Fatalf("err %v fetches %d", err, n.Load())
	}

	f2 := newFakeBroker(t, "never", `{}`)
	n.Store(0)
	tokens = &Tokens{Fetch: func(context.Context) (*oauth2.Token, error) {
		return &oauth2.Token{AccessToken: fmt.Sprintf("t%d", n.Add(1)), Expiry: time.Now().Add(time.Hour)}, nil
	}}
	b = Broker{URL: f2.srv.URL, Tokens: tokens, HC: f2.srv.Client()}
	if _, err := b.IssueDraft(context.Background(), Draft{Repo: "Smana/x", Title: "t"}); err == nil || n.Load() != 2 {
		t.Fatalf("err %v fetches %d", err, n.Load())
	}
}

// The token cache is reused until it expires.
func TestTokensCache(t *testing.T) {
	var n int
	s := &Tokens{Fetch: func(context.Context) (*oauth2.Token, error) {
		n++
		return &oauth2.Token{AccessToken: "a", Expiry: time.Now().Add(time.Hour)}, nil
	}}
	for range 3 {
		if _, err := s.Token(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n != 1 {
		t.Fatalf("fetched %d times", n)
	}
}

// L9: the token fetch goes through the audited egress client, passed in the context.
func TestTokenFetchUsesHTTPClientFromContext(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"x","token_type":"Bearer","expires_in":3600}`))
	}))
	defer srv.Close()
	cc := clientcredentials.Config{ClientID: "i", ClientSecret: "s", TokenURL: srv.URL + "/oauth/v2/token"}
	hc := httpx.New(5*time.Second, nil)
	tokens := &Tokens{Fetch: func(ctx context.Context) (*oauth2.Token, error) {
		return cc.Token(context.WithValue(ctx, oauth2.HTTPClient, hc))
	}}
	if tok, err := tokens.Token(context.Background()); err != nil || tok != "x" || hits.Load() != 1 {
		t.Fatalf("%q %v %d", tok, err, hits.Load())
	}
}
