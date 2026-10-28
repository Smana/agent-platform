// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"

	"github.com/Smana/maku/internal/authn"
)

type logins map[string]string // sub → GitHub login

func (l logins) Login(_ context.Context, sub, _ string) (string, error) {
	if sub == "broken" {
		return "", errors.New("zitadel down")
	}
	return l[sub], nil
}

type perms map[string]bool // login → write

func (p perms) CanWrite(_ context.Context, _, login string) (bool, error) {
	if login == "flaky" {
		return false, errors.New("github down")
	}
	return p[login], nil
}

// D4's lookup: the factory resolves the token's sub to a GitHub login itself, then asks GitHub.
func TestGitHubAccessResolvesTheLoginThenAsksGitHub(t *testing.T) {
	a := GitHubAccess{Identity: logins{"291": "alice-gh", "292": "bob-gh", "293": "flaky"}, Perms: perms{"alice-gh": true}}
	for name, tc := range map[string]struct {
		sub    string
		ok     bool
		failed bool
	}{
		"a writer":                    {"291", true, false},
		"a reader":                    {"292", false, false},
		"no GitHub link":              {"999", false, false},
		"no sub":                      {"", false, false},
		"the login lookup fails":      {"broken", false, true},
		"the permission lookup fails": {"293", false, true},
	} {
		t.Run(name, func(t *testing.T) {
			ok, err := a.CanWrite(t.Context(), authn.Principal{Sub: tc.sub}, "Smana/x")
			if ok != tc.ok || (err != nil) != tc.failed {
				t.Fatalf("%v %v", ok, err)
			}
		})
	}
}

// D1: the person a token is charged as is the same login the write check resolved.
func TestGitHubAccessNamesThePerson(t *testing.T) {
	a := GitHubAccess{Identity: logins{"291": "alice-gh"}}
	for sub, want := range map[string]string{"291": "github:alice-gh", "999": "", "": ""} {
		if got, err := a.Person(t.Context(), authn.Principal{Sub: sub}, "Smana/x"); err != nil || got != want {
			t.Fatalf("%q: %q %v", sub, got, err)
		}
	}
	if _, err := a.Person(t.Context(), authn.Principal{Sub: "broken"}, "Smana/x"); err == nil {
		t.Fatal("a failed lookup is no answer")
	}
}
