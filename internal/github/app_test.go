// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	pr     = "https://github.com/Smana/cloud-native-ref/pull/12"
	marker = "<!-- agent-room:3kq7x2ma:42 -->"
	bot    = "ogenki-agent-factory[bot]"
)

type comment struct {
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
}

// fakeGitHub serves the endpoints the client uses, checking the App JWT and
// the installation token the way GitHub does.
type fakeGitHub struct {
	pub      *rsa.PublicKey
	mu       sync.Mutex
	comments []comment
	requests int
	apps     int
	tokens   int
	posts    int
	pages    []string
	claims   jwt.RegisteredClaims // the last App JWT's
	scope    map[string]any
	missing  bool          // the App is not installed on the repository
	expires  time.Duration // an installation token's life; an hour by default
	postCode int           // the comment POST's status; 201 by default
	postHdr  http.Header   // headers sent with postCode
	stale    string        // an installation token GitHub now refuses with 401
	refuse   bool          // GitHub refuses every installation token with 401
	current  string        // the last token minted
}

// locked runs fn under the fake's lock: the handler runs on the server's goroutines.
func (f *fakeGitHub) locked(fn func()) { f.mu.Lock(); defer f.mu.Unlock(); fn() }

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	appJWT := func() bool {
		var claims jwt.RegisteredClaims
		tok, err := jwt.ParseWithClaims(auth, &claims, func(*jwt.Token) (any, error) { return f.pub, nil },
			jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer("4242"), jwt.WithExpirationRequired())
		f.claims = claims
		return err == nil && tok.Valid
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/app" && appJWT():
		f.apps++
		_, _ = w.Write([]byte(`{"slug":"ogenki-agent-factory"}`))
	case r.Method == http.MethodGet && r.URL.Path == "/repos/Smana/cloud-native-ref/installation" && appJWT():
		if f.missing {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":77}`))
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/77/access_tokens" && appJWT():
		f.tokens++
		f.current = fmt.Sprintf("ghs_installation-%d", f.tokens)
		_ = json.NewDecoder(r.Body).Decode(&f.scope)
		life := f.expires
		if life == 0 {
			life = time.Hour
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": f.current, "expires_at": time.Now().Add(life)})
	case r.URL.Path == "/repos/Smana/cloud-native-ref/issues/12/comments" && strings.HasPrefix(auth, "ghs_installation-") && (auth == f.stale || f.refuse):
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	case r.URL.Path == "/repos/Smana/cloud-native-ref/issues/12/comments" && strings.HasPrefix(auth, "ghs_installation-"):
		if r.Method == http.MethodGet {
			f.pages = append(f.pages, r.URL.RawQuery)
			per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			lo, hi := min((page-1)*per, len(f.comments)), min(page*per, len(f.comments))
			_ = json.NewEncoder(w).Encode(f.comments[lo:hi])
			return
		}
		if f.postCode != 0 {
			for k, v := range f.postHdr {
				w.Header()[k] = v
			}
			http.Error(w, `{"message":"nope ghs_installation"}`, f.postCode)
			return
		}
		var in struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.posts++
		c := comment{Body: in.Body, HTMLURL: fmt.Sprintf("%s#issuecomment-%d", pr, len(f.comments)+1)}
		c.User.Login = bot
		f.comments = append(f.comments, c)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(c)
	default:
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	}
}

func writeKey(t *testing.T, dir string, key *rsa.PrivateKey, id string) {
	t.Helper()
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(filepath.Join(dir, "private_key"), pemKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app_id"), []byte(id), 0o600); err != nil {
		t.Fatal(err)
	}
}

// clock is a settable Now.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func app(t *testing.T) (*App, *fakeGitHub, *clock) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeKey(t, dir, key, "4242\n")
	f := &fakeGitHub{pub: &key.PublicKey}
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	c := &clock{t: time.Now()}
	return &App{Dir: dir, API: srv.URL, HC: srv.Client(), Now: c.now}, f, c
}

