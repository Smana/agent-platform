// SPDX-License-Identifier: Apache-2.0

package ghidentity_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/ghidentity"
)

func TestLoginFollowsTheGitHubLink(t *testing.T) {
	r := &ghidentity.Resolver{IDPID: "gh-idp", TTL: 5 * time.Minute, Now: time.Now,
		Links: func(_ context.Context, user string) ([]ghidentity.Link, error) {
			if user == "u1" {
				return []ghidentity.Link{{IDPID: "google-idp", UserID: "1"}, {IDPID: "gh-idp", UserID: "583231"}}, nil
			}
			return []ghidentity.Link{{IDPID: "google-idp", UserID: "2"}}, nil
		},
		LoginOf: func(_ context.Context, repo string, id int64) (string, error) {
			if id != 583231 || repo != "Smana/x" {
				t.Fatalf("resolved id %d for %q", id, repo)
			}
			return "octocat", nil
		}}
	if got, err := r.Login(context.Background(), "u1", "Smana/x"); err != nil || got != "octocat" {
		t.Fatalf("linked user: %q %v", got, err)
	}
	if got, err := r.Login(context.Background(), "u2", "Smana/x"); err != nil || got != "" {
		t.Fatalf("unlinked user: %q %v", got, err)
	}
}

func TestOnlyTheConfiguredIdPCounts(t *testing.T) {
	r := &ghidentity.Resolver{IDPID: "gh-idp", TTL: time.Minute, Now: time.Now,
		Links: func(context.Context, string) ([]ghidentity.Link, error) {
			return []ghidentity.Link{{IDPID: "other-oauth-idp", UserID: "583231"}}, nil
		},
		LoginOf: func(context.Context, string, int64) (string, error) { return "octocat", nil }}
	if got, _ := r.Login(context.Background(), "u1", "Smana/x"); got != "" {
		t.Fatalf("a non-GitHub IdP's link was trusted: %q", got)
	}
}

func TestLoginFailsClosedPastTheCache(t *testing.T) {
	now := time.Unix(0, 0)
	fail := false
	r := &ghidentity.Resolver{IDPID: "gh-idp", TTL: 5 * time.Minute, Now: func() time.Time { return now },
		Links: func(context.Context, string) ([]ghidentity.Link, error) {
			if fail {
				return nil, errors.New("zitadel down")
			}
			return []ghidentity.Link{{IDPID: "gh-idp", UserID: "583231"}}, nil
		},
		LoginOf: func(context.Context, string, int64) (string, error) { return "octocat", nil }}
	if got, _ := r.Login(context.Background(), "u1", "Smana/x"); got != "octocat" {
		t.Fatal("first resolve")
	}
	fail = true
	now = now.Add(4 * time.Minute)
	if got, err := r.Login(context.Background(), "u1", "Smana/x"); got != "octocat" || err != nil {
		t.Fatalf("a fresh cache must stand: %q %v", got, err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := r.Login(context.Background(), "u1", "Smana/x"); err == nil {
		t.Fatal("must fail closed past the TTL")
	}
}

func TestAFailedGitHubLookupIsNotCached(t *testing.T) {
	calls, down := 0, true
	r := &ghidentity.Resolver{IDPID: "gh-idp", TTL: time.Minute, Now: time.Now,
		Links: func(context.Context, string) ([]ghidentity.Link, error) {
			return []ghidentity.Link{{IDPID: "gh-idp", UserID: "583231"}}, nil
		},
		LoginOf: func(context.Context, string, int64) (string, error) {
			calls++
			if down {
				return "", errors.New("github down")
			}
			return "octocat", nil
		}}
	if got, err := r.Login(context.Background(), "u1", "Smana/x"); err == nil || got != "" {
		t.Fatalf("a GitHub failure must be an error: %q %v", got, err)
	}
	down = false
	if got, err := r.Login(context.Background(), "u1", "Smana/x"); err != nil || got != "octocat" || calls != 2 {
		t.Fatalf("the failure was cached: %q %v after %d calls", got, err, calls)
	}
}

func TestANonNumericLinkIdIsAnError(t *testing.T) {
	for _, id := range []string{"octocat", "", "0", "-5"} {
		r := &ghidentity.Resolver{IDPID: "gh-idp", TTL: time.Minute, Now: time.Now,
			Links: func(context.Context, string) ([]ghidentity.Link, error) {
				return []ghidentity.Link{{IDPID: "gh-idp", UserID: id}}, nil
			},
			LoginOf: func(context.Context, string, int64) (string, error) { return "octocat", nil }}
		if got, err := r.Login(context.Background(), "u1", "Smana/x"); err == nil || got != "" {
			t.Fatalf("id %q: %q %v", id, got, err)
		}
	}
}

func TestTheCacheIsBoundedAndEvictsTheOldest(t *testing.T) {
	now := time.Unix(0, 0)
	calls := map[string]int{}
	r := &ghidentity.Resolver{IDPID: "gh-idp", TTL: time.Hour, Now: func() time.Time { return now },
		Links: func(_ context.Context, u string) ([]ghidentity.Link, error) {
			calls[u]++
			return nil, nil
		}}
	ghidentity.SetMaxEntries(r, 3)
	for i := range 4 {
		now = now.Add(time.Second)
		if _, err := r.Login(context.Background(), "u"+strconv.Itoa(i), "Smana/x"); err != nil {
			t.Fatal(err)
		}
	}
	if n := ghidentity.Len(r); n != 3 {
		t.Fatalf("cache holds %d entries, cap is 3", n)
	}
	_, _ = r.Login(context.Background(), "u3", "Smana/x") // newest: still cached
	_, _ = r.Login(context.Background(), "u0", "Smana/x") // oldest: evicted, looked up again
	if calls["u3"] != 1 || calls["u0"] != 2 {
		t.Fatalf("lookups: %v", calls)
	}
}
