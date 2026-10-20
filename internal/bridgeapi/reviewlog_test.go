// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/authn"
)

type failingAuth struct{ err error }

func (a failingAuth) Authenticate(*http.Request) (authn.Principal, error) {
	return authn.Principal{}, a.err
}

// A TokenReview call that fails is the broker's problem, not the credential's: Warn with the cause,
// once a minute per cause, and never the token. A bad credential stays at Debug.
func TestAReviewFailureIsWarnedOnceAMinuteAndABadTokenIsNot(t *testing.T) {
	var buf bytes.Buffer
	now := time.Unix(1_800_000_000, 0)
	s := &Server{Logger: slog.New(slog.NewTextHandler(&buf, nil)), Now: func() time.Time { return now }}
	req := func(a failingAuth) {
		s.Runs = a
		rec := call(t, s.Routes(), http.MethodPost, "/v1/bridge/hello", "secret-token", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status %d, want 401", rec.Code)
		}
	}
	cause := fmt.Errorf("%w: tokenreviews is forbidden", authn.ErrReviewUnavailable)
	req(failingAuth{cause})
	req(failingAuth{cause})
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Fatalf("%d warnings for one cause, want 1:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "tokenreviews is forbidden") || strings.Contains(buf.String(), "secret-token") {
		t.Fatalf("warning lacks the cause or leaks the token:\n%s", buf.String())
	}
	now = now.Add(2 * warnEvery)
	req(failingAuth{cause})
	if n := strings.Count(buf.String(), "level=WARN"); n != 2 {
		t.Fatalf("%d warnings after the window, want 2", n)
	}
	buf.Reset()
	req(failingAuth{authn.ErrWrongAudience})
	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("a bad credential must not warn:\n%s", buf.String())
	}
}