func TestCommentsOnceAsTheApp(t *testing.T) {
	a, f, _ := app(t)
	first, err := a.Comment(t.Context(), pr, marker, "### Agent review: approved")
	if err != nil {
		t.Fatal(err)
	}
	again, err := a.Comment(t.Context(), pr, marker, "### Agent review: approved")
	f.locked(func() {
		if err != nil || again != first || f.posts != 1 {
			t.Fatalf("second call: %q vs %q, posts = %d, err = %v", again, first, f.posts, err)
		}
		if f.comments[0].Body != "### Agent review: approved\n\n"+marker {
			t.Fatalf("the marker ends the comment: %q", f.comments[0].Body)
		}
		if f.tokens != 1 || f.apps != 1 {
			t.Fatalf("the installation token and the slug are cached: %d mints, %d /app", f.tokens, f.apps)
		}
		scope, _ := json.Marshal(f.scope)
		if string(scope) != `{"permissions":{"pull_requests":"write"},"repositories":["cloud-native-ref"]}` {
			t.Fatalf("token scope = %s", scope)
		}
	})
}

// A marker in someone else's comment (an agent's, say) must not suppress the
// verdict, nor must the App's own comment that merely quotes the marker.
func TestOnlyTheAppsOwnCommentCounts(t *testing.T) {
	a, f, _ := app(t)
	f.locked(func() {
		forged := comment{Body: "nothing to see " + marker, HTMLURL: pr + "#forged"}
		forged.User.Login = "ogenki-agents[bot]"
		lookalike := comment{Body: "nothing to see " + marker, HTMLURL: pr + "#lookalike"}
		lookalike.User.Login = "ogenki-agent-factory"
		quoted := comment{Body: marker + " was quoted here", HTMLURL: pr + "#quoted"}
		quoted.User.Login = bot
		f.comments = append(f.comments, forged, lookalike, quoted)
	})
	got, err := a.Comment(t.Context(), pr, marker, "### Agent review: approved")
	f.locked(func() {
		if err != nil || f.posts != 1 || got != pr+"#issuecomment-4" {
			t.Fatalf("got %q, posts = %d, err = %v", got, f.posts, err)
		}
	})
}

// The App's comment is found on any page: the list is oldest first.
func TestFindsItsCommentPastTheFirstPage(t *testing.T) {
	a, f, _ := app(t)
	f.locked(func() {
		for i := range 2*perPage + 5 {
			c := comment{Body: fmt.Sprintf("chatter %d", i), HTMLURL: fmt.Sprintf("%s#c%d", pr, i)}
			c.User.Login = "someone"
			f.comments = append(f.comments, c)
		}
		mine := comment{Body: "### Agent review: approved\n\n" + marker + "\n", HTMLURL: pr + "#mine"}
		mine.User.Login = bot
		f.comments = append(f.comments, mine)
	})
	got, err := a.Comment(t.Context(), pr, marker, "### Agent review: approved")
	f.locked(func() {
		if err != nil || got != pr+"#mine" || f.posts != 0 {
			t.Fatalf("got %q, posts %d, err %v", got, f.posts, err)
		}
		if len(f.pages) != 3 {
			t.Fatalf("pages read: %v", f.pages)
		}
	})
}

// Past maxPages the client cannot know the marker is absent, so it refuses
// rather than risk a second verdict comment.
func TestAPullRequestTooLongToSearchIsRefused(t *testing.T) {
	a, f, _ := app(t)
	f.locked(func() {
		for range maxPages * perPage {
			c := comment{Body: "chatter"}
			c.User.Login = "someone"
			f.comments = append(f.comments, c)
		}
	})
	_, err := a.Comment(t.Context(), pr, marker, "x")
	f.locked(func() {
		if !errors.Is(err, ErrTooManyComments) || f.posts != 0 || len(f.pages) != maxPages {
			t.Fatalf("err = %v, posts %d, pages %d", err, f.posts, len(f.pages))
		}
	})
}

