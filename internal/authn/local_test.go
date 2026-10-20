// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocal(t *testing.T) {
	l := NewLocal("http://localhost:8080", []string{"admin", "member"})
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/rooms", nil)
	p, err := l.Authenticate(r)
	if err != nil || p.ID != LocalPrincipal || len(p.Groups) != 2 {
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
