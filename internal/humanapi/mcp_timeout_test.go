// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestMCPTimeoutIsUnderTheProxys(t *testing.T) {
	if mcpTimeout >= 30*time.Second {
		t.Fatalf("mcpTimeout %v is not under oauth2-proxy's 30 s", mcpTimeout)
	}
	ctx, cancel := mcpContext(httptest.NewRequestWithContext(t.Context(), "POST", "/mcp", nil))
	defer cancel()
	d, ok := ctx.Deadline()
	if left := time.Until(d); !ok || left > mcpTimeout || left < mcpTimeout-time.Second {
		t.Fatalf("deadline in %v, want about %v", left, mcpTimeout)
	}
}
