// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

func TestLocal(t *testing.T) {
	l := NewLocal("http://localhost:8080", []string{"admin", "member"})
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/rooms", nil)
	r.Host = "localhost:8080"
	p, err := l.Authenticate(r)
	if err != nil || p.ID != LocalPrincipal || len(p.Groups) != 2 || p.ClientID != LocalClientID || !p.Expiry.After(time.Now()) {
		t.Fatalf("no Origin (curl, maku): %+v %v", p, err)
	}
	r.Header.Set("Origin", "http://localhost:8080")
	if _, err := l.Authenticate(r); err != nil {
		t.Fatalf("own origin: %v", err)
	}
	r.Header.Set("Origin", "https://evil.example")
	if _, err := l.Authenticate(r); !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign origin must be ErrForbidden, got %v", err)
	}
}

func TestLocalForeignHost(t *testing.T) {
	l := NewLocal("http://localhost:8080", []string{"admin"})
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/rooms", nil)
	r.Host = "evil.example:8080" // DNS rebinding: no Origin on a same-origin GET, but its own Host
	if _, err := l.Authenticate(r); !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign host must be ErrForbidden, got %v", err)
	}
}

// A local session is the one human, whatever bearer it carries, and it lasts an hour: the UI
// reconnects on the broker's 4001 when the socket reaches it.
func TestLocalHumanAuthenticate(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	l := NewLocalWithClock("http://localhost:8080", []string{"admin", "member"}, func() time.Time { return now })
	for _, bearer := range []string{"", "Bearer bogus"} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/rooms", nil)
		r.Host = "localhost:8080"
		if bearer != "" {
			r.Header.Set("Authorization", bearer)
		}
		p, err := l.Authenticate(r)
		if err != nil {
			t.Fatalf("bearer %q: %v", bearer, err)
		}
		if p.ID != LocalPrincipal || p.Sub != "local" || p.ClientID != LocalClientID || p.AccessToken != "" ||
			!slices.Equal(p.Groups, []string{"admin", "member"}) || !p.Expiry.Equal(now.Add(time.Hour)) {
			t.Fatalf("bearer %q: %#v (expiry %s)", bearer, p, p.Expiry)
		}
	}
	for _, tc := range []struct {
		origin string
		want   error
	}{
		{"http://localhost:8080", nil},
		{"", nil},
		{"https://evil.example", ErrForbidden},
		{"null", ErrForbidden},
		{"http://localhost:8081", ErrForbidden},
	} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/rooms", nil)
		r.Host = "localhost:8080"
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		_, err := l.Authenticate(r)
		if tc.want == nil && err != nil {
			t.Errorf("origin %q: %v", tc.origin, err)
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("origin %q: %v, want %v", tc.origin, err, tc.want)
		}
	}
}

func TestLoopbackOrigins(t *testing.T) {
	for _, tc := range []struct {
		public string
		want   []string
	}{
		{"http://localhost:8080", []string{"http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"}},
		{"http://127.0.0.1:18080", []string{"http://127.0.0.1:18080", "http://localhost:18080", "http://[::1]:18080"}},
		{"http://LOCALHOST:8080", []string{"http://LOCALHOST:8080", "http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"}},
		{"https://rooms.example", []string{"https://rooms.example"}},
	} {
		if got := LoopbackOrigins(tc.public); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.public, got, tc.want)
		}
	}
}

// kubectl port-forward listens on 127.0.0.1 and [::1] and keeps the client's Host: the three
// loopback names on publicURL's port are the listener's own; anything else is a rebound page.
func TestLocalLoopbackAliases(t *testing.T) {
	l := NewLocal("http://localhost:8080", []string{"admin", "member"})
	for _, tc := range []struct {
		host, origin string
		want         error
	}{
		{"localhost:8080", "", nil},
		{"127.0.0.1:8080", "", nil},
		{"[::1]:8080", "", nil},
		{"LOCALHOST:8080", "", nil},
		{"127.0.0.1:8080", "http://127.0.0.1:8080", nil},
		{"[::1]:8080", "http://[::1]:8080", nil},
		{"localhost:8080", "http://127.0.0.1:8080", nil},
		{"evil.example:8080", "", ErrForbidden},
		{"localhost.evil.example:8080", "", ErrForbidden},
		{"127.0.0.1:8081", "", ErrForbidden},
		{"127.0.0.2:8080", "", ErrForbidden},
		{"127.0.0.1:8080", "https://127.0.0.1:8080", ErrForbidden},
		{"127.0.0.1:8080", "http://localhost:8081", ErrForbidden},
		{"127.0.0.1:8080", "http://evil.example", ErrForbidden},
	} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/rooms", nil)
		r.Host = tc.host
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		_, err := l.Authenticate(r)
		if (tc.want == nil) != (err == nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("host %q origin %q: %v, want %v", tc.host, tc.origin, err, tc.want)
		}
	}
}
