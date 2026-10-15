// SPDX-License-Identifier: Apache-2.0

package prpattern

import (
	"strconv"
	"testing"
)

func TestCache(t *testing.T) {
	t.Run("compiles a repository once", func(t *testing.T) {
		var c Cache
		first := c.For("Smana/ref")
		if c.For("Smana/ref") != first {
			t.Fatal("a second lookup compiled the repository again")
		}
	})
	t.Run("keeps at most maxRepositories and still matches past them", func(t *testing.T) {
		var c Cache
		for i := range maxRepositories + 10 {
			c.For("o/r" + strconv.Itoa(i))
		}
		if len(c.sets) != maxRepositories {
			t.Fatalf("kept %d repositories, want %d", len(c.sets), maxRepositories)
		}
		past := "o/r" + strconv.Itoa(maxRepositories+5)
		if !c.For(past).IsPR("https://github.com/" + past + "/pull/1") {
			t.Fatal("a repository past the bound does not match")
		}
	})
	t.Run("the patterns", func(t *testing.T) {
		var c Cache
		s := c.For("Smana/ref")
		for _, tc := range []struct {
			url        string
			isPR, task bool
		}{
			{"https://github.com/Smana/ref/pull/12", true, true},
			{"https://github.com/Smana/ref/pull/012", true, false},
			{"https://github.com/Smana/ref/pull/12345678901", false, true},
			{"https://github.com/Smana/ref/pull/12/files", false, false},
			{"https://github.com/Smana/refx/pull/12", false, false},
		} {
			if s.IsPR(tc.url) != tc.isPR || s.IsTask(tc.url) != tc.task {
				t.Errorf("%s: IsPR %v IsTask %v, want %v %v", tc.url, s.IsPR(tc.url), s.IsTask(tc.url), tc.isPR, tc.task)
			}
		}
		if got := s.Mention("see https://github.com/Smana/ref/pull/7, done"); got != "https://github.com/Smana/ref/pull/7" {
			t.Errorf("Mention = %q", got)
		}
	})
	t.Run("an empty repository matches no pull request", func(t *testing.T) {
		var c Cache
		s := c.For("")
		const url = "https://github.com//pull/1"
		if s.IsPR(url) || s.IsTask(url) || s.Mention("see "+url) != "" {
			t.Fatalf("repository \"\" matched %s", url)
		}
	})
}
