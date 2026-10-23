// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	k1 = []byte("0123456789abcdef0123456789abcdef")
	k2 = []byte("fedcba9876543210fedcba9876543210")
)

// R15, R16, live proof 2 offline, invariant 4: a delivery verifies under any of its keys; an old
// timestamp, a wrong signature, a forged id and a body changed in flight are rejected.
func TestVerify(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	body := []byte(`{"type":"room.ended","room":"aaaaaaaa","state":"done"}`)
	id := ID("slack-app", "aaaaaaaa", 42, "room.ended")
	h := Sign([][]byte{k1, k2}, id, now, body)
	if got, err := Verify([][]byte{k2}, h, body, now.Add(time.Minute)); err != nil || got != id {
		t.Fatalf("the second key alone: %q %v", got, err)
	}
	if !strings.HasPrefix(id, "evt_") || id != ID("slack-app", "aaaaaaaa", 42, "room.ended") || id == ID("other", "aaaaaaaa", 42, "room.ended") {
		t.Fatalf("the id is stable per subscriber, room, seq and type: %s", id)
	}
	tamper := func(f func(http.Header)) http.Header {
		c := h.Clone()
		f(c)
		return c
	}
	v0 := func(c http.Header) {
		c.Set("webhook-signature", strings.ReplaceAll(c.Get("webhook-signature"), "v1,", "v0,"))
	}
	for name, c := range map[string]struct {
		h    http.Header
		body []byte
		at   time.Time
	}{
		"older than five minutes": {h, body, now.Add(Tolerance + time.Second)},
		"from the future":         {h, body, now.Add(-Tolerance - time.Second)},
		"a wrong key":             {Sign([][]byte{[]byte("not the key, not the key, not it")}, id, now, body), body, now},
		"a forged id":             {tamper(func(c http.Header) { c.Set("webhook-id", "evt_forged") }), body, now},
		"a changed body":          {h, []byte(`{"type":"room.ended","room":"bbbbbbbb","state":"done"}`), now},
		"no signature":            {tamper(func(c http.Header) { c.Del("webhook-signature") }), body, now},
		"a v0 signature":          {tamper(v0), body, now},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify([][]byte{k1}, c.h, c.body, c.at); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// R16: one or two base64 keys, one per line; anything else refuses to start.
func TestReadKeys(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "k")
		_ = os.WriteFile(p, []byte(s), 0o600)
		return p
	}
	if ks, err := ReadKeys(write("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=\nZmVkY2JhOTg3NjU0MzIxMGZlZGNiYTk4NzY1NDMyMTA=\n")); err != nil || len(ks) != 2 {
		t.Fatalf("%d %v", len(ks), err)
	}
	for _, bad := range []string{"", "not base64!", "c2hvcnQ=", "a\nb\nc\n"} {
		if _, err := ReadKeys(write(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