func TestTheTokenIsRenewedBeforeItExpires(t *testing.T) {
	a, f, c := app(t)
	if _, err := a.Comment(t.Context(), pr, marker, "x"); err != nil {
		t.Fatal(err)
	}
	c.add(54 * time.Minute)
	if _, err := a.Comment(t.Context(), pr, marker, "x"); err != nil {
		t.Fatal(err)
	}
	f.locked(func() {
		if f.tokens != 1 {
			t.Fatalf("%d mints with six minutes left", f.tokens)
		}
	})
	c.add(time.Minute + time.Second)
	if _, err := a.Comment(t.Context(), pr, marker, "x"); err != nil {
		t.Fatal(err)
	}
	f.locked(func() {
		if f.tokens != 2 || f.apps != 1 {
			t.Fatalf("%d mints, %d /app: renewed under five minutes, the slug kept", f.tokens, f.apps)
		}
	})
}

// A token already inside the renewal window is not cached as live.
func TestADeadTokenIsRefused(t *testing.T) {
	a, f, _ := app(t)
	f.locked(func() { f.expires = -time.Minute })
	if _, err := a.Comment(t.Context(), pr, marker, "x"); err == nil {
		t.Fatal("an expired token was used")
	}
}

// Concurrent callers wait for one mint rather than racing to many.
func TestOneMintForConcurrentCallers(t *testing.T) {
	a, f, _ := app(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, _, err := a.installation(t.Context(), mustAPI(t, a), "Smana", "cloud-native-ref"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	f.locked(func() {
		if f.tokens != 1 {
			t.Fatalf("%d mints", f.tokens)
		}
	})
}

func TestErrorsSayWhetherARetryCanHelp(t *testing.T) {
	a, f, _ := app(t)
	f.locked(func() { f.missing = true })
	_, err := a.Comment(t.Context(), pr, marker, "x")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusNotFound || !ae.Permanent() {
		t.Fatalf("err = %v", err)
	}
	for status, permanent := range map[int]bool{401: true, 403: true, 404: true, 422: true, 429: false, 500: false, 502: false} {
		if (&APIError{Status: status}).Permanent() != permanent {
			t.Errorf("HTTP %d: Permanent() != %v", status, permanent)
		}
	}
	a, f, _ = app(t)
	f.locked(func() { f.postCode = http.StatusUnprocessableEntity })
	_, err = a.Comment(t.Context(), pr, marker, "x")
	if !errors.As(err, &ae) || ae.Status != http.StatusUnprocessableEntity || strings.Contains(err.Error(), "ghs_") {
		t.Fatalf("err = %v: the status, never the reply body", err)
	}
}

func TestOnlyGitHubPullRequests(t *testing.T) {
	a, f, _ := app(t)
	for _, u := range []string{
		"https://github.com/Smana/cloud-native-ref/issues/12",
		"https://evil.example/Smana/cloud-native-ref/pull/12",
		"http://github.com/Smana/cloud-native-ref/pull/12",
		"https://github.com/Smana/../pull/12",
		"https://github.com/Smana/cloud-native-ref/pull/0",
		"https://github.com/Smana/cloud-native-ref/pull/12?x=1",
		pr + "/files",
	} {
		if _, err := a.Comment(t.Context(), u, marker, "x"); err == nil {
			t.Errorf("accepted %s", u)
		}
	}
	f.locked(func() {
		if f.requests != 0 {
			t.Fatalf("%d requests for refused URLs", f.requests)
		}
	})
}

// The installation token goes to an https API host only: a plain-HTTP server
// that would answer, and credentials in the URL, are both refused unsent.
func TestOnlyAnHTTPSAPI(t *testing.T) {
	a, f, _ := app(t)
	plain := httptest.NewServer(f)
	t.Cleanup(plain.Close)
	tls, _ := url.Parse(a.API)
	for _, api := range []string{plain.URL, "https://user:pw@" + tls.Host, "https://", "::"} {
		b := &App{Dir: a.Dir, API: api, HC: plain.Client(), Now: a.Now}
		if strings.HasPrefix(api, "https://user") {
			b.HC = a.HC
		}
		if _, err := b.Comment(t.Context(), pr, marker, "x"); err == nil {
			t.Errorf("API %q accepted", api)
		}
	}
	f.locked(func() {
		if f.requests != 0 {
			t.Errorf("%d requests", f.requests)
		}
	})
}

// GitHub caps an App JWT at ten minutes and rejects one issued in its future:
// iat is backdated a minute, exp nine minutes ahead.
func TestTheAppJWTIsBackdatedAndShort(t *testing.T) {
	a, f, c := app(t)
	if _, err := a.Comment(t.Context(), pr, marker, "x"); err != nil {
		t.Fatal(err)
	}
	now := c.now().Truncate(time.Second)
	f.locked(func() {
		if !f.claims.IssuedAt.Equal(now.Add(-time.Minute)) || !f.claims.ExpiresAt.Equal(now.Add(9*time.Minute)) {
			t.Fatalf("iat %v exp %v, now %v", f.claims.IssuedAt, f.claims.ExpiresAt, now)
		}
	})
}

// A bad key file fails without quoting it: the error can reach a log.
func TestBadKeyFilesNeverLeakIntoErrors(t *testing.T) {
	a, _, _ := app(t)
	if err := os.WriteFile(filepath.Join(a.Dir, "private_key"), []byte("-----BEGIN RSA PRIVATE KEY-----\nc2VjcmV0LWtleS1ieXRlcw==\n-----END RSA PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := a.Comment(t.Context(), pr, marker, "x")
	if err == nil || strings.Contains(err.Error(), "c2VjcmV0") {
		t.Fatalf("err = %v", err)
	}
	for _, id := range []string{"", "abc", "0", "-3"} {
		a, f, _ := app(t)
		if err := os.WriteFile(filepath.Join(a.Dir, "app_id"), []byte(id), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Comment(t.Context(), pr, marker, "x"); err == nil {
			t.Errorf("app id %q accepted", id)
		}
		f.locked(func() {
			if f.requests != 0 {
				t.Errorf("app id %q: %d requests", id, f.requests)
			}
		})
	}
}

func TestIncompleteAppIsRefused(t *testing.T) {
	a, _, _ := app(t)
	for name, broken := range map[string]*App{
		"no client": {Dir: a.Dir, API: a.API, Now: a.Now},
		"no clock":  {Dir: a.Dir, API: a.API, HC: a.HC},
	} {
		if _, err := broken.Comment(t.Context(), pr, marker, "x"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDisabledUntilTheKeyLands(t *testing.T) {
	if (&App{Dir: t.TempDir()}).Enabled() {
		t.Fatal("no key file, no App")
	}
	if a, _, _ := app(t); !a.Enabled() {
		t.Fatal("key file present")
	}
}

func mustAPI(t *testing.T, a *App) *url.URL {
	t.Helper()
	u, err := a.api()
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// Concurrent posts of one verdict search and post one at a time: the second
// finds the first's comment instead of posting its own.
func TestConcurrentCallsForOneMarkerPostOnce(t *testing.T) {
	a, f, _ := app(t)
	urls := make([]string, 8)
	var wg sync.WaitGroup
	for i := range urls {
		wg.Go(func() {
			u, err := a.Comment(t.Context(), pr, marker, "### Agent review: approved")
			if err != nil {
				t.Error(err)
			}
			urls[i] = u
		})
	}
	wg.Wait()
	f.locked(func() {
		if f.posts != 1 {
			t.Fatalf("%d posts for one marker", f.posts)
		}
	})
	for _, u := range urls {
		if u != urls[0] {
			t.Fatalf("urls %v", urls)
		}
	}
	if len(a.posting) != 0 {
		t.Fatalf("%d marker locks left held", len(a.posting))
	}
}

// Another marker is not held up by one whose post is in flight.
func TestAnotherMarkerIsNotHeld(t *testing.T) {
	a, _, _ := app(t)
	unlock, err := a.lockMarker(t.Context(), pr+marker)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := a.Comment(t.Context(), pr, "<!-- agent-room:3kq7x2ma:43 -->", "x"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := a.Comment(ctx, pr, marker, "x"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a held marker waits until ctx ends: %v", err)
	}
}

// Permanent tells the poster what a retry cannot heal (review forwards for 3.5):
// GitHub's 4xx other than 429, a URL that is no pull request, and a pull
// request too long to search. A key that cannot be read or an App id that is
// not one may heal once the Secret lands, so they are not.
func TestPermanent(t *testing.T) {
	a, _, _ := app(t)
	_, notPR := a.Comment(t.Context(), "https://github.com/Smana/cloud-native-ref/issues/1", marker, "x")
	b, _, _ := app(t)
	if err := os.Remove(filepath.Join(b.Dir, "private_key")); err != nil {
		t.Fatal(err)
	}
	_, noKey := b.Comment(t.Context(), pr, marker, "x")
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"not a pull request":   {notPR, true},
		"too many comments":    {fmt.Errorf("post: %w", ErrTooManyComments), true},
		"not installed":        {fmt.Errorf("x: %w", &APIError{Status: 404}), true},
		"rate limited":         {&APIError{Status: 429}, false},
		"GitHub down":          {&APIError{Status: 502}, false},
		"no key file yet":      {noKey, false},
		"a network failure":    {errors.New("dial tcp: connection refused"), false},
		"no error":             {nil, false},
		"the caller cancelled": {context.Canceled, false},
	} {
		if tc.err == nil && name != "no error" {
			t.Fatalf("%s: no error to classify", name)
		}
		if got := Permanent(tc.err); got != tc.want {
			t.Errorf("%s (%v): Permanent = %v", name, tc.err, got)
		}
	}
}

// A 401 on an installation token (revoked, or the App's key rotated) is healed
// by one fresh token: the cached one is evicted and the call retried once.
func TestA401RetriesOnceWithAFreshToken(t *testing.T) {
	a, f, _ := app(t)
	if _, err := a.Comment(t.Context(), pr, marker, "x"); err != nil {
		t.Fatal(err)
	}
	f.locked(func() { f.stale = f.current })
	if _, err := a.Comment(t.Context(), pr, "<!-- agent-room:3kq7x2ma:43 -->", "y"); err != nil {
		t.Fatal(err)
	}
	f.locked(func() {
		if f.tokens != 2 || f.posts != 2 {
			t.Fatalf("%d mints, %d posts", f.tokens, f.posts)
		}
		f.refuse = true
	})
	_, err := a.Comment(t.Context(), pr, "<!-- agent-room:3kq7x2ma:44 -->", "z")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized || !Permanent(err) {
		t.Fatalf("err = %v", err)
	}
	f.locked(func() {
		if f.tokens != 3 {
			t.Fatalf("%d mints: one retry, not a loop", f.tokens)
		}
	})
}

// A 403 that says when to come back is GitHub's secondary rate limit, and a
// 403 or 429 with no requests left is its primary one: both heal, so the
// verdict is retried rather than dropped.
func TestRateLimitsAreTransient(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      int
		hdr       http.Header
		permanent bool
		after     time.Duration
	}{
		{"a 403 with Retry-After", 403, http.Header{"Retry-After": {"60"}}, false, time.Minute},
		{"a 403 with no requests left", 403, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"%RESET%"}}, false, 2 * time.Minute},
		{"a 429 with an HTTP date", 429, http.Header{"Retry-After": {"%DATE%"}}, false, 3 * time.Minute},
		{"a plain 403", 403, nil, true, 0},
		{"a 403 with requests left", 403, http.Header{"X-Ratelimit-Remaining": {"12"}}, true, 0},
		{"a 422", 422, http.Header{"Retry-After": {"60"}}, true, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, c := app(t)
			hdr := http.Header{}
			for k, v := range tc.hdr {
				v := strings.ReplaceAll(v[0], "%RESET%", strconv.FormatInt(c.now().Add(2*time.Minute).Unix(), 10))
				v = strings.ReplaceAll(v, "%DATE%", c.now().Add(3*time.Minute).UTC().Format(http.TimeFormat))
				hdr[k] = []string{v}
			}
			f.locked(func() { f.postCode, f.postHdr = tc.code, hdr })
			_, err := a.Comment(t.Context(), pr, marker, "x")
			var ae *APIError
			if !errors.As(err, &ae) || ae.Status != tc.code {
				t.Fatalf("err = %v", err)
			}
			if Permanent(err) != tc.permanent {
				t.Fatalf("Permanent = %v, want %v", !tc.permanent, tc.permanent)
			}
			if d := ae.RetryAfter - tc.after; d < -2*time.Second || d > 2*time.Second {
				t.Fatalf("RetryAfter = %s, want %s", ae.RetryAfter, tc.after)
			}
		})
	}
}

// The body is the poster's; whatever it holds, it never carries an HTML
// comment, so it cannot plant another verdict's marker.
func TestABodyCannotPlantAMarker(t *testing.T) {
	a, f, _ := app(t)
	other := "<!-- agent-room:3kq7x2ma:99 -->"
	if _, err := a.Comment(t.Context(), pr, marker, "LGTM\n\n"+other); err != nil {
		t.Fatal(err)
	}
	f.locked(func() {
		if strings.Count(f.comments[0].Body, "<!--") != 1 || !strings.HasSuffix(f.comments[0].Body, marker) {
			t.Fatalf("body = %q", f.comments[0].Body)
		}
	})
	// The planted marker is not found as the App's comment for verdict 99.
	if _, err := a.Comment(t.Context(), pr, other, "the real verdict 99"); err != nil {
		t.Fatal(err)
	}
	f.locked(func() {
		if f.posts != 2 {
			t.Fatalf("verdict 99 was suppressed: %d posts", f.posts)
		}
	})
}

// Neutralise makes untrusted text (an agent's summary) inert on a pull request:
// no hidden direction or zero-width characters, no HTML (comments, images,
// details), no auto-loading image, no @mention notifying anyone.
func TestNeutralise(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain text is kept", "Looks right.\n- tests pass\n> quoted", "Looks right.\n- tests pass\n> quoted"},
		{"a fake marker", "ok <!-- agent-room:3kq7x2ma:42 -->", "ok &lt;!-- agent-room:3kq7x2ma:42 -->"},
		{"an HTML image", `<img src="https://evil.example/t.png">`, `&lt;img src="https://evil.example/t.png">`},
		{"a details block", "<details open>x</details>", "&lt;details open>x&lt;/details>"},
		{"a markdown image", "![x](https://evil.example/t.png)", "!\\[x](https://evil.example/t.png)"},
		{"a reference image", "![x][r]\n\n[r]: https://evil.example/t.png", "!\\[x][r]\n\n[r]: https://evil.example/t.png"},
		{"a mention", "cc @Smana and @org/team", "cc ＠Smana and ＠org/team"},
		{"an email is not a mention", "mail a@b.example", "mail a＠b.example"},
		{"a lone at", "meet @ noon", "meet @ noon"},
		{"bidi overrides", "safe\u202egnp.exe\u202c ok\u2066x\u2069", "safegnp.exe okx"},
		{"zero-width and BOM", "a\u200bb\u200dc\ufeffd\u2060e\u00adf", "abcdef"},
		{"controls", "a\u0000b\u001b[31mc\u0085d\re", "ab[31mcde"},
		{"an entity that would decode", "&lt;!-- agent-room:x:1 --&gt;", "&amp;lt;!-- agent-room:x:1 --&amp;gt;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Neutralise(tc.in); got != tc.want {
				t.Fatalf("Neutralise(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
