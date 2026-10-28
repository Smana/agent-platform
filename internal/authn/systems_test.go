// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
)

func TestSystemsWithoutIssuerRefuses(t *testing.T) {
	for name, bearer := range map[string]string{"an arbitrary bearer": "Bearer x", "no bearer": ""} {
		req := httptest.NewRequestWithContext(context.Background(), "GET", "/", nil)
		if bearer != "" {
			req.Header.Set("Authorization", bearer)
		}
		if _, err := NewSystems(nil, nil).Authenticate(req); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("%s: want ErrUnauthenticated, got %v", name, err)
		}
	}
}
