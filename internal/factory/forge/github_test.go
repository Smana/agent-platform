// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Smana/agent-platform/internal/httpx"
)

const prJSON = `{"data":{"repository":{"pullRequest":{
 "id":"PR_kwDO1","number":12,"url":"https://github.com/Smana/demo/pull/12","title":"docs: fix a link","state":"OPEN",
 "headRefName":"agent/3buqdlot","headRefOid":"abc123",
 "author":{"__typename":"Bot","login":"ogenki-agents"},
 "mergedBy":null,"mergeCommit":null,"autoMergeRequest":null,
 "labels":{"nodes":[{"name":"factory/class:docs-links"}]},
 "reviews":{"nodes":[{"databaseId":901,"state":"CHANGES_REQUESTED","body":"Use the relative link.","submittedAt":"2026-09-27T10:00:00Z",
   "author":{"__typename":"User","login":"Smana"},"comments":{"nodes":[{"path":"docs/a.md","line":3,"body":"here"}]}}]},
 "comments":{"nodes":[{"databaseId":55,"body":"/factory retry","createdAt":"2026-09-27T10:05:00Z","author":{"__typename":"User","login":"Smana"}}]},
 "commits":{"nodes":[{"commit":{"message":"docs: fix a link\n\nAgent-Run: 7f3cq2xz"}}]}}}}}`

const issueJSON = `{"data":{"repository":{"issue":{"number":7,"url":"https://github.com/Smana/demo/issues/7",
 "title":"Fix the link","body":"The link in docs/a.md is broken.","state":"OPEN","lastEditedAt":null,
 "labels":{"nodes":[{"name":"factory/ready"}]},
 "timelineItems":{"nodes":[{"createdAt":"2026-09-27T09:04:00Z"}]}}}}}`

const eventsJSON = `[
 {"event":"labeled","label":{"name":"factory/ready"},"actor":{"login":"someone"},"created_at":"2026-09-27T09:00:00Z"},
 {"event":"labeled","label":{"name":"bug"},"actor":{"login":"Smana"},"created_at":"2026-09-27T09:01:00Z"},
 {"event":"unlabeled","label":{"name":"factory/ready"},"actor":{"login":"ogenki-agent-factory[bot]"},"created_at":"2026-09-27T09:02:00Z"},
 {"event":"labeled","label":{"name":"factory/ready"},"actor":{"login":"Smana"},"created_at":"2026-09-27T09:03:00Z"}]`

const appID = "123456"

// fakeGitHub is GitHub for one App installed on Smana/demo: it verifies the App JWT with the
// current public key, mints installation tokens, and serves the REST and GraphQL calls only to
// a request carrying the current token.
type fakeGitHub struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	pub   *rsa.PublicKey
	token string
	mints int
	ttl   time.Duration
	now   func() time.Time
	calls []string
	mint  func(w http.ResponseWriter) bool // overrides the mint reply when it returns true
}

