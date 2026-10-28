// SPDX-License-Identifier: Apache-2.0

package slackapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func slackServer(t *testing.T, handle func(method string, r *http.Request, body []byte) string) *SlackClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer xoxb-test" {
			t.Errorf("authorization %q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, handle(r.URL.Path, r, body))
	}))
	t.Cleanup(srv.Close)
	return &SlackClient{BaseURL: srv.URL + "/api/", Token: func() string { return "xoxb-test" }, HC: srv.Client()}
}

// R22/R23: Post sends chat.postMessage with the metadata and no unfurling, and returns ts.
func TestSlackPost(t *testing.T) {
	var got map[string]any
	c := slackServer(t, func(path string, _ *http.Request, body []byte) string {
		if path != "/api/chat.postMessage" {
			t.Errorf("path %s", path)
		}
		_ = json.Unmarshal(body, &got)
		return `{"ok":true,"ts":"1729.0002"}`
	})
	meta := &Metadata{EventType: "factory_draft", EventPayload: DraftPayload{Repo: "a/b", Title: "t", Body: "b", SHA256: "h"}}
	ts, err := c.Post(context.Background(), "C03", "1729.0001", "hello", meta)
	if err != nil || ts != "1729.0002" {
		t.Fatalf("%q %v", ts, err)
	}
	if got["channel"] != "C03" || got["thread_ts"] != "1729.0001" || got["text"] != "hello" ||
		got["unfurl_links"] != false || got["unfurl_media"] != false {
		t.Fatalf("%+v", got)
	}
	m, _ := got["metadata"].(map[string]any)
	p, _ := m["event_payload"].(map[string]any)
	if m["event_type"] != "factory_draft" || p["repo"] != "a/b" || p["sha256"] != "h" {
		t.Fatalf("metadata %+v", got["metadata"])
	}
}

// M9: Message reads conversations.replies with ts=item.ts and keeps the message whose ts is the
// one asked; the thread's parent (also in a reply answer) is never taken for it.
func TestSlackMessage(t *testing.T) {
	var q url.Values
	c := slackServer(t, func(path string, r *http.Request, _ []byte) string {
		if path != "/api/conversations.replies" {
			t.Errorf("path %s", path)
		}
		q = r.URL.Query()
		return `{"ok":true,"messages":[
		 {"ts":"1729.0001","user":"U02DEF","text":"parent"},
		 {"ts":"1729.0002","thread_ts":"1729.0001","user":"U0BOT","text":"draft",
		  "metadata":{"event_type":"factory_draft","event_payload":{"repo":"a/b","title":"t","body":"b","sha256":"h"}}}]}`
	})
	m, err := c.Message(context.Background(), "C03", "1729.0002")
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("channel") != "C03" || q.Get("ts") != "1729.0002" || q.Get("limit") != "1" ||
		q.Get("inclusive") != "true" || q.Get("include_all_metadata") != "true" {
		t.Fatalf("query %v", q)
	}
	if m.TS != "1729.0002" || m.ThreadTS != "1729.0001" || m.User != "U0BOT" || m.Metadata == nil ||
		m.Metadata.EventType != "factory_draft" || m.Metadata.EventPayload.SHA256 != "h" {
		t.Fatalf("%+v", m)
	}
	if _, err := c.Message(context.Background(), "C03", "9999.0000"); err == nil {
		t.Fatal("a message that is not in the answer was invented")
	}
}

func TestSlackNotOKIsError(t *testing.T) {
	c := slackServer(t, func(string, *http.Request, []byte) string { return `{"ok":false,"error":"channel_not_found"}` })
	if _, err := c.Post(context.Background(), "C03", "", "x", nil); err == nil {
		t.Fatal("Post: ok:false accepted")
	}
	if _, err := c.Message(context.Background(), "C03", "1.0"); err == nil {
		t.Fatal("Message: ok:false accepted")
	}
}

func TestSlackThreadFollowsTheCursor(t *testing.T) {
	var cursors []string
	c := slackServer(t, func(_ string, r *http.Request, _ []byte) string {
		cur := r.URL.Query().Get("cursor")
		cursors = append(cursors, cur)
		if cur == "" {
			return `{"ok":true,"messages":[{"ts":"1729.0001","user":"U02DEF","text":"parent"}],"response_metadata":{"next_cursor":"c2"}}`
		}
		return `{"ok":true,"messages":[{"ts":"1729.0002","thread_ts":"1729.0001","user":"U0BOT","text":"d",
		 "metadata":{"event_type":"factory_draft","event_payload":{"repo":"a/b","author":"U02DEF","rev":2}}}]}`
	})
	msgs, err := c.Thread(context.Background(), "C03", "1729.0001")
	if err != nil || len(msgs) != 2 || len(cursors) != 2 || cursors[1] != "c2" ||
		msgs[1].Metadata == nil || msgs[1].Metadata.EventPayload.Author != "U02DEF" || msgs[1].Metadata.EventPayload.Rev != 2 {
		t.Fatalf("%v %v %+v", err, cursors, msgs)
	}
}

func TestSlackThreadPastTheLastPageFailsClosed(t *testing.T) {
	c := slackServer(t, func(string, *http.Request, []byte) string {
		return `{"ok":true,"messages":[{"ts":"1729.0001","user":"U02DEF","text":"p"}],"response_metadata":{"next_cursor":"more"}}`
	})
	if _, err := c.Thread(context.Background(), "C03", "1729.0001"); !errors.Is(err, ErrThreadTooLong) {
		t.Fatalf("%v", err)
	}
}
