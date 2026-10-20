// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
)

func TestSystemsWithoutIssuerRefuses(t *testing.T) {
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/", nil)
	req.Header.Set("Authorization", "Bearer x")
	if _, err := NewSystems(nil, nil).Authenticate(req); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("want ErrUnauthenticated, got %v", err)
	}
}
