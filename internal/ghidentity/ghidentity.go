// SPDX-License-Identifier: Apache-2.0

// Package ghidentity resolves a ZITADEL user to the GitHub login they linked (spec D7). The source
// is the user's GitHub IdP link, never a token claim: a link is added only by authenticating at
// GitHub, while user metadata is writable by machine users and user.write holders. It also says
// whether the ZITADEL user is still active (#126): the broker verifies bearers offline, so only
// this read refuses a user deactivated after their token was minted.
package ghidentity

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Link is one of a ZITADEL user's IdP links. UserID is the external id: for GitHub, the numeric
// user id. The link's userName is deliberately absent; it is stale after a rename.
type Link struct{ IDPID, UserID string }

// ErrInactive is a ZITADEL user that is not active: inactive, locked, deleted or initial.
var ErrInactive = errors.New("ghidentity: the user is not active")

// Resolver maps a ZITADEL user to a GitHub login.
type Resolver struct {
	// Active reports whether the ZITADEL user is active; nil skips the check.
	Active  func(ctx context.Context, zitadelUser string) (bool, error)
	Links   func(ctx context.Context, zitadelUser string) ([]Link, error)
	LoginOf func(ctx context.Context, repo string, githubID int64) (string, error)
	IDPID   string
	TTL     time.Duration
	Now     func() time.Time

	mu         sync.Mutex
	cache      map[string]entry
	maxEntries int // 0 means defaultMaxEntries
}

// entry caches a user's answers, each with the time it was read; a zero time is not read yet.
type entry struct {
	login   string
	at      time.Time // of login
	active  bool
	stateAt time.Time
}

// defaultMaxEntries bounds the cache; when it is full the oldest entry is dropped.
const defaultMaxEntries = 10_000

// CheckActive is nil when sub is an active ZITADEL user, ErrInactive when it is not, and another
// error when ZITADEL cannot say. Both answers are cached for TTL; a failure is not, so the caller
// fails closed.
func (r *Resolver) CheckActive(ctx context.Context, sub string) error {
	if r.Active == nil {
		return nil
	}
	if sub == "" {
		return ErrInactive
	}
	now := r.Now()
	r.mu.Lock()
	e := r.cache[sub]
	r.mu.Unlock()
	if e.stateAt.IsZero() || now.Sub(e.stateAt) >= r.TTL {
		active, err := r.Active(ctx, sub)
		if err != nil {
			return fmt.Errorf("ghidentity: cannot read %s's state: %w", sub, err)
		}
		r.update(sub, func(e *entry) { e.active, e.stateAt = active, now })
		e.active = active
	}
	if !e.active {
		return ErrInactive
	}
	return nil
}

// Login is sub's current GitHub login, "" when sub has no link to the GitHub IdP, or ErrInactive
// when sub is not an active user. A cached answer stands for TTL; past it a failure is an error
// and nothing is cached, so the caller fails closed.
func (r *Resolver) Login(ctx context.Context, sub, repo string) (string, error) {
	if sub == "" || r.IDPID == "" {
		return "", nil
	}
	if err := r.CheckActive(ctx, sub); err != nil {
		return "", err
	}
	r.mu.Lock()
	e := r.cache[sub]
	r.mu.Unlock()
	if !e.at.IsZero() && r.Now().Sub(e.at) < r.TTL {
		return e.login, nil
	}
	links, err := r.Links(ctx, sub)
	if err != nil {
		return "", fmt.Errorf("ghidentity: cannot read %s's links: %w", sub, err)
	}
	// ZITADEL does not order the links or limit a user to one per IdP, so every match counts and
	// two distinct ids are ambiguous: fail closed rather than pick one.
	var id int64
	for _, l := range links {
		if l.IDPID != r.IDPID {
			continue
		}
		n, err := strconv.ParseInt(l.UserID, 10, 64)
		if err != nil || n <= 0 {
			return "", fmt.Errorf("ghidentity: GitHub link of %s holds %q, not a numeric id", sub, l.UserID)
		}
		if id != 0 && n != id {
			return "", fmt.Errorf("ghidentity: %s has several GitHub links", sub)
		}
		id = n
	}
	login := ""
	if id != 0 {
		if login, err = r.LoginOf(ctx, repo, id); err != nil {
			return "", fmt.Errorf("ghidentity: cannot resolve GitHub id %d: %w", id, err)
		}
	}
	at := r.Now()
	r.update(sub, func(e *entry) { e.login, e.at = login, at })
	return login, nil
}

// update applies fn to sub's entry, first dropping the oldest entry if sub would grow a full cache.
func (r *Resolver) update(sub string, fn func(*entry)) {
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
			if t := v.read(); first || t.Before(at) {
				oldest, at, first = k, t, false
			}
		}
		delete(r.cache, oldest)
	}
	e := r.cache[sub]
	fn(&e)
	r.cache[sub] = e
}

// read is when the entry's latest answer was read.
func (e entry) read() time.Time {
	if e.stateAt.After(e.at) {
		return e.stateAt
	}
	return e.at
}
