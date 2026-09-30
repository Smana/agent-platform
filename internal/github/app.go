// SPDX-License-Identifier: Apache-2.0

// Package github is the broker's GitHub App client (SP2 design §3, ruling P28). It posts
// an agent's review verdict as one pull request comment, as SP3's factory App.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Smana/agent-platform/internal/httpx"
)

const (
	// An App JWT lives at most ten minutes; it is backdated a minute for clock
	// drift and expires after nine, so the two stay inside GitHub's limit.
	jwtBackdate = time.Minute
	jwtLifetime = 9 * time.Minute
	// refreshBefore renews an installation token (one hour) while it still has this long to live.
	refreshBefore = 5 * time.Minute
	// perPage and maxPages bound the search for the App's comment: 3 000 comments.
	// A page of 30 comments of GitHub's 65 536 characters, JSON-escaped, stays under maxPage.
	perPage  = 30
	maxPages = 100
	maxPage  = 16 << 20
	maxReply = 64 << 10 // an App, installation, token or created-comment reply
)

// ErrTooManyComments reports a pull request with more comments than the client
// searches, so it cannot know whether the App already posted, and does not post.
var ErrTooManyComments = errors.New("github: too many comments to search for the marker")

// ErrNotAPullRequest reports a URL that is not a GitHub pull request's.
var ErrNotAPullRequest = errors.New("github: not a GitHub pull request URL")

// Permanent reports an error a retry cannot heal: GitHub's 4xx other than 429,
// a URL that names no pull request, or one too long to search. An unreadable
// key or a bad App id is not: the owner's Secret can still land or be fixed.
func Permanent(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Permanent()
	}
	return errors.Is(err, ErrNotAPullRequest) || errors.Is(err, ErrTooManyComments)
}

