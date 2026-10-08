// SPDX-License-Identifier: Apache-2.0

// Package ghidentity resolves a ZITADEL user to the GitHub login they linked (spec D7). The source
// is the user's GitHub IdP link, never a token claim: a link is added only by authenticating at
// GitHub, while user metadata is writable by machine users and user.write holders.
package ghidentity

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Link is one of a ZITADEL user's IdP links. UserID is the external id: for GitHub, the numeric
// user id. The link's userName is deliberately absent; it is stale after a rename.
type Link struct{ IDPID, UserID string }

// Resolver maps a ZITADEL user to a GitHub login.
type Resolver struct {
	Links   func(ctx context.Context, zitadelUser string) ([]Link, error)
	LoginOf func(ctx context.Context, repo string, githubID int64) (string, error)
	IDPID   string
	TTL     time.Duration
	Now     func() time.Time

	mu         sync.Mutex
	cache      map[string]entry
	maxEntries int // 0 means defaultMaxEntries
}

type entry struct {
	login string
	at    time.Time
}

// defaultMaxEntries bounds the cache; when it is full the oldest entry is dropped.
const defaultMaxEntries = 10_000

// Login is sub's current GitHub login, "" when sub has no link to the GitHub IdP. A cached answer
// stands for TTL; past it a failure is an error and nothing is cached, so the caller fails closed.
func (r *Resolver) Login(ctx context.Context, sub, repo string) (string, error) {
	if sub == "" || r.IDPID == "" {
		return "", nil
	}
	r.mu.Lock()
	e, hit := r.cache[sub]
	r.mu.Unlock()
	if hit && r.Now().Sub(e.at) < r.TTL {
		return e.login, nil
	}
	links, err := r.Links(ctx, sub)
	if err != nil {
		return "", fmt.Errorf("ghidentity: cannot read %s's links: %w", sub, err)
	}
	login := ""
	for _, l := range links {
		if l.IDPID != r.IDPID {
			continue
		}
		id, err := strconv.ParseInt(l.UserID, 10, 64)
		if err != nil || id <= 0 {
			return "", fmt.Errorf("ghidentity: GitHub link of %s holds %q, not a numeric id", sub, l.UserID)
		}
		if login, err = r.LoginOf(ctx, repo, id); err != nil {
			return "", fmt.Errorf("ghidentity: cannot resolve GitHub id %d: %w", id, err)
		}
		break
	}
	r.put(sub, entry{login: login, at: r.Now()})
	return login, nil
}

// put stores e, first dropping the oldest entry if sub would grow a full cache.
func (r *Resolver) put(sub string, e entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil {
		r.cache = map[string]entry{}
	}
	limit := r.maxEntries
	if limit == 0 {
		limit = defaultMaxEntries
	}
	if _, known := r.cache[sub]; !known && len(r.cache) >= limit {
		oldest, first := "", true
		var at time.Time
		for k, v := range r.cache {
			if first || v.at.Before(at) {
				oldest, at, first = k, v.at, false
			}
		}
		delete(r.cache, oldest)
	}
	r.cache[sub] = e
}
