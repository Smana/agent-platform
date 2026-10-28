// SPDX-License-Identifier: Apache-2.0

package ghidentity_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Smana/maku/internal/ghidentity"
)

// stateWorld is one ZITADEL user whose state the test flips, counting state reads.
type stateWorld struct {
	active, down bool
	reads        int
	now          time.Time
}

func (w *stateWorld) resolver() *ghidentity.Resolver {
	return &ghidentity.Resolver{IDPID: "gh-idp", TTL: 5 * time.Minute, Now: func() time.Time { return w.now },
		Active: func(context.Context, string) (bool, error) {
			w.reads++
			if w.down {
				return false, errors.New("zitadel down")
			}
			return w.active, nil
		},
		Links: func(context.Context, string) ([]ghidentity.Link, error) {
			return []ghidentity.Link{{IDPID: "gh-idp", UserID: "583231"}}, nil
		},
		LoginOf: func(context.Context, string, int64) (string, error) { return "octocat", nil }}
}

// #126: a deactivated user is refused once the cached active answer is a TTL old, and a younger
// one still serves.
func TestADeactivatedUserIsRefusedPastTheTTL(t *testing.T) {
	w := &stateWorld{active: true, now: time.Unix(0, 0)}
	r := w.resolver()
	ctx := context.Background()
	if got, err := r.Login(ctx, "u1", "Smana/x"); err != nil || got != "octocat" {
		t.Fatalf("an active user: %q %v", got, err)
	}
	w.active, w.now = false, w.now.Add(4*time.Minute)
	if got, err := r.Login(ctx, "u1", "Smana/x"); err != nil || got != "octocat" {
		t.Fatalf("a cached active answer younger than the TTL must serve: %q %v", got, err)
	}
	if err := r.CheckActive(ctx, "u1"); err != nil || w.reads != 1 {
		t.Fatalf("the state is cached with the links: %v after %d reads", err, w.reads)
	}
	w.now = w.now.Add(2 * time.Minute)
	if _, err := r.Login(ctx, "u1", "Smana/x"); !errors.Is(err, ghidentity.ErrInactive) {
		t.Fatalf("past the TTL: %v, want ErrInactive", err)
	}
	if err := r.CheckActive(ctx, "u1"); !errors.Is(err, ghidentity.ErrInactive) || w.reads != 2 {
		t.Fatalf("an inactive answer is cached too: %v after %d reads", err, w.reads)
	}
}

func TestAStateReadErrorFailsClosed(t *testing.T) {
	w := &stateWorld{active: true, down: true, now: time.Unix(0, 0)}
	r := w.resolver()
	err := r.CheckActive(context.Background(), "u1")
	if err == nil || errors.Is(err, ghidentity.ErrInactive) {
		t.Fatalf("a ZITADEL error: %v, want an error that is not ErrInactive", err)
	}
	if _, err := r.Login(context.Background(), "u1", "Smana/x"); err == nil {
		t.Fatal("Login must fail closed on a state error")
	}
	w.down = false
	if err := r.CheckActive(context.Background(), "u1"); err != nil || w.reads != 3 {
		t.Fatalf("the failure was cached: %v after %d reads", err, w.reads)
	}
}

func TestZitadelActiveReadsTheUsersState(t *testing.T) {
	for _, tc := range []struct {
		code   int
		body   string
		active bool
		err    bool
	}{
		{200, `{"user":{"userId":"u1","state":"USER_STATE_ACTIVE"}}`, true, false},
		{200, `{"user":{"userId":"u1","state":"USER_STATE_INACTIVE"}}`, false, false},
		{200, `{"user":{"userId":"u1","state":"USER_STATE_LOCKED"}}`, false, false},
		{200, `{"user":{"userId":"u1","state":"USER_STATE_INITIAL"}}`, false, false},
		{200, `{"user":{"userId":"u1","state":"USER_STATE_DELETED"}}`, false, false},
		{200, `{"user":{"userId":"u1"}}`, false, false},
		{404, `{"code":5,"message":"User could not be found"}`, false, false}, // deleted
		{500, `boom`, false, true},
		{403, `no`, false, true},
		{200, `not json`, false, true},
	} {
		var path, auth, method string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path, auth, method = r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Method
			w.WriteHeader(tc.code)
			_, _ = w.Write([]byte(tc.body))
		}))
		active, err := ghidentity.ZitadelActive(srv.Client(), srv.URL+"/", pat)(context.Background(), "a/b")
		srv.Close()
		if active != tc.active || (err != nil) != tc.err {
			t.Errorf("%d %s: %v %v", tc.code, tc.body, active, err)
		}
		if err != nil && strings.Contains(err.Error(), pat) {
			t.Errorf("the error leaks the PAT: %v", err)
		}
		if method != http.MethodGet || path != "/v2/users/a%2Fb" || auth != "Bearer "+pat {
			t.Fatalf("request: %s %s %q", method, path, auth)
		}
	}
}
