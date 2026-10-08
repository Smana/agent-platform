// SPDX-License-Identifier: Apache-2.0

package repoaccess_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/repoaccess"
)

func TestCanReadFollowsGitHub(t *testing.T) {
	perm := map[string]string{"dev1": "read", "dev2": "none", "dev3": "triage", "dev4": "maintain", "dev5": "admin", "dev6": "write"}
	c := &repoaccess.Checker{TTL: 5 * time.Minute, Now: time.Now,
		Perm: func(_ context.Context, _, _, login string) (string, error) { return perm[login], nil }}
	for login, want := range map[string]bool{"dev1": true, "dev2": false, "dev3": true, "dev4": true, "dev5": true, "dev6": true,
		"unknown": false} {
		if ok, err := c.CanRead(context.Background(), "Smana/x", login); ok != want || err != nil {
			t.Errorf("%s: %v, %v; want %v", login, ok, err, want)
		}
	}
	if ok, _ := c.CanRead(context.Background(), "Smana/x", ""); ok {
		t.Fatal("no login admitted")
	}
}

func TestCanReadAsksNothingForNoRepositoryOrNoLogin(t *testing.T) {
	asked := 0
	c := &repoaccess.Checker{TTL: 5 * time.Minute, Now: time.Now,
		Perm: func(context.Context, string, string, string) (string, error) { asked++; return "admin", nil }}
	for _, tc := range [][2]string{{"Smana/x", ""}, {"", "dev1"}, {"Smana", "dev1"}, {"/x", "dev1"}, {"Smana/", "dev1"}} {
		if ok, err := c.CanRead(context.Background(), tc[0], tc[1]); ok || err != nil {
			t.Errorf("%q %q: %v, %v", tc[0], tc[1], ok, err)
		}
	}
	if asked != 0 {
		t.Fatalf("GitHub asked %d times", asked)
	}
}

func TestCanReadFailsClosedPastTheCache(t *testing.T) {
	now := time.Unix(0, 0)
	fail := false
	c := &repoaccess.Checker{TTL: 5 * time.Minute, Now: func() time.Time { return now },
		Perm: func(context.Context, string, string, string) (string, error) {
			if fail {
				return "", errors.New("github down")
			}
			return "write", nil
		}}
	if ok, _ := c.CanRead(context.Background(), "Smana/x", "dev1"); !ok {
		t.Fatal("writer refused")
	}
	fail = true
	now = now.Add(4 * time.Minute)
	if ok, err := c.CanRead(context.Background(), "Smana/x", "dev1"); !ok || err != nil {
		t.Fatalf("a fresh cache must stand: %v %v", ok, err)
	}
	now = now.Add(2 * time.Minute)
	if ok, err := c.CanRead(context.Background(), "Smana/x", "dev1"); ok || err == nil {
		t.Fatal("must fail closed past the TTL")
	}
}

// A revoked read takes effect once the cached answer is TTL old, and an answer is never shared
// between logins or repositories, whatever their case.
func TestCanReadRefreshesAndKeysByRepoAndLogin(t *testing.T) {
	now := time.Unix(0, 0)
	perm := map[string]string{"Smana/x dev1": "read"}
	asked := 0
	c := &repoaccess.Checker{TTL: 5 * time.Minute, Now: func() time.Time { return now },
		Perm: func(_ context.Context, owner, repo, login string) (string, error) {
			asked++
			return perm[owner+"/"+repo+" "+login], nil
		}}
	ctx := context.Background()
	if ok, _ := c.CanRead(ctx, "Smana/x", "dev1"); !ok {
		t.Fatal("reader refused")
	}
	if ok, _ := c.CanRead(ctx, "Smana/y", "dev1"); ok {
		t.Fatal("another repository shares the answer")
	}
	if ok, _ := c.CanRead(ctx, "Smana/x", "dev2"); ok {
		t.Fatal("another login shares the answer")
	}
	if ok, _ := c.CanRead(ctx, "smana/X", "DEV1"); !ok || asked != 3 {
		t.Fatalf("GitHub's names are case-insensitive: %v after %d asks", ok, asked)
	}
	delete(perm, "Smana/x dev1")
	if ok, _ := c.CanRead(ctx, "Smana/x", "dev1"); !ok {
		t.Fatal("a fresh answer stands")
	}
	now = now.Add(5 * time.Minute)
	if ok, err := c.CanRead(ctx, "Smana/x", "dev1"); ok || err != nil {
		t.Fatalf("a revoked read must lapse with the cache: %v %v", ok, err)
	}
}

func TestCanReadBoundsItsCache(t *testing.T) {
	now := time.Unix(0, 0)
	asked := 0
	c := &repoaccess.Checker{TTL: 5 * time.Minute, Now: func() time.Time { return now },
		Perm: func(context.Context, string, string, string) (string, error) { asked++; return "read", nil }}
	repoaccess.SetMaxEntries(c, 2)
	ctx := context.Background()
	for _, login := range []string{"a", "b", "c"} {
		now = now.Add(time.Second)
		_, _ = c.CanRead(ctx, "Smana/x", login)
	}
	if n := repoaccess.Entries(c); n != 2 {
		t.Fatalf("%d entries, want the cap of 2", n)
	}
	_, _ = c.CanRead(ctx, "Smana/x", "c")
	_, _ = c.CanRead(ctx, "Smana/x", "b")
	if asked != 3 {
		t.Fatalf("the newest entries must survive: %d asks", asked)
	}
	_, _ = c.CanRead(ctx, "Smana/x", "a")
	if asked != 4 {
		t.Fatalf("the oldest entry must be the one dropped: %d asks", asked)
	}
}
