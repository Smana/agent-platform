// SPDX-License-Identifier: Apache-2.0

package slackapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/Smana/agent-platform/internal/httpx"
)

const (
	protocolVersion = "2025-11-25"
	maxReply        = 256 << 10
)

// Tokens caches the App's client-credentials token until it expires or the broker refuses it
// (L9): clientcredentials' own TokenSource reuses a token until it expires and cannot be told
// that the broker refused it. Fetch should go through httpx (oauth2.HTTPClient in the context).
type Tokens struct {
	Fetch func(ctx context.Context) (*oauth2.Token, error)

	mu  sync.Mutex
	tok *oauth2.Token
}

// Token is the cached access token, fetched when there is none or it expired.
func (s *Tokens) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.tok.Valid() {
		t, err := s.Fetch(ctx)
		if err != nil {
			return "", fmt.Errorf("slackapp: token: %w", err)
		}
		s.tok = t
	}
	return s.tok.AccessToken, nil
}

// Invalidate drops the cached token after a 401, so the retry fetches a new one.
func (s *Tokens) Invalidate() {
	s.mu.Lock()
	s.tok = nil
	s.mu.Unlock()
}

// Refused is a tool's refusal ("rejected: <code>"), shown in the thread as is.
type Refused string

func (r Refused) Error() string { return string(r) }

// Broker is the room-broker's MCP endpoint, called as the App's own principal. It never calls
// tasks/*: the App follows a room through webhooks.
type Broker struct {
	URL    string // the broker's base URL, from config only
	Tokens *Tokens
	HC     *http.Client // httpx.New in production
}

// RunCreate is a draft filed as a run for the person who asked.
type RunCreate struct {
	Draft
	MaxCostUSD float64
	OnBehalfOf string // slack:<team>/<user>
	Ref        string // slack/<team>/<channel>/<thread_ts>
	Key        string // the idempotency key
}

// RunResult is what run_create answered.
type RunResult struct {
	Room      string
	Issue     int
	Duplicate bool
}

// IssueDraft asks the broker to judge a draft against the issue template.
func (b Broker) IssueDraft(ctx context.Context, d Draft) (Draft, error) {
	sc, err := b.call(ctx, "issue_draft", map[string]any{"repo": d.Repo, "title": d.Title, "body": d.Body})
	if err != nil {
		return Draft{}, err
	}
	var out struct{ Repo, Title, Body string }
	if err := json.Unmarshal(sc, &out); err != nil {
		return Draft{}, fmt.Errorf("slackapp: issue_draft result: %w", err)
	}
	return Draft(out), nil
}

// RunCreate files the run. Its arguments are run_create's schema exactly: `text` is the draft's
// body and dataClass is always public.
func (b Broker) RunCreate(ctx context.Context, c RunCreate) (RunResult, error) {
	sc, err := b.call(ctx, "run_create", map[string]any{"repo": c.Repo, "title": c.Title, "text": c.Body,
		"dataClass": "public", "max_cost": c.MaxCostUSD, "on_behalf_of": c.OnBehalfOf, "ref": c.Ref,
		"idempotency_key": c.Key})
	if err != nil {
		return RunResult{}, err
	}
	var out struct {
		Room      string `json:"room"`
		Issue     int    `json:"issue"`
		Duplicate bool   `json:"duplicate"`
	}
	if err := json.Unmarshal(sc, &out); err != nil {
		return RunResult{}, fmt.Errorf("slackapp: run_create result: %w", err)
	}
	return RunResult(out), nil
}

// call is one stateless tools/call. A 401 drops the token and retries once with a new one.
func (b Broker) call(ctx context.Context, tool string, args map[string]any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		raw, status, err := b.post(ctx, body)
		if err != nil {
			return nil, err
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			b.Tokens.Invalidate()
			continue
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("slackapp: broker answered %d", status)
		}
		return decodeResult(raw)
	}
}

func (b Broker) post(ctx context.Context, body []byte) ([]byte, int, error) {
	tok, err := b.Tokens.Token(ctx)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(b.URL, "/")+"/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	req.Header.Set("Authorization", "Bearer "+tok)
	hc := b.HC
	if hc == nil {
		hc = httpx.New(30*time.Second, nil)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("slackapp: broker: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := httpx.ReadBody(resp.Body, maxReply)
	if err != nil {
		return nil, 0, fmt.Errorf("slackapp: broker reply: %w", err)
	}
	return raw, resp.StatusCode, nil
}

func decodeResult(raw []byte) (json.RawMessage, error) {
	var msg struct {
		Error  *struct{ Message string } `json:"error"`
		Result *struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			StructuredContent json.RawMessage `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, fmt.Errorf("slackapp: broker reply: %w", err)
	}
	switch {
	case msg.Error != nil:
		return nil, fmt.Errorf("slackapp: broker: %s", msg.Error.Message)
	case msg.Result == nil:
		return nil, errors.New("slackapp: broker reply has no result")
	case msg.Result.IsError:
		text := "rejected"
		if len(msg.Result.Content) > 0 {
			text = msg.Result.Content[0].Text
		}
		return nil, Refused(text)
	case len(msg.Result.StructuredContent) == 0:
		return nil, errors.New("slackapp: broker result has no structuredContent")
	}
	return msg.Result.StructuredContent, nil
}
