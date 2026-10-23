// SPDX-License-Identifier: Apache-2.0

package slackapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func slackSign(secret, ts, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("v0:" + ts + ":" + body))
	return "v0=" + hex.EncodeToString(m.Sum(nil))
}

// R27, invariant 5: Slack's v0 signature over the raw body, five minutes either way, in constant
// time; anything else is 401 before the body is parsed.
func TestSlackSignature(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	body := `{"type":"event_callback","event_id":"Ev1"}`
	req := func(sig, ts, body string) *http.Request {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/slack/events", strings.NewReader(body))
		r.Header.Set("X-Slack-Signature", sig)
		r.Header.Set("X-Slack-Request-Timestamp", ts)
		return r
	}
	v := Verifier{Secret: func() string { return "s3cret" }, Now: func() time.Time { return now }}
	if got, err := v.Body(req(slackSign("s3cret", ts, body), ts, body)); err != nil || string(got) != body {
		t.Fatalf("%q %v", got, err)
	}
	old := strconv.FormatInt(now.Add(-6*time.Minute).Unix(), 10)
	for name, r := range map[string]*http.Request{
		"a wrong secret":    req(slackSign("other", ts, body), ts, body),
		"a stale timestamp": req(slackSign("s3cret", old, body), old, body),
		"a changed body":    req(slackSign("s3cret", ts, body), ts, body+" "),
		"no signature":      req("", ts, body),
		"an empty secret":   nil,
		"over 1 MiB":        req(slackSign("s3cret", ts, strings.Repeat("a", 1<<20+1)), ts, strings.Repeat("a", 1<<20+1)),
	} {
		t.Run(name, func(t *testing.T) {
			vv := v
			if r == nil {
				vv.Secret = func() string { return "" }
				r = req(slackSign("", ts, body), ts, body)
			}
			if _, err := vv.Body(r); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
