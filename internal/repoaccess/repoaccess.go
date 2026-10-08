// SPDX-License-Identifier: Apache-2.0

// Package repoaccess answers "may this GitHub login read this repository?" for room visibility
// (spec D7): a room is never more visible than its repository.
package repoaccess

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Checker asks GitHub for a login's permission on a repository and caches the answer for TTL.
// Perm, TTL and Now are required.
type Checker struct {
	Perm func(ctx context.Context, owner, repo, login string) (string, error)
	TTL  time.Duration
	Now  func() time.Time

	mu         sync.Mutex
	cache      map[string]entry
	maxEntries int // 0 means defaultMaxEntries
}

type entry struct {
	read bool
	at   time.Time
}

// defaultMaxEntries bounds the cache; when it is full the oldest entry is dropped.
const defaultMaxEntries = 10_000

// readable are GitHub's permissions that include read. "none", "" and anything unknown are not.
var readable = map[string]bool{"admin": true, "maintain": true, "write": true, "triage": true, "read": true}

// CanRead is true when login may read repository ("owner/name"). An answer is cached for TTL;
// when GitHub fails, a cached answer younger than TTL stands, otherwise it is an error and the
// caller fails closed.
func (c *Checker) CanRead(ctx context.Context, repository, login string) (bool, error) {
	owner, repo, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || repo == "" || login == "" {
		return false, nil
	}
	// GitHub's owner, repository and login names are case-insensitive.
	key := strings.ToLower(repository + "\x00" + login)
	c.mu.Lock()
	e, hit := c.cache[key]
	c.mu.Unlock()
	if hit && c.Now().Sub(e.at) < c.TTL {
		return e.read, nil
	}
	perm, err := c.Perm(ctx, owner, repo, login)
	if err != nil {
		return false, fmt.Errorf("repoaccess: cannot verify %s's access to %s: %w", login, repository, err)
	}
	e = entry{read: readable[perm], at: c.Now()}
	c.put(key, e)
	return e.read, nil
}

// put stores e, first dropping the oldest entry if key would grow a full cache.
func (c *Checker) put(key string, e entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache = map[string]entry{}
	}
	limit := c.maxEntries
	if limit == 0 {
		limit = defaultMaxEntries
	}
	if _, known := c.cache[key]; !known && len(c.cache) >= limit {
		oldest, first := "", true
		var at time.Time
		for k, v := range c.cache {
			if first || v.at.Before(at) {
				oldest, at, first = k, v.at, false
			}
		}
		delete(c.cache, oldest)
	}
	c.cache[key] = e
}
