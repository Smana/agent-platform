// SPDX-License-Identifier: Apache-2.0

package forge

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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Smana/agent-platform/internal/httpx"
)

const (
	// An App JWT lives at most ten minutes; it is backdated a minute for clock drift and
	// expires after nine, so the two together stay inside GitHub's limit.
	jwtBackdate = time.Minute
	jwtLifetime = 9 * time.Minute
	// refreshBefore renews an installation token (one hour) while it still has this long to live.
	refreshBefore = 5 * time.Minute
	// maxAuthReply bounds an installation or token reply: a few hundred bytes in practice.
	maxAuthReply = 64 << 10
)

// permissions is the factory App's installation-token scope: exactly what it holds (R16),
// on the one repository. A token asking for more fails to mint, which is the point.
func permissions() map[string]string {
	return map[string]string{"contents": "read", "issues": "write", "pull_requests": "write", "metadata": "read"}
}

// MergerPermissions is the merger App's installation-token scope (TW4, R16): the merge-side
// calls read checks and statuses, write contents on the merger's own revert branches, and arm
// and merge pull requests — and nothing else. Like the factory's, it is the App's ceiling: a
// mint asking for more fails.
func MergerPermissions() map[string]string {
	return map[string]string{"checks": "read", "statuses": "read", "contents": "write", "pull_requests": "write", "metadata": "read"}
}

// tokens mints and caches installation tokens for one App on one repository. The App id and key
// are mounted files read at each mint, so a rotation needs no restart (AGENTS.md).
type tokens struct {
	api                *url.URL
	hc                 *http.Client
	owner, name, agent string
	idFile, keyFile    string
	now                func() time.Time
	// perms is the connection's scope; nil is the factory App's (Options.Permissions).
	perms map[string]string

	mu           sync.Mutex
	token        string
	expires      time.Time
	installation int64
}

// get returns a token with more than refreshBefore to live, minting one when needed. Holding the
// lock across the mint makes concurrent callers wait for one mint instead of racing to many.
func (s *tokens) get(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && s.now().Before(s.expires.Add(-refreshBefore)) {
		return s.token, nil
	}
	appJWT, err := s.sign()
	if err != nil {
		return "", err
	}
	if s.installation == 0 {
		if s.installation, err = s.find(ctx, appJWT); err != nil {
			return "", err
		}
	}
	tok, exp, err := s.mint(ctx, appJWT)
	if err != nil {
		return "", err
	}
	s.token, s.expires = tok, exp
	return tok, nil
}

// sign is the App JWT: RS256, issued by the App id.
func (s *tokens) sign() (string, error) {
	raw, err := os.ReadFile(filepath.Clean(s.idFile))
	if err != nil {
		return "", fmt.Errorf("app id: %w", err)
	}
	id, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || id < 1 {
		return "", errors.New("app id: not a positive integer")
	}
	pemKey, err := os.ReadFile(filepath.Clean(s.keyFile))
	if err != nil {
		return "", fmt.Errorf("private key: %w", err)
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM(pemKey)
	if err != nil {
		return "", errors.New("private key: not an RSA key in PEM") // never the parser's text: it may quote the file
	}
	now := s.now()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer: strconv.FormatInt(id, 10), IssuedAt: jwt.NewNumericDate(now.Add(-jwtBackdate)),
		ExpiresAt: jwt.NewNumericDate(now.Add(jwtLifetime)),
	}).SignedString(key)
	if err != nil {
		return "", fmt.Errorf("sign the App JWT: %w", err)
	}
	return signed, nil
}

// find is the App's installation on the repository.
func (s *tokens) find(ctx context.Context, appJWT string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	status, err := s.call(ctx, http.MethodGet, "repos/"+s.owner+"/"+s.name+"/installation", appJWT, nil, &out)
	switch {
	case status == http.StatusNotFound:
		return 0, fmt.Errorf("the App is not installed on %s/%s", s.owner, s.name)
	case err != nil:
		return 0, fmt.Errorf("find the App's installation: %w", err)
	case out.ID < 1:
		return 0, errors.New("find the App's installation: no id in the reply")
	}
	return out.ID, nil
}

// mint is a new installation token, scoped to the repository and to the connection's
// permissions: the factory App's, unless Options said otherwise.
func (s *tokens) mint(ctx context.Context, appJWT string) (string, time.Time, error) {
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	perms := s.perms
	if perms == nil {
		perms = permissions()
	}
	in := map[string]any{"repositories": []string{s.name}, "permissions": perms}
	path := "app/installations/" + strconv.FormatInt(s.installation, 10) + "/access_tokens"
	if _, err := s.call(ctx, http.MethodPost, path, appJWT, in, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("mint an installation token: %w", err)
	}
	if out.Token == "" || !out.ExpiresAt.After(s.now()) {
		return "", time.Time{}, errors.New("mint an installation token: the reply has no live token")
	}
	return out.Token, out.ExpiresAt, nil
}

// call is one App-authenticated request. Its errors name the status only: a reply body may
// quote a credential.
func (s *tokens) call(ctx context.Context, method, path, appJWT string, in, out any) (int, error) {
	var body bytes.Buffer
	if in != nil {
		if err := json.NewEncoder(&body).Encode(in); err != nil {
			return 0, fmt.Errorf("encode: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, s.api.JoinPath(path).String(), &body)
	if err != nil {
		return 0, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", s.agent)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := httpx.ReadBody(resp.Body, maxAuthReply)
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return resp.StatusCode, fmt.Errorf("%s %s: status %d", method, path, resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp.StatusCode, errors.New("the reply is not JSON")
	}
	return resp.StatusCode, nil
}

// auth puts the installation token on requests to the API host, and only there: anything else,
// a redirect's target included, goes out without it.
type auth struct {
	base   http.RoundTripper
	host   string
	agent  string
	tokens *tokens
}

// RoundTrip implements http.RoundTripper.
func (a *auth) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Host, a.host) {
		return a.base.RoundTrip(req)
	}
	tok, err := a.tokens.get(req.Context())
	if err != nil {
		return nil, err
	}
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("User-Agent", a.agent)
	return a.base.RoundTrip(r)
}
