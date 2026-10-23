// SPDX-License-Identifier: Apache-2.0

// Package webhook is the broker's outbound events (factory interfaces F6): id-only payloads,
// signed as Standard Webhooks sign them, so a subscriber verifies, rejects a stale timestamp and
// dedupes on webhook-id. Both the broker and the factory's Slack App use it.
package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Tolerance is how far a delivery's timestamp may stand from the receiver's clock (the design's
// five minutes), either way.
const Tolerance = 5 * time.Minute

// minKey is the shortest signing key accepted: 32 bytes, HMAC-SHA256's block-strength floor.
const minKey = 32

// Event is one delivery's payload: ids, states and reason codes only (R19). No room text.
type Event struct {
	Type   string `json:"type"`
	Room   string `json:"room"`
	Repo   string `json:"repo,omitempty"`
	Run    string `json:"run,omitempty"`
	PR     int    `json:"pr,omitempty"`
	Reason string `json:"reason,omitempty"`
	Limit  string `json:"limit,omitempty"`
	State  string `json:"state,omitempty"`
}

// ID is a delivery's webhook-id: the same for every retry and every leader, distinct per
// subscriber, so each subscriber dedupes its own stream (R15).
func ID(subscriber, room string, seq int64, typ string) string {
	sum := sha256.Sum256([]byte(subscriber + "\x00" + room + "\x00" + strconv.FormatInt(seq, 10) + "\x00" + typ))
	return "evt_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:16]))
}

// mac is the HMAC-SHA256 over "id.timestamp.body" under one key.
func mac(key []byte, id, ts string, body []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + ts + "."))
	m.Write(body)
	return m.Sum(nil)
}

// Sign is a delivery's three headers: one v1 signature per key, space-separated (R16).
func Sign(keys [][]byte, id string, ts time.Time, body []byte) http.Header {
	t := strconv.FormatInt(ts.Unix(), 10)
	sigs := make([]string, 0, len(keys))
	for _, k := range keys {
		sigs = append(sigs, "v1,"+base64.StdEncoding.EncodeToString(mac(k, id, t, body)))
	}
	h := http.Header{}
	h.Set("webhook-id", id)
	h.Set("webhook-timestamp", t)
	h.Set("webhook-signature", strings.Join(sigs, " "))
	return h
}

// ErrInvalid is any delivery that does not verify; its text never says which check failed.
var ErrInvalid = errors.New("webhook: invalid delivery")

// Verify checks a delivery against any of keys: the timestamp within Tolerance of now, then one v1
// signature over id, timestamp and body, compared in constant time. It returns the webhook-id.
func Verify(keys [][]byte, h http.Header, body []byte, now time.Time) (string, error) {
	id, ts := h.Get("webhook-id"), h.Get("webhook-timestamp")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if id == "" || err != nil {
		return "", ErrInvalid
	}
	if d := now.Sub(time.Unix(sec, 0)); d > Tolerance || d < -Tolerance {
		return "", ErrInvalid
	}
	for _, sig := range strings.Fields(h.Get("webhook-signature")) {
		v, ok := strings.CutPrefix(sig, "v1,")
		if !ok {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			continue
		}
		for _, k := range keys {
			if hmac.Equal(got, mac(k, id, ts, body)) {
				return id, nil
			}
		}
	}
	return "", ErrInvalid
}

// ReadKeys reads one or two base64 keys, one per line (R16).
func ReadKeys(path string) ([][]byte, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("webhook: keys: %w", err)
	}
	var keys [][]byte
	for line := range bytes.Lines(raw) {
		s := strings.TrimSpace(string(line))
		if s == "" {
			continue
		}
		k, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(k) < minKey {
			return nil, errors.New("webhook: keys: each line is a base64 key of at least 32 bytes")
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 || len(keys) > 2 {
		return nil, errors.New("webhook: keys: one or two keys")
	}
	return keys, nil
}
