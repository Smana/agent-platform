// SPDX-License-Identifier: Apache-2.0

// Package slackapp is the factory's Slack App (factory interfaces F9): a sponsored principal that
// drafts and files issues from a thread, and a webhook subscriber that keeps the thread told.
// It never answers an approval and never relays room text.
package slackapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maxSlackBody = 1 << 20
	skew         = 5 * time.Minute
)

// ErrSignature is any request that does not verify; it never says which check failed.
var ErrSignature = errors.New("slackapp: request not signed by Slack")

// Verifier checks Slack's v0 request signature (R27). Secret is read at use: a rotation is a
// Secret update and a restart-free reload.
type Verifier struct {
	Secret func() string
	Now    func() time.Time
}

// Body reads and verifies the request's body, returning it only when Slack signed it.
func (v Verifier) Body(r *http.Request) ([]byte, error) {
	secret := v.Secret()
	if secret == "" {
		return nil, ErrSignature // an unprovisioned App is closed, never open
	}
	ts := r.Header.Get("X-Slack-Request-Timestamp")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return nil, ErrSignature
	}
	if d := v.Now().Sub(time.Unix(sec, 0)); d > skew || d < -skew {
		return nil, ErrSignature
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSlackBody+1))
	if err != nil || len(body) > maxSlackBody {
		return nil, ErrSignature
	}
	got, err := hex.DecodeString(strings.TrimPrefix(r.Header.Get("X-Slack-Signature"), "v0="))
	if err != nil || !strings.HasPrefix(r.Header.Get("X-Slack-Signature"), "v0=") {
		return nil, ErrSignature
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("v0:" + ts + ":"))
	m.Write(body)
	if !hmac.Equal(got, m.Sum(nil)) {
		return nil, ErrSignature
	}
	return body, nil
}
