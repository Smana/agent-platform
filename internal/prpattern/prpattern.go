// SPDX-License-Identifier: Apache-2.0

// Package prpattern matches a repository's GitHub pull-request URLs. A Cache
// compiles each repository's patterns once, for a bounded number of
// repositories.
package prpattern

import (
	"regexp"
	"sync"
)

// maxRepositories bounds a Cache. Repositories come from Room and AgentRun
// specs, an open set: past the bound, a repository's patterns are compiled per
// call and not kept.
const maxRepositories = 256

// Set is one repository's pull-request patterns.
type Set struct {
	mention *regexp.Regexp // a PR URL inside text, its number 1 to 10 digits
	url     *regexp.Regexp // exactly a PR URL, its number 1 to 10 digits
	task    *regexp.Regexp // exactly a PR URL, its number with no leading zero
}

// Mention is the first pull request of the repository text names, or "".
func (s *Set) Mention(text string) string { return s.mention.FindString(text) }

// IsPR reports whether url is exactly a pull request of the repository.
func (s *Set) IsPR(url string) bool { return s.url.MatchString(url) }

// IsTask reports whether url is exactly a pull request of the repository whose
// number has no leading zero: a run's task URL.
func (s *Set) IsTask(url string) bool { return s.task.MatchString(url) }

// Cache holds compiled Sets by repository. The zero value is ready to use.
type Cache struct {
	mu   sync.Mutex
	sets map[string]*Set
}

// For is the repository's Set, compiled on first use.
func (c *Cache) For(repository string) *Set {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.sets[repository]; ok {
		return s
	}
	s := compile(repository)
	if len(c.sets) < maxRepositories {
		if c.sets == nil {
			c.sets = map[string]*Set{}
		}
		c.sets[repository] = s
	}
	return s
}

func compile(repository string) *Set {
	// An empty repository matches nothing: no pull request URL has "//pull/".
	base := `https://github\.com/` + regexp.QuoteMeta(repository) + `/pull/`
	return &Set{
		mention: regexp.MustCompile(base + `[0-9]{1,10}\b`),
		url:     regexp.MustCompile(`^` + base + `[0-9]{1,10}$`),
		task:    regexp.MustCompile(`^` + base + `[1-9][0-9]*$`),
	}
}