func newFakeGitHub(t *testing.T, pub *rsa.PublicKey, now func() time.Time) *fakeGitHub {
	g := &fakeGitHub{t: t, pub: pub, ttl: time.Hour, now: now}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/Smana/demo/installation", func(w http.ResponseWriter, r *http.Request) {
		if !g.appJWT(r) {
			http.Error(w, "bad JWT", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"id":42}`)
	})
	mux.HandleFunc("GET /repos/Smana/other/installation", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	mux.HandleFunc("GET /repos/Smana/zero/installation", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":0}`)
	})
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !g.appJWT(r) {
			http.Error(w, "bad JWT", http.StatusUnauthorized)
			return
		}
		var in struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Repositories) != 1 || in.Repositories[0] != "demo" ||
			in.Permissions["contents"] != "read" || in.Permissions["issues"] != "write" || in.Permissions["pull_requests"] != "write" ||
			len(in.Permissions) != 4 || in.Permissions["metadata"] != "read" {
			t.Errorf("the token is scoped to one repository and the factory App's permissions: %+v %v", in, err)
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.mint != nil && g.mint(w) {
			return
		}
		g.mints++
		g.token = "ghs_" + strings.Repeat("x", g.mints)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": g.token, "expires_at": g.now().Add(g.ttl).Format(time.RFC3339)})
	})
	mux.HandleFunc("POST /graphql", g.authed(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "pullRequest(") {
			_, _ = io.WriteString(w, prJSON)
			return
		}
		_, _ = io.WriteString(w, issueJSON)
	}))
	mux.HandleFunc("GET /repos/Smana/demo/issues/7/events", g.authed(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, eventsJSON)
	}))
	// Issue 8 has more events than the forge reads: every page links to a next one.
	mux.HandleFunc("GET /repos/Smana/demo/issues/8/events", g.authed(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/Smana/demo/issues/8/events?page=%d>; rel="next"`, g.srv.URL, page+1))
		_, _ = io.WriteString(w, eventsJSON)
	}))
	mux.HandleFunc("DELETE /repos/Smana/demo/issues/7/labels/{name}", g.authed(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("name") == "gone" {
			w.WriteHeader(http.StatusNotFound) // already gone: not an error
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	mux.HandleFunc("GET /repos/Smana/demo/issues", g.authed(func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("labels") != "factory/ready" || q.Get("state") != "open" {
			t.Errorf("query %v", q)
		}
		_, _ = io.WriteString(w, `[{"number":7},{"number":12,"pull_request":{"url":"x"}}]`)
	}))
	mux.HandleFunc("GET /repos/Smana/demo/pulls", g.authed(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("head") != "Smana:agent/3buqdlot" {
			_, _ = io.WriteString(w, `[]`)
			return
		}
		_, _ = io.WriteString(w, `[{"number":12},{"number":3}]`)
	}))
	mux.HandleFunc("POST /repos/Smana/demo/issues/7/comments", g.authed(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["body"] != "hello" {
			t.Errorf("comment %v", in)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":1}`)
	}))
	mux.HandleFunc("GET /repos/Smana/demo/issues/7/comments", g.authed(func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("sort") != "created" || q.Get("direction") != "desc" || q.Get("per_page") != "50" {
			t.Errorf("query %v", q)
		}
		_, _ = io.WriteString(w, `[{"id":9,"body":"hi","user":{"login":"Smana"},"created_at":"2026-09-27T10:00:00Z"}]`)
	}))
	mux.HandleFunc("POST /repos/Smana/demo/issues/7/labels", g.authed(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	}))
	mux.HandleFunc("GET /rate_limit", g.authed(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"resources":{}}`)
	}))
	g.srv = httptest.NewTLSServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

// appJWT is GitHub's check of an App JWT: RS256, signed by the App's key, issued by its id,
// valid for at most ten minutes.
func (g *fakeGitHub) appJWT(r *http.Request) bool {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	g.mu.Lock()
	pub := g.pub
	g.mu.Unlock()
	var c jwt.RegisteredClaims
	_, err := jwt.ParseWithClaims(raw, &c, func(*jwt.Token) (any, error) { return pub, nil },
		jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(appID), jwt.WithTimeFunc(g.now))
	return err == nil && c.ExpiresAt != nil && c.IssuedAt != nil && c.ExpiresAt.Sub(c.IssuedAt.Time) <= 10*time.Minute
}

func (g *fakeGitHub) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		ok := g.token != "" && r.Header.Get("Authorization") == "Bearer "+g.token
		g.calls = append(g.calls, r.Method+" "+r.URL.Path)
		g.mu.Unlock()
		if !ok {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		if r.Header.Get("User-Agent") != "agent-factory/test" {
			g.t.Errorf("user agent %q", r.Header.Get("User-Agent"))
		}
		h(w, r)
	}
}

func newKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type rig struct {
	g     *GitHub
	gh    *fakeGitHub
	clk   *clock
	opts  Options
	keyTo string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	k, pemKey := newKey(t)
	clk := &clock{t: time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)}
	gh := newFakeGitHub(t, &k.PublicKey, clk.now)
	dir := t.TempDir()
	idFile, keyFile := filepath.Join(dir, "app_id"), filepath.Join(dir, "private_key")
	if err := os.WriteFile(idFile, []byte(appID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pemKey, 0o600); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(gh.srv.Certificate())
	o := Options{Repository: "Smana/demo", AppIDFile: idFile, PrivateKeyFile: keyFile, APIURL: gh.srv.URL + "/",
		UserAgent: "agent-factory/test", HTTP: httpx.New(5*time.Second, roots), Now: clk.now}
	g, err := Connect(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	return &rig{g: g, gh: gh, clk: clk, opts: o, keyTo: keyFile}
}

func TestPullRequestSnapshot(t *testing.T) {
	r := newRig(t)
	pr, err := r.g.PullRequest(t.Context(), 12)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Author != "ogenki-agents[bot]" || pr.HeadRef != "agent/3buqdlot" || pr.NodeID != "PR_kwDO1" || pr.HeadSHA != "abc123" ||
		pr.State != "OPEN" || pr.AutoMerge || pr.MergedBy != "" || len(pr.Labels) != 1 {
		t.Fatalf("identity: %+v", pr)
	}
	if pr.Trailer("Agent-Run") != "7f3cq2xz" {
		t.Fatal("the head commit's Agent-Run trailer")
	}
	if len(pr.Reviews) != 1 || pr.Reviews[0].Author != "Smana" || pr.Reviews[0].ID != 901 || pr.Reviews[0].Comments[0].Path != "docs/a.md" ||
		pr.Reviews[0].Comments[0].Line != 3 || pr.Reviews[0].At.IsZero() {
		t.Fatalf("reviews %+v", pr.Reviews)
	}
	if len(pr.Comments) != 1 || pr.Comments[0].ID != 55 || pr.Comments[0].Author != "Smana" {
		t.Fatalf("comments %+v", pr.Comments)
	}
}

func TestLabelEventsKeepOnlyTheLabelsAdditions(t *testing.T) {
	r := newRig(t)
	evs, err := r.g.LabelEvents(t.Context(), 7, "factory/ready")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Actor != "someone" || evs[1].Actor != "Smana" || evs[1].Label != "factory/ready" {
		t.Fatalf("%+v", evs)
	}
}

// Past the forge's page cap the newest events are unread: the caller is told, not handed a
// silently partial list (review M4).
func TestLabelEventsPastThePageCapAreTruncated(t *testing.T) {
	r := newRig(t)
	evs, err := r.g.LabelEvents(t.Context(), 8, "factory/ready")
	if !errors.Is(err, ErrEventsTruncated) || len(evs) != 20 {
		t.Fatalf("%d %v", len(evs), err)
	}
}

func TestIssueAndLabels(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	iss, err := r.g.Issue(ctx, 7)
	if err != nil || iss.Title != "Fix the link" || !iss.LastEditedAt.IsZero() || iss.Labels[0] != "factory/ready" ||
		!iss.TitleEditedAt.Equal(time.Date(2026, 9, 27, 9, 4, 0, 0, time.UTC)) {
		t.Fatalf("a body edit and a title rename are both visible: %+v %v", iss, err)
	}
	if err := r.g.RemoveLabel(ctx, 7, "gone"); err != nil {
		t.Fatalf("a 404 on removal means the label is already gone: %v", err)
	}
	if err := r.g.RemoveLabel(ctx, 7, "locked"); err == nil {
		t.Fatal("any other refusal is an error")
	}
	items, err := r.g.Labeled(ctx, "factory/ready")
	if err != nil || len(items) != 2 || items[0].PullRequest || !items[1].PullRequest {
		t.Fatalf("%+v %v", items, err)
	}
	if n, err := r.g.PullRequestForBranch(ctx, "agent/3buqdlot"); err != nil || n != 12 {
		t.Fatalf("the newest PR of the branch: %d %v", n, err)
	}
	if n, err := r.g.PullRequestForBranch(ctx, "agent/none"); err != nil || n != 0 {
		t.Fatalf("no PR: %d %v", n, err)
	}
	if _, err := r.g.PullRequestForBranch(ctx, ""); err == nil {
		t.Fatal("an empty head filter would match every pull request")
	}
	for _, n := range []int{0, -1, 1 << 31} {
		if _, err := r.g.Issue(ctx, n); err == nil {
			t.Fatalf("issue %d", n)
		}
		if _, err := r.g.PullRequest(ctx, n); err == nil {
			t.Fatalf("pull request %d", n)
		}
	}
	if err := r.g.Comment(ctx, 7, "hello"); err != nil {
		t.Fatal(err)
	}
	cs, err := r.g.RecentComments(ctx, 7)
	if err != nil || len(cs) != 1 || cs[0].ID != 9 || cs[0].Author != "Smana" || cs[0].Body != "hi" {
		t.Fatalf("%+v %v", cs, err)
	}
	if err := r.g.AddLabels(ctx, 7, "factory/class:docs-links"); err != nil {
		t.Fatal(err)
	}
	if err := r.g.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

// The installation token is minted once and reused until five minutes before it expires; the
// key and the App id are read at each mint, so a rotated key needs no restart.
func TestInstallationTokenIsCachedRefreshedAndReadsTheKeyAtUse(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	for range 3 {
		if _, err := r.g.Issue(ctx, 7); err != nil {
			t.Fatal(err)
		}
	}
	if r.gh.mints != 1 {
		t.Fatalf("minted %d tokens", r.gh.mints)
	}
	r.clk.add(54 * time.Minute) // six minutes left: still fresh
	if _, err := r.g.Issue(ctx, 7); err != nil || r.gh.mints != 1 {
		t.Fatalf("%d %v", r.gh.mints, err)
	}
	k, pemKey := newKey(t) // the owner rotates the App's key
	if err := os.WriteFile(r.keyTo, pemKey, 0o600); err != nil {
		t.Fatal(err)
	}
	r.gh.mu.Lock()
	r.gh.pub = &k.PublicKey
	r.gh.mu.Unlock()
	r.clk.add(2 * time.Minute) // four minutes left: refreshed with the new key
	if _, err := r.g.Issue(ctx, 7); err != nil || r.gh.mints != 2 {
		t.Fatalf("%d %v", r.gh.mints, err)
	}
}

// /readyz's "App token fresh" (§6.5): a call succeeded in the last three minutes.
func TestHealthyFollowsTheLastSuccess(t *testing.T) {
	r := newRig(t)
	if !r.g.Healthy(r.clk.now()) {
		t.Fatal("Connect's mint is a success")
	}
	r.clk.add(3 * time.Minute)
	if r.g.Healthy(r.clk.now()) {
		t.Fatal("three minutes without a success is stale")
	}
	if err := r.g.RemoveLabel(t.Context(), 7, "locked"); err == nil {
		t.Fatal("refused")
	}
	if r.g.Healthy(r.clk.now()) {
		t.Fatal("a failed call is not a success")
	}
	if err := r.g.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !r.g.Healthy(r.clk.now()) {
		t.Fatal("a successful ping")
	}
	if r.g.Healthy(r.clk.now().Add(-time.Second)) {
		t.Fatal("a success in the future of now is not trusted")
	}
}

func TestConnectRefuses(t *testing.T) {
	base := newRig(t)
	_, otherKey := newKey(t)
	for name, c := range map[string]struct {
		edit func(t *testing.T, o *Options)
		why  string
	}{
		"an http API":         {func(_ *testing.T, o *Options) { o.APIURL = strings.Replace(o.APIURL, "https://", "http://", 1) }, "must be https://<host>/"},
		"an API with no host": {func(_ *testing.T, o *Options) { o.APIURL = "https:///" }, "must be https://<host>/"},
		"userinfo in the API": {func(_ *testing.T, o *Options) { o.APIURL = strings.Replace(o.APIURL, "https://", "https://u:p@", 1) }, "must be https://<host>/"},
		"a repository path":   {func(_ *testing.T, o *Options) { o.Repository = "Smana/demo/x" }, "owner/name"},
		"no owner":            {func(_ *testing.T, o *Options) { o.Repository = "/demo" }, "owner/name"},
		"no HTTP client":      {func(_ *testing.T, o *Options) { o.HTTP = nil }, "HTTP"},
		"no clock":            {func(_ *testing.T, o *Options) { o.Now = nil }, "clock"},
		"no user agent":       {func(_ *testing.T, o *Options) { o.UserAgent = "" }, "user agent"},
		"a non-numeric id":    {func(t *testing.T, o *Options) { o.AppIDFile = write(t, "abc") }, "app id"},
		"a zero id":           {func(t *testing.T, o *Options) { o.AppIDFile = write(t, "0") }, "app id"},
		"a missing id":        {func(_ *testing.T, o *Options) { o.AppIDFile = "/nonexistent/app_id" }, "app id"},
		"not a key":           {func(t *testing.T, o *Options) { o.PrivateKeyFile = write(t, "not a key") }, "private key"},
		"another App's key":   {func(t *testing.T, o *Options) { o.PrivateKeyFile = write(t, string(otherKey)) }, "401"},
		"not installed":       {func(_ *testing.T, o *Options) { o.Repository = "Smana/other" }, "not installed"},
		"no installation id":  {func(_ *testing.T, o *Options) { o.Repository = "Smana/zero" }, "no id"},
	} {
		t.Run(name, func(t *testing.T) {
			o := base.opts
			c.edit(t, &o)
			_, err := Connect(t.Context(), o)
			if err == nil || !strings.Contains(err.Error(), c.why) {
				t.Fatalf("err = %v, want one naming %q", err, c.why)
			}
		})
	}
}

// A mint reply GitHub never sends is refused, and never logged: the error names the status only.
func TestConnectRefusesABadMint(t *testing.T) {
	for name, reply := range map[string]string{
		"no token":      `{"expires_at":"2026-09-30T11:00:00Z"}`,
		"no expiry":     `{"token":"ghs_secret"}`,
		"already spent": `{"token":"ghs_secret","expires_at":"2026-09-30T09:00:00Z"}`,
		"not JSON":      `ghs_secret`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.gh.mu.Lock()
			r.gh.mint = func(w http.ResponseWriter) bool { _, _ = io.WriteString(w, reply); return true }
			r.gh.mu.Unlock()
			_, err := Connect(t.Context(), r.opts)
			if err == nil || strings.Contains(err.Error(), "ghs_secret") {
				t.Fatalf("err = %v", err)
			}
		})
	}
	r := newRig(t)
	r.gh.mu.Lock()
	r.gh.mint = func(w http.ResponseWriter) bool {
		http.Error(w, "ghs_secret", http.StatusInternalServerError)
		return true
	}
	r.gh.mu.Unlock()
	if _, err := Connect(t.Context(), r.opts); err == nil || strings.Contains(err.Error(), "ghs_secret") {
		t.Fatalf("err = %v", err)
	}
}

// The installation token goes to the API host only: a request anywhere else, a redirect's
// target included, never carries it.
func TestTheTokenGoesToTheAPIHostOnly(t *testing.T) {
	r := newRig(t)
	var got string
	other := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		got = req.Header.Get("Authorization")
	}))
	defer other.Close()
	roots := x509.NewCertPool()
	roots.AddCert(other.Certificate())
	hc := r.g.authed(httpx.New(5*time.Second, roots))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, other.URL+"/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got != "" {
		t.Fatalf("another host got %q", got)
	}
}

type recorder struct{ got *http.Request }

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.got = req
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

// Plain http to the API host never carries the token either: it would cross the wire in clear.
func TestTheTokenNeverGoesOverPlainHTTP(t *testing.T) {
	r := newRig(t)
	for scheme, want := range map[string]bool{"http": false, "https": true} {
		rec := &recorder{}
		a := &auth{base: rec, host: r.g.api.Host, agent: "agent-factory/test", tokens: r.g.tokens}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, scheme+"://"+r.g.api.Host+"/x", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := a.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if got := rec.got.Header.Get("Authorization") != ""; got != want {
			t.Errorf("%s: token sent = %v", scheme, got)
		}
		if req.Header.Get("Authorization") != "" {
			t.Error("the caller's request is never modified")
		}
	}
}

func TestHealthyIsFalseBeforeAnyCall(t *testing.T) {
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	if (&GitHub{now: func() time.Time { return now }}).Healthy(now) {
		t.Fatal("no call yet, no fresh token")
	}
}

func write(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
