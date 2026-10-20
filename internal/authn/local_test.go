// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLocal(t *testing.T) {
	l := NewLocal("http://localhost:8080", []string{"admin", "member"})
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/rooms", nil)
	r.Host = "localhost:8080"
	p, err := l.Authenticate(r)
	if err != nil || p.ID != LocalPrincipal || len(p.Groups) != 2 || p.ClientID != LocalClientID || !p.Expiry.After(time.Now()) {
		t.Fatalf("no Origin (curl, roomctl): %+v %v", p, err)
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
