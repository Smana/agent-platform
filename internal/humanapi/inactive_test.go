// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Smana/agent-platform/internal/wire"
)

// #126: a ZITADEL user deactivated after their token was minted is refused once the cached
// active answer is a TTL old, on every human route, admins included; a younger answer serves.
func TestADeactivatedUserIsRefusedPastTheTTL(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.Header
	}{
		{"a member", header("dev1")},
		{"an admin", admin("boss")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := w0()
			e := setup(t, abc(t), w.option())
			if code, _ := get(t, e, "/api/rooms", tc.h); code != http.StatusOK {
				t.Fatalf("an active user: %d", code)
			}
			sub := tc.h.Get("X-Test-User")
			w.set(func() { w.inactive[sub], w.now = true, w.now.Add(4*time.Minute) })
			if got := listed(t, e, tc.h); len(got) == 0 {
				t.Fatal("a cached active answer younger than the TTL must serve")
			}
			w.set(func() { w.now = w.now.Add(2 * time.Minute) })
			for _, path := range []string{"/api/rooms", "/api/rooms/" + roomID + "/summary", "/api/roomctl"} {
				if code, body := get(t, e, path, tc.h); code != http.StatusUnauthorized || body != "user_inactive\n" {
					t.Errorf("GET %s: %d %q, want 401 user_inactive", path, code, body)
				}
			}
			if code, body := opens(t, e, roomID, tc.h); code != http.StatusUnauthorized || body != "user_inactive\n" {
				t.Errorf("the WebSocket: %d %q, want 401 user_inactive", code, body)
			}
			if body := scrape(t, e); !strings.Contains(body, `rooms_rejected_actions_total{reason="user_inactive"}`) {
				t.Error("the refusal is not counted in rooms_rejected_actions_total")
			}
		})
	}
}

// A state ZITADEL cannot give past the cache fails closed, for an admin too, naming nothing.
func TestAnUnreadableUserStateFailsClosed(t *testing.T) {
	w := w0()
	w.stateErr = true
	e := setup(t, abc(t), w.option())
	for _, h := range []http.Header{header("dev1"), admin("boss")} {
		if code, body := get(t, e, "/api/rooms", h); code != http.StatusServiceUnavailable || body != "access_unverified\n" {
			t.Errorf("%s: %d %q, want 503 access_unverified", h.Get("X-Test-User"), code, body)
		}
	}
}

// A machine principal has no ZITADEL user state: it is never refused as inactive.
func TestAMachinePrincipalIsNotRefusedAsInactive(t *testing.T) {
	w := w0()
	w.inactive["factory"] = true
	e := setup(t, abc(t), w.option())
	if code, body := get(t, e, "/api/roomctl", header("factory", "X-Test-Kind", "system")); code != http.StatusOK {
		t.Fatalf("a system principal: %d %q", code, body)
	}
}

// An open socket, a member's or an admin's, is cut on the ping once the user is inactive past
// the cache, as 5d's revoke cuts it.
func TestAnOpenSocketIsCutWhenTheUserTurnsInactive(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.Header
	}{
		{"a member", header("dev1")},
		{"an admin", admin("boss")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := w0()
			timers := newFakeTimers(30 * time.Second)
			e := setup(t, abc(t), w.option(), timers.option)
			p := openAs(t, e, tc.h)
			sub := tc.h.Get("X-Test-User")
			w.set(func() { w.inactive[sub], w.now = true, w.now.Add(4*time.Minute) })
			timers.beat(t)
			e.log.add(1)
			if f := p.next(t); f.Type != wire.FrameEvent || f.Event.Seq != 11 {
				t.Fatalf("a fresh cached answer must keep the stream: %+v", f)
			}
			w.set(func() { w.now = w.now.Add(2 * time.Minute) })
			take(t, timers.pings, "ping").c <- time.Time{}
			if code, reason := p.closed(t); code != websocket.StatusPolicyViolation || reason != "user_inactive" {
				t.Fatalf("closed %d %q, want %d user_inactive", code, reason, websocket.StatusPolicyViolation)
			}
			eventually(t, "the socket's goroutines end", func() bool { return e.srv.open() == 0 })
			if body := scrape(t, e); !strings.Contains(body, `rooms_connections_dropped_total{reason="user_inactive"} 1`) {
				t.Error("the cut is not counted")
			}
		})
	}
}
