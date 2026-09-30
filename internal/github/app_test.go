// SPDX-License-Identifier: Apache-2.0

package github

import (
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
		_ = json.NewDecoder(r.Body).Decode(&f.scope)
		life := f.expires
		if life == 0 {
			life = time.Hour
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_installation", "expires_at": time.Now().Add(life)})
	case r.URL.Path == "/repos/Smana/cloud-native-ref/issues/12/comments" && auth == "ghs_installation":
		if r.Method == http.MethodGet {
			f.pages = append(f.pages, r.URL.RawQuery)
			per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			lo, hi := min((page-1)*per, len(f.comments)), min(page*per, len(f.comments))
			_ = json.NewEncoder(w).Encode(f.comments[lo:hi])
			return
		}
		if f.postCode != 0 {
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