var prURL = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)/pull/([1-9][0-9]*)$`)

// APIError is a GitHub answer outside 2xx. It names the status and path only:
// a reply body can quote a credential. RateLimited marks a 403 or 429 that is
// GitHub's primary or secondary rate limit, and RetryAfter is when it lifts,
// when GitHub says.
type APIError struct {
	Status      int
	Path        string
	RateLimited bool
	RetryAfter  time.Duration
}

func (e *APIError) Error() string { return fmt.Sprintf("github %s: HTTP %d", e.Path, e.Status) }

// Permanent reports a 4xx other than a rate limit, which a retry does not heal
// (the App is not installed, the pull request is gone, the comment is refused).
func (e *APIError) Permanent() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != http.StatusTooManyRequests && !e.RateLimited
}

// apiError reads a refusal's rate-limit headers: Retry-After (seconds or an
// HTTP date) for the secondary limit, X-RateLimit-Remaining 0 and its reset for
// the primary one. Only a 403 or a 429 can be a rate limit.
func apiError(resp *http.Response, path string, now time.Time) *APIError {
	e := &APIError{Status: resp.StatusCode, Path: path}
	h := resp.Header
	if v := h.Get("Retry-After"); v != "" {
		if s, err := strconv.Atoi(v); err == nil && s >= 0 {
			e.RetryAfter = time.Duration(s) * time.Second
		} else if t, err := http.ParseTime(v); err == nil {
			e.RetryAfter = max(t.Sub(now), 0)
		}
		e.RateLimited = true
	}
	if h.Get("X-Ratelimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(h.Get("X-Ratelimit-Reset"), 10, 64); err == nil && e.RetryAfter == 0 {
			e.RetryAfter = max(time.Unix(reset, 0).Sub(now), 0)
		}
		e.RateLimited = true
	}
	e.RateLimited = e.RateLimited && (e.Status == http.StatusForbidden || e.Status == http.StatusTooManyRequests)
	return e
}

// App reads its id and key from Dir (app_id, private_key) at every mint. Dir is
// an optional Secret volume that kubelet fills once the owner has written the
// key (ruling P31), so the broker needs no restart and has no startup race. API
// is GitHub's https API base, HC the httpx egress client, Now the clock: all
// three are required.
type App struct {
	Dir string
	API string
	HC  *http.Client
	Now func() time.Time

	mu     sync.Mutex // held across a mint: concurrent callers wait for one
	slug   string
	tokens map[string]token // by owner/repo

	postMu  sync.Mutex
	posting map[string]*markerLock // by pull request + marker, while held or awaited
}

// markerLock serialises one marker's search and post; waiters counts its holder
// and those queued, so the last one out removes it.
type markerLock struct {
	sem     chan struct{}
	waiters int
}

// lockMarker holds key until unlock is called, or fails once ctx ends first.
// Search then post is not atomic on GitHub, so two calls for one marker would
// both find nothing and both post.
func (a *App) lockMarker(ctx context.Context, key string) (func(), error) {
	a.postMu.Lock()
	if a.posting == nil {
		a.posting = map[string]*markerLock{}
	}
	l := a.posting[key]
	if l == nil {
		l = &markerLock{sem: make(chan struct{}, 1)}
		a.posting[key] = l
	}
	l.waiters++
	a.postMu.Unlock()
	release := func() {
		a.postMu.Lock()
		if l.waiters--; l.waiters == 0 {
			delete(a.posting, key)
		}
		a.postMu.Unlock()
	}
	select {
	case l.sem <- struct{}{}:
		return func() { <-l.sem; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

type token struct {
	value string
	exp   time.Time
}

// Enabled reports whether the App's key has landed in Dir.
func (a *App) Enabled() bool {
	_, err := os.Stat(filepath.Join(a.Dir, "private_key"))
	return err == nil
}

// api is the validated API base: https, a host, no credentials.
func (a *App) api() (*url.URL, error) {
	u, err := url.Parse(a.API)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, errors.New("github: the API URL must be https://<host>/, without credentials")
	}
	if a.HC == nil || a.Now == nil {
		return nil, errors.New("github: an HTTP client and a clock are required")
	}
	return u, nil
}

// appJWT is the App JWT: RS256, issued by the App id.
func (a *App) appJWT() (string, error) {
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(a.Dir, "app_id")))
	if err != nil {
		return "", fmt.Errorf("github: app id: %w", err)
	}
	id, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || id < 1 {
		return "", errors.New("github: app id: not a positive integer")
	}
	pemKey, err := os.ReadFile(filepath.Clean(filepath.Join(a.Dir, "private_key")))
	if err != nil {
		return "", fmt.Errorf("github: private key: %w", err)
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM(pemKey)
	if err != nil {
		return "", errors.New("github: private key: not an RSA key in PEM") // never the parser's text: it may quote the file
	}
	now := a.Now()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer: strconv.FormatInt(id, 10), IssuedAt: jwt.NewNumericDate(now.Add(-jwtBackdate)),
		ExpiresAt: jwt.NewNumericDate(now.Add(jwtLifetime)),
	}).SignedString(key)
	if err != nil {
		return "", fmt.Errorf("github: sign the App JWT: %w", err)
	}
	return signed, nil
}

// do is one request to the API with bearer, decoding a 2xx reply of at most
// limit bytes into out.
func (a *App) do(ctx context.Context, api *url.URL, method, path, query, bearer string, in, out any, limit int64) error {
	var body bytes.Buffer
	if in != nil {
		if err := json.NewEncoder(&body).Encode(in); err != nil {
			return fmt.Errorf("github: encode: %w", err)
		}
	}
	u := api.JoinPath(path)
	u.RawQuery = query
	req, err := http.NewRequestWithContext(ctx, method, u.String(), &body)
	if err != nil {
		return fmt.Errorf("github: request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "room-broker")
	req.Header.Set("Authorization", "Bearer "+bearer)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.HC.Do(req)
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return apiError(resp, path, a.Now())
	}
	raw, err := httpx.ReadBody(resp.Body, limit)
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("github %s %s: the reply is not JSON", method, path)
	}
	return nil
}

// installation returns a token for one repository, scoped down to commenting,
// cached until refreshBefore its expiry, and the App's slug: the bot login
// `<slug>[bot]` marks the App's own comments.
func (a *App) installation(ctx context.Context, api *url.URL, owner, repo string) (string, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := owner + "/" + repo
	if t := a.tokens[key]; a.slug != "" && a.Now().Before(t.exp.Add(-refreshBefore)) {
		return t.value, a.slug, nil
	}
	appJWT, err := a.appJWT()
	if err != nil {
		return "", "", err
	}
	if a.slug == "" {
		var app struct {
			Slug string `json:"slug"`
		}
		if err := a.do(ctx, api, http.MethodGet, "app", "", appJWT, nil, &app, maxReply); err != nil {
			return "", "", err
		}
		if app.Slug == "" {
			return "", "", errors.New("github: the App has no slug")
		}
		a.slug = app.Slug
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	if err := a.do(ctx, api, http.MethodGet, "repos/"+owner+"/"+repo+"/installation", "", appJWT, nil, &inst, maxReply); err != nil {
		return "", "", err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	// The App holds the factory's permissions (SP3); this token gets only what a
	// verdict comment needs, on one repository (ruling P34).
	scope := map[string]any{"repositories": []string{repo},
		"permissions": map[string]string{"pull_requests": "write"}}
	if err := a.do(ctx, api, http.MethodPost, "app/installations/"+strconv.FormatInt(inst.ID, 10)+"/access_tokens", "",
		appJWT, scope, &out, maxReply); err != nil {
		return "", "", err
	}
	if out.Token == "" || !out.ExpiresAt.After(a.Now()) {
		return "", "", errors.New("github: the token reply has no live token")
	}
	if a.tokens == nil {
		a.tokens = map[string]token{}
	}
	a.tokens[key] = token{out.Token, out.ExpiresAt}
	return out.Token, a.slug, nil
}

// Comment posts body, then marker, on the pull request, once. The App's own
// comment that already ends with marker is returned instead: a retry after a
// crash, or a new leader. Another author's copy of the marker never counts.
// Calls for one marker run one at a time; other markers are not held up.
func (a *App) Comment(ctx context.Context, pr, marker, body string) (string, error) {
	m := prURL.FindStringSubmatch(pr)
	if m == nil || m[2] == "." || m[2] == ".." {
		return "", ErrNotAPullRequest
	}
	api, err := a.api()
	if err != nil {
		return "", err
	}
	unlock, err := a.lockMarker(ctx, pr+marker)
	if err != nil {
		return "", err
	}
	defer unlock()
	// The body never opens an HTML comment, so it cannot plant another verdict's marker.
	body = strings.ReplaceAll(body, "<!--", "&lt;!--")
	url, err := a.comment(ctx, api, m[1], m[2], m[3], marker, body)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
		// A revoked token, or a rotated key: one fresh token, one retry.
		a.evict(m[1] + "/" + m[2])
		url, err = a.comment(ctx, api, m[1], m[2], m[3], marker, body)
	}
	return url, err
}

// evict drops the cached installation token of owner/repo.
func (a *App) evict(repo string) {
	a.mu.Lock()
	delete(a.tokens, repo)
	a.mu.Unlock()
}

// comment is one attempt: a token, the search for the marker, then the post.
func (a *App) comment(ctx context.Context, api *url.URL, owner, repo, number, marker, body string) (string, error) {
	tok, slug, err := a.installation(ctx, api, owner, repo)
	if err != nil {
		return "", err
	}
	path := "repos/" + owner + "/" + repo + "/issues/" + number + "/comments"
	found, err := a.find(ctx, api, path, tok, slug+"[bot]", marker)
	if err != nil || found != "" {
		return found, err
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if err := a.do(ctx, api, http.MethodPost, path, "", tok, map[string]string{"body": body + "\n\n" + marker}, &out, maxReply); err != nil {
		return "", err
	}
	return out.HTMLURL, nil
}

// find is the html_url of login's comment ending with marker, or "". GitHub
// lists an issue's comments oldest first, so every page up to maxPages is read.
func (a *App) find(ctx context.Context, api *url.URL, path, tok, login, marker string) (string, error) {
	for page := 1; page <= maxPages; page++ {
		var cs []struct {
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
			User    struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		query := "per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page)
		if err := a.do(ctx, api, http.MethodGet, path, query, tok, nil, &cs, maxPage); err != nil {
			return "", err
		}
		for _, c := range cs {
			// The marker ends the App's comment (review M6).
			if c.User.Login == login && strings.HasSuffix(strings.TrimSpace(c.Body), marker) {
				return c.HTMLURL, nil
			}
		}
		if len(cs) < perPage {
			return "", nil
		}
	}
	return "", ErrTooManyComments
}
