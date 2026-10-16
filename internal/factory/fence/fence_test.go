// SPDX-License-Identifier: Apache-2.0

package fence

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	for name, tc := range map[string]struct {
		in, want string
		ok       bool
	}{
		"one factory":    {"factory: aws-0\n", "aws-0", true},
		"a comment":      {"# owner: platform\nfactory: gcp-0\n", "gcp-0", true},
		"empty":          {"", "", false},
		"no factory key": {"owner: aws-0\n", "", false},
		"an unknown key": {"factory: aws-0\nalso: gcp-0\n", "", false},
		"a list":         {"factory: [aws-0, gcp-0]\n", "", false},
		"not a name":     {"factory: AWS 0\n", "", false},
		"an empty name":  {"factory: \"\"\n", "", false},
		"not yaml":       {"factory: [\n", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Parse([]byte(tc.in))
			if (err == nil) != tc.ok || got != tc.want {
				t.Fatalf("%q %v", got, err)
			}
		})
	}
}

// file is one repository's fence file as GitHub would answer it.
type file struct {
	body  string
	found bool
	err   error
}

type files struct {
	mu sync.Mutex
	by map[string]file
}

func (f *files) set(repo string, v file) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.by[repo] = v
}

func (f *files) read(_ context.Context, repo string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.by[repo]
	if !ok {
		return nil, false, errors.New("no connection")
	}
	return []byte(v.body), v.found, v.err
}

// D7: a repository is this factory's only when its default branch names this factory. Absent,
// unreadable or naming another, it is not; a read that fails keeps the last answer until it is
// Stale, then it is not either.
func TestOwnership(t *testing.T) {
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	fs := &files{by: map[string]file{
		"Smana/mine":    {"factory: aws-0\n", true, nil},
		"Smana/theirs":  {"factory: gcp-0\n", true, nil},
		"Smana/absent":  {"", false, nil},
		"Smana/garbled": {"factory: [\n", true, nil},
	}}
	var recorded sync.Map
	f := &Fence{Name: "aws-0", Read: fs.read, Repositories: []string{"Smana/mine", "Smana/theirs", "Smana/absent", "Smana/garbled", "Smana/unwired"},
		Stale: 15 * time.Minute, Now: func() time.Time { return now },
		OnState: func(_ context.Context, repo string, owned bool) { recorded.Store(repo, owned) }}
	if f.Owned("Smana/mine") {
		t.Fatal("owned before the first read")
	}
	_ = f.Refresh(t.Context())
	for repo, want := range map[string]bool{"Smana/mine": true, "smana/MINE": true, "Smana/theirs": false, "Smana/absent": false,
		"Smana/garbled": false, "Smana/unwired": false, "Smana/unlisted": false} {
		if got := f.Owned(repo); got != want {
			t.Errorf("%s: owned %v, want %v", repo, got, want)
		}
	}
	if v, _ := recorded.Load("Smana/mine"); v != true {
		t.Fatal("the gauge hears the state")
	}
	if v, _ := recorded.Load("Smana/theirs"); v != false {
		t.Fatal("the gauge hears an unowned repository")
	}
	fs.set("Smana/mine", file{err: errors.New("github down")})
	now = now.Add(10 * time.Minute)
	_ = f.Refresh(t.Context())
	if !f.Owned("Smana/mine") {
		t.Fatal("a failed read keeps a fresh answer")
	}
	now = now.Add(6 * time.Minute)
	if f.Owned("Smana/mine") {
		t.Fatal("past Stale with no fresh read, the repository is not owned")
	}
	fs.set("Smana/mine", file{"factory: gcp-0\n", true, nil})
	_ = f.Refresh(t.Context())
	if f.Owned("Smana/mine") {
		t.Fatal("the fence moved to another factory")
	}
}
