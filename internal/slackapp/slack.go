// SPDX-License-Identifier: Apache-2.0

package slackapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Smana/agent-platform/internal/httpx"
)

const (
	slackAPI      = "https://slack.com/api/"
	maxSlackReply = 1 << 20
)

// Metadata is the draft the App attaches to its own message (R23); Slack keeps it off the
// visible text.
type Metadata struct {
	EventType    string       `json:"event_type"`
	EventPayload DraftPayload `json:"event_payload"`
}

// DraftPayload is the checked draft and its hash, sha256 hex of repo, title and body joined by NUL.
type DraftPayload struct {
	Repo   string `json:"repo"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	SHA256 string `json:"sha256"`
	// Author is the Slack user who wrote the mention: the only one whose thread replies edit the
	// draft. Rev counts the drafts the thread has had, 1 for the first.
	Author string `json:"author,omitempty"`
	Rev    int    `json:"rev,omitempty"`
}

// Message is a Slack message as the ✅ flow reads it (M9). User is who posted it: the App's own
// bot user for its drafts.
type Message struct {
	TS, ThreadTS, User, Text string
	Metadata                 *Metadata
}

// Slack is the Web API calls the App makes.
type Slack interface {
	// Thread is the thread's messages, oldest first, the parent included, with their metadata.
	Thread(ctx context.Context, channel, threadTS string) ([]Message, error)
	Post(ctx context.Context, channel, threadTS, text string, meta *Metadata) (ts string, err error)
	Message(ctx context.Context, channel, ts string) (Message, error)
}

// SlackClient is Slack's Web API over httpx. Token is read at use, so a rotated Secret applies.
type SlackClient struct {
	BaseURL string // https://slack.com/api/ when empty
	Token   func() string
	HC      *http.Client // httpx.New in production
}

type slackAnswer struct {
	OK               bool   `json:"ok"`
	Error            string `json:"error"`
	TS               string `json:"ts"`
	ResponseMetadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
	Messages []struct {
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
		User     string `json:"user"`
		Text     string `json:"text"`
		Metadata *struct {
			EventType    string       `json:"event_type"`
			EventPayload DraftPayload `json:"event_payload"`
		} `json:"metadata"`
	} `json:"messages"`
}

// Post is chat.postMessage: no unfurling, the metadata attached when there is one.
func (c *SlackClient) Post(ctx context.Context, channel, threadTS, text string, meta *Metadata) (string, error) {
	body := map[string]any{"channel": channel, "text": text, "unfurl_links": false, "unfurl_media": false}
	if threadTS != "" {
		body["thread_ts"] = threadTS
	}
	if meta != nil {
		body["metadata"] = meta
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("chat.postMessage"), bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	a, err := c.do(req)
	if err != nil {
		return "", err
	}
	return a.TS, nil
}

// Message is conversations.replies for one ts (M9): the draft is a thread reply, which
// conversations.history never returns. The answer may also hold the thread's parent, so it keeps
// only the message whose ts is the one asked.
func (c *SlackClient) Message(ctx context.Context, channel, ts string) (Message, error) {
	q := url.Values{"channel": {channel}, "ts": {ts}, "limit": {"1"}, "inclusive": {"true"}, "include_all_metadata": {"true"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("conversations.replies")+"?"+q.Encode(), nil)
	if err != nil {
		return Message{}, err
	}
	a, err := c.do(req)
	if err != nil {
		return Message{}, err
	}
	for _, m := range a.Messages {
		if m.TS != ts {
			continue
		}
		out := Message{TS: m.TS, ThreadTS: m.ThreadTS, User: m.User, Text: m.Text}
		if m.Metadata != nil {
			out.Metadata = &Metadata{EventType: m.Metadata.EventType, EventPayload: m.Metadata.EventPayload}
		}
		return out, nil
	}
	return Message{}, errors.New("slackapp: slack: the message is not in the answer")
}

const (
	threadPageSize = 200
	threadMaxPages = 5
)

// Thread is conversations.replies for a thread, followed over a few pages at most: a thread longer
// than that is read as far as it goes, which is the draft's start, not its latest.
func (c *SlackClient) Thread(ctx context.Context, channel, threadTS string) ([]Message, error) {
	var out []Message
	cursor := ""
	for range threadMaxPages {
		q := url.Values{"channel": {channel}, "ts": {threadTS}, "limit": {strconv.Itoa(threadPageSize)}, "include_all_metadata": {"true"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("conversations.replies")+"?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		a, err := c.do(req)
		if err != nil {
			return nil, err
		}
		for _, m := range a.Messages {
			msg := Message{TS: m.TS, ThreadTS: m.ThreadTS, User: m.User, Text: m.Text}
			if m.Metadata != nil {
				msg.Metadata = &Metadata{EventType: m.Metadata.EventType, EventPayload: m.Metadata.EventPayload}
			}
			out = append(out, msg)
		}
		if cursor = a.ResponseMetadata.NextCursor; cursor == "" {
			break
		}
	}
	return out, nil
}

func (c *SlackClient) url(method string) string {
	base := c.BaseURL
	if base == "" {
		base = slackAPI
	}
	return strings.TrimSuffix(base, "/") + "/" + method
}

// do sends the request as the bot and decodes the answer; a non-ok answer is an error whose text
// is Slack's own code, never the token.
func (c *SlackClient) do(req *http.Request) (slackAnswer, error) {
	req.Header.Set("Authorization", "Bearer "+c.Token())
	hc := c.HC
	if hc == nil {
		hc = httpx.New(10*time.Second, nil)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return slackAnswer{}, fmt.Errorf("slackapp: slack: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := httpx.ReadBody(resp.Body, maxSlackReply)
	if err != nil {
		return slackAnswer{}, fmt.Errorf("slackapp: slack reply: %w", err)
	}
	var a slackAnswer
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("slackapp: slack answered %d: %w", resp.StatusCode, err)
	}
	if !a.OK {
		return a, fmt.Errorf("slackapp: slack: %s", a.Error)
	}
	return a, nil
}
