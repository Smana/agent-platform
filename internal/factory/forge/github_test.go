// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
 "comments":{"nodes":[{"databaseId":55,"body":"/factory retry","createdAt":"2026-09-27T10:05:00Z","lastEditedAt":null,"author":{"__typename":"User","login":"Smana"}},
   {"databaseId":56,"body":"/factory retry","createdAt":"2026-09-27T10:06:00Z","lastEditedAt":"2026-09-27T10:07:00Z","author":{"__typename":"User","login":"Smana"}}]},
 "commits":{"nodes":[{"commit":{"message":"docs: fix a link\n\nAgent-Run: 7f3cq2xz","committedDate":"2026-09-27T09:58:00Z"}}]}}}}}`

const issueJSON = `{"data":{"repository":{"issue":{"number":7,"url":"https://github.com/Smana/demo/issues/7",
 "title":"Fix the link","body":"The link in docs/a.md is broken.","state":"OPEN","lastEditedAt":null,
 "labels":{"nodes":[{"name":"factory/ready"}]},
 "timelineItems":{"nodes":[{"createdAt":"2026-09-27T09:04:00Z"}]}}}}}`

// The two sides of docs/a.md the Files test reads (task 7.2, R52).
const (
	aBase = "see [the guide](docs/old.md).\n"
	aHead = "see [the guide](docs/new.md).\n"
)

// R51's orphan scan: the newest-100 window GitHub returns, a mix of `agent/` and other heads.
// The connection takes no headRefPrefix, so the filtering is client-side; `agents/` and
// `my-agent/` must not slip through a loose match.
const agentPullsJSON = `{"data":{"repository":{"pullRequests":{"nodes":[
 {"number":34,"headRefName":"main","headRepository":{"owner":{"login":"Smana"},"name":"demo"},
  "labels":{"nodes":[{"name":"bug"}]}},
 {"number":31,"headRefName":"agent/3buqdlot","headRepository":{"owner":{"login":"Smana"},"name":"demo"},
  "labels":{"nodes":[{"name":"factory/class:docs-links"}]}},
 {"number":35,"headRefName":"agents/not-mine","headRepository":{"owner":{"login":"Smana"},"name":"demo"}},
 {"number":32,"headRefName":"agent/forkedit","headRepository":{"owner":{"login":"someone"},"name":"demo"}},
 {"number":36,"headRefName":"my-agent/looks-close","headRepository":{"owner":{"login":"Smana"},"name":"demo"}},
 {"number":33,"headRefName":"agent/gonesome","headRepository":null,
  "labels":{"nodes":[{"name":"factory/class:review"}]}}]}}}}`

const eventsJSON = `[
 {"event":"labeled","label":{"name":"factory/ready"},"actor":{"login":"someone"},"created_at":"2026-09-27T09:00:00Z"},
 {"event":"labeled","label":{"name":"bug"},"actor":{"login":"Smana"},"created_at":"2026-09-27T09:01:00Z"},
 {"event":"unlabeled","label":{"name":"factory/ready"},"actor":{"login":"ogenki-agent-factory[bot]"},"created_at":"2026-09-27T09:02:00Z"},
 {"event":"labeled","label":{"name":"factory/ready"},"actor":{"login":"Smana"},"created_at":"2026-09-27T09:03:00Z"}]`

const appID = "123456"

// rollupJSON is the merger's one-query read of a head commit (task 7.1, R16): three check runs
// in three states and one policy-bot commit status whose creator is a Bot.
const rollupJSON = `{"data":{"repository":{"pullRequest":{"commits":{"nodes":[{"commit":{"statusCheckRollup":{"contexts":{"nodes":[
   {"__typename":"CheckRun","name":"Pre-commit checks","status":"COMPLETED","conclusion":"SUCCESS"},
   {"__typename":"CheckRun","name":"Kubernetes validation","status":"IN_PROGRESS","conclusion":null},
   {"__typename":"CheckRun","name":"Security scanning","status":"COMPLETED","conclusion":"FAILURE"},
   {"__typename":"StatusContext","context":"policy-bot: main","state":"PENDING","creator":{"__typename":"Bot","login":"ogenki-merge-gate"}}
 ]}}}}]}}}}}`

// fakeGitHub is GitHub for one App installed on Smana/demo: it verifies the App JWT with the
// current public key, mints installation tokens, and serves the REST and GraphQL calls only to
// a request carrying the current token.
type fakeGitHub struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	pub    *rsa.PublicKey
	token  string
	mints  int
	ttl    time.Duration
	now    func() time.Time
	calls  []string
	bodies []string                         // the POST /graphql bodies, in order
	mint   func(w http.ResponseWriter) bool // overrides the mint reply when it returns true
	// TW4: the scope the next mint must request; every other request gets GitHub's refusal.
	// perms records each minted body's scope, in order, so a test can pin it per connection.
	wantPerms map[string]string
	perms     []map[string]string
}

func (g *fakeGitHub) setWantPerms(p map[string]string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.wantPerms = p
}

// mintedPerms are the permission maps of the mints served so far, in order.
func (g *fakeGitHub) mintedPerms() []map[string]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.perms)
}

func newFakeGitHub(t *testing.T, pub *rsa.PublicKey, now func() time.Time) *fakeGitHub {
	g := &fakeGitHub{t: t, pub: pub, ttl: time.Hour, now: now, wantPerms: permissions()}
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
		g.mu.Lock()
		want := g.wantPerms
		g.mu.Unlock()
		// GitHub 422s a token asking beyond the App's installation scope; so does this.
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Repositories) != 1 || in.Repositories[0] != "demo" ||
			!maps.Equal(in.Permissions, want) {
			http.Error(w, `{"message":"Unprocessable Entity"}`, http.StatusUnprocessableEntity)
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.mint != nil && g.mint(w) {
			return
		}
		g.perms = append(g.perms, maps.Clone(in.Permissions))
		g.mints++
		g.token = "ghs_" + strings.Repeat("x", g.mints)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": g.token, "expires_at": g.now().Add(g.ttl).Format(time.RFC3339)})
	})
	mux.HandleFunc("POST /graphql", g.authed(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.bodies = append(g.bodies, string(b))
		g.mu.Unlock()
		// The merger's rollup and mutations (task 7.1), routed before the snapshot tests:
		// the rollup query also selects pullRequest(number: $number).
		switch {
		case strings.Contains(string(b), "statusCheckRollup"):
			_, _ = io.WriteString(w, rollupJSON)
			return
		case strings.Contains(string(b), "mergePullRequest(input"):
			switch {
			case strings.Contains(string(b), `"expectedHeadOid":"def456"`):
				w.WriteHeader(http.StatusConflict) // GitHub checks the head at merge time (R52)
				_, _ = io.WriteString(w, `{"errors":[{"message":"Head sha does not match expected"}]}`)
			case strings.Contains(string(b), `"expectedHeadOid":"blocked"`):
				w.WriteHeader(http.StatusMethodNotAllowed)
				_, _ = io.WriteString(w, `{"errors":[{"message":"Merge pull request not allowed"}]}`)
			default:
				_, _ = io.WriteString(w, `{"data":{"mergePullRequest":{"clientMutationId":null}}}`)
			}
			return
		case strings.Contains(string(b), "revertPullRequest"):
			_, _ = io.WriteString(w, `{"data":{"revertPullRequest":{"revertPullRequest":{"id":"PR_rev","number":13,"url":"https://github.com/Smana/demo/pull/13"}}}}`)
			return
		case strings.Contains(string(b), "enablePullRequestAutoMerge"):
			_, _ = io.WriteString(w, `{"data":{"enablePullRequestAutoMerge":{"clientMutationId":null}}}`)
			return
		case strings.Contains(string(b), "disablePullRequestAutoMerge"):
			_, _ = io.WriteString(w, `{"data":{"disablePullRequestAutoMerge":{"clientMutationId":null}}}`)
			return
		}
		if strings.Contains(string(b), "pullRequests(") {
			if strings.Contains(string(b), "headRefPrefix") || !strings.Contains(string(b), "states: [OPEN]") {
				g.t.Errorf("the orphan scan must not send headRefPrefix (GitHub rejects it on the pullRequests connection) and must keep states: [OPEN]: %s", b)
			}
			_, _ = io.WriteString(w, agentPullsJSON)
			return
		}
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
		q := r.URL.Query()
		if q.Get("labels") == "factory/revert" {
			if q.Get("state") != "all" {
				t.Errorf("revert labels must list closed pull requests too: %v", q)
			}
			_, _ = io.WriteString(w, `[{"number":901,"pull_request":{"url":"x"}}]`)
			return
		}
		if q.Get("labels") != "factory/ready" || q.Get("state") != "open" {
			t.Errorf("query %v", q)
		}
		_, _ = io.WriteString(w, `[{"number":7},{"number":12,"pull_request":{"url":"x"}}]`)
	}))
	mux.HandleFunc("GET /repos/Smana/demo/commits/{sha}/check-runs", g.authed(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"total_count":1,"check_runs":[{"name":"Pre-commit checks","status":"completed","conclusion":"success"}]}`)
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
	// An issue's comments as GitHub serves them: ascending id, paged, sort and direction ignored
	// (review R1). Issue 7 has one comment; 9, 10 and 11 have 130, 180 and 60; 13 has two, the
	// second edited after it was posted.
	mux.HandleFunc("GET /repos/Smana/demo/issues/{n}/comments", g.authed(func(w http.ResponseWriter, r *http.Request) {
		total := map[string]int{"7": 1, "9": 130, "10": 180, "11": 60, "13": 2}[r.PathValue("n")]
		q := r.URL.Query()
		per, _ := strconv.Atoi(q.Get("per_page"))
		page, _ := strconv.Atoi(q.Get("page"))
		if per == 0 {
			per = 30
		}
		page = max(page, 1)
		last := max((total+per-1)/per, 1)
		link := func(p int, rel string) string {
			return fmt.Sprintf(`<%s/repos/Smana/demo/issues/%s/comments?per_page=%d&page=%d>; rel="%s"`, g.srv.URL, r.PathValue("n"), per, p, rel)
		}
		var rels []string
		if page < last {
			rels = append(rels, link(page+1, "next"), link(last, "last"))
		}
		if page > 1 {
			rels = append(rels, link(1, "first"), link(page-1, "prev"))
		}
		if len(rels) > 0 {
			w.Header().Set("Link", strings.Join(rels, ", "))
		}
		var cs []string
		for id := (page-1)*per + 1; id <= min(page*per, total); id++ {
			updated := "2026-09-27T10:00:00Z"
			if r.PathValue("n") == "13" && id == 2 {
				updated = "2026-09-27T11:00:00Z"
			}
			cs = append(cs, fmt.Sprintf(`{"id":%d,"body":"c%d","user":{"login":"Smana"},"created_at":"2026-09-27T10:00:00Z","updated_at":%q}`,
				id, id, updated))
		}
		_, _ = io.WriteString(w, "["+strings.Join(cs, ",")+"]")
	}))
	mux.HandleFunc("PATCH /repos/Smana/demo/pulls/{n}", g.authed(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if len(in) != 1 || in["state"] != "closed" {
			t.Errorf("a close changes the state only: %v", in)
		}
		if r.PathValue("n") != "12" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"number":12,"state":"closed"}`)
	}))
	mux.HandleFunc("POST /repos/Smana/demo/issues/7/labels", g.authed(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	}))
	mux.HandleFunc("GET /repos/Smana/demo/compare/{basehead}", g.authed(func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("basehead") {
		case "old...new": // one modified file: both sides have content.
			_, _ = io.WriteString(w, `{"files":[{"filename":"docs/a.md","status":"modified"}]}`)
		case "old...mid": // a file over the contents API's megabyte: GitHub lists it without content.
			_, _ = io.WriteString(w, `{"files":[{"filename":"docs/big.png","status":"modified"}]}`)
		default: // mid...new: exactly one page, so more changed files are unread.
			var fs []string
			for i := range 100 {
				fs = append(fs, fmt.Sprintf(`{"filename":"f%03d.md","status":"modified"}`, i))
			}
			_, _ = io.WriteString(w, `{"files":[`+strings.Join(fs, `,`)+`]}`)
		}
	}))
	mux.HandleFunc("GET /repos/Smana/demo/contents/{path...}", g.authed(func(w http.ResponseWriter, r *http.Request) {
		p, ref := r.PathValue("path"), r.URL.Query().Get("ref")
		switch {
		case p == "docs/a.md" && (ref == "old" || ref == "new"):
			text := aBase
			if ref == "new" {
				text = aHead
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "a.md", "path": p, "type": "file",
				"encoding": "base64", "size": len(text), "content": base64.StdEncoding.EncodeToString([]byte(text))})
		case p == "docs/big.png":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "big.png", "path": p, "type": "file", "size": 2_000_000})
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
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

// graphqlBodies are the bodies of the GraphQL calls so far, in order, for tests that assert on
// what was actually sent.
func (g *fakeGitHub) graphqlBodies() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.bodies)
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
	if pr.Trailer("Agent-Run") != "7f3cq2xz" || !pr.HeadCommittedAt.Equal(time.Date(2026, 9, 27, 9, 58, 0, 0, time.UTC)) {
		t.Fatal("the head commit's Agent-Run trailer and date")
	}
	if len(pr.Reviews) != 1 || pr.Reviews[0].Author != "Smana" || pr.Reviews[0].ID != 901 || pr.Reviews[0].Comments[0].Path != "docs/a.md" ||
		pr.Reviews[0].Comments[0].Line != 3 || pr.Reviews[0].At.IsZero() {
		t.Fatalf("reviews %+v", pr.Reviews)
	}
	if len(pr.Comments) != 2 || pr.Comments[0].ID != 55 || pr.Comments[0].Author != "Smana" || pr.Comments[0].Edited || !pr.Comments[1].Edited {
		t.Fatalf("comments, and which were edited: %+v", pr.Comments)
	}
}

// R51: the orphan scan's list is the open agent-branch pull requests of this repository, with
// their labels; a fork's branch and a deleted head repository are never the factory's. The stub
// window mixes in non-agent heads (main, agents/, my-agent/) because GitHub's connection cannot
// filter them; none may reach the caller.
func TestAgentPullsAreTheOwnOnes(t *testing.T) {
	r := newRig(t)
	ps, err := r.g.AgentPulls(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 3 {
		t.Fatalf("%+v", ps)
	}
	for _, p := range ps {
		if !strings.HasPrefix(p.HeadRef, "agent/") {
			t.Fatalf("a non-agent head survived the client-side filter: %+v", p)
		}
	}
	if ps[0].Number != 31 || ps[0].HeadRef != "agent/3buqdlot" || ps[0].Fork || len(ps[0].Labels) != 1 ||
		ps[0].Labels[0] != "factory/class:docs-links" {
		t.Fatalf("the own one, with its labels: %+v", ps[0])
	}
	if !ps[1].Fork || !ps[2].Fork {
		t.Fatalf("a fork's and a deleted head's are marked: %+v %+v", ps[1], ps[2])
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

// Review R1: GitHub lists an issue's comments oldest first and ignores sort and direction, so the
// newest 50 are on the last pages. A marker posted after the 50th comment must still be found.
func TestRecentCommentsAreTheNewest(t *testing.T) {
	r := newRig(t)
	// A short last page, a full one, and one page of more than 50.
	for issue, total := range map[int]int64{9: 130, 10: 180, 11: 60} {
		cs, err := r.g.RecentComments(t.Context(), issue)
		if err != nil || len(cs) != 50 {
			t.Fatalf("#%d: %d %v", issue, len(cs), err)
		}
		for i, c := range cs {
			if want := total - int64(i); c.ID != want {
				t.Fatalf("#%d: comment %d is id %d, want %d (newest first)", issue, i, c.ID, want)
			}
		}
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
	if err != nil || len(cs) != 1 || cs[0].ID != 1 || cs[0].Author != "Smana" || cs[0].Body != "c1" || cs[0].Edited {
		t.Fatalf("%+v %v", cs, err)
	}
	if cs, err = r.g.RecentComments(ctx, 13); err != nil || len(cs) != 2 || !cs[0].Edited || cs[1].Edited {
		t.Fatalf("an edit shows: %+v %v", cs, err)
	}
	if err := r.g.ClosePR(ctx, 12); err != nil {
		t.Fatal(err)
	}
	if err := r.g.ClosePR(ctx, 13); err == nil {
		t.Fatal("a refused close is an error")
	}
	if err := r.g.AddLabels(ctx, 7, "factory/class:docs-links"); err != nil {
		t.Fatal(err)
	}
	if err := r.g.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

// Task 7.1 (R16): the merger reads the head rollup and a commit's check runs, arms squash
// auto-merge pinned to the decided head, and opens reverts.
func TestChecksAndMutations(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()

	c, err := r.g.PullRequestChecks(ctx, 12)
	if err != nil {
		t.Fatal(err)
	}
	want := []Check{{"Pre-commit checks", "SUCCESS"}, {"Kubernetes validation", "PENDING"}, {"Security scanning", "FAILURE"}}
	if len(c.Runs) != 3 || c.Runs[0] != want[0] || c.Runs[1] != want[1] || c.Runs[2] != want[2] {
		t.Fatalf("%+v", c.Runs)
	}
	if st, by := c.StatusOf("policy-bot: main"); st != "PENDING" || by != "ogenki-merge-gate[bot]" {
		t.Fatalf("%s %s", st, by)
	}
	// Arming stays for the revert path (R52): expectedHeadOid still pins it when a caller gives one.
	if err := r.g.EnableAutoMerge(ctx, "PR_1", "abc123"); err != nil {
		t.Fatal(err)
	}
	bodies := r.gh.graphqlBodies()
	if len(bodies) != 2 { // the rollup query, then the arm
		t.Fatalf("%d graphql calls", len(bodies))
	}
	if !strings.Contains(bodies[1], "SQUASH") || !strings.Contains(bodies[1], `"expectedHeadOid":"abc123"`) {
		t.Fatalf("squash auto-merge of the decided head only: %s", bodies[1])
	}
	rv, err := r.g.RevertPR(ctx, "PR_1", `Revert "docs: fix a link"`, "reverts #12")
	if err != nil || rv.Number != 13 || rv.NodeID != "PR_rev" {
		t.Fatalf("%+v %v", rv, err)
	}
	runs, err := r.g.CommitChecks(ctx, "abc123")
	if err != nil || len(runs) != 1 || runs[0].State != "SUCCESS" {
		t.Fatalf("%+v %v", runs, err)
	}
}

// R52: the decision merges now, with expectedHeadOid — GitHub checks the head at merge time, so
// a 409 is the head moving and a 405 a merge it will not allow as it stands.
func TestMergeChecksTheHeadAtMergeTime(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	if err := r.g.Merge(ctx, "PR_1", "abc123"); err != nil {
		t.Fatal(err)
	}
	bodies := r.gh.graphqlBodies()
	if len(bodies) != 1 {
		t.Fatalf("%d graphql calls", len(bodies))
	}
	if !strings.Contains(bodies[0], "mergePullRequest(input") || !strings.Contains(bodies[0], "SQUASH") ||
		!strings.Contains(bodies[0], `"expectedHeadOid":"abc123"`) {
		t.Fatalf("squash merge of the decided head only: %s", bodies[0])
	}
	if err := r.g.Merge(ctx, "PR_1", "def456"); !errors.Is(err, ErrHeadMoved) {
		t.Fatalf("a moved head: %v", err)
	}
	if err := r.g.Merge(ctx, "PR_1", "blocked"); !errors.Is(err, ErrNotMergeable) {
		t.Fatalf("a refused merge: %v", err)
	}
	if err := r.g.Merge(ctx, "PR_1", ""); err == nil {
		t.Fatal("a merge without the decided head would merge whatever landed since")
	}
}

// §6.4: a maintainer's factory/revert lands on merged pull requests, which are closed, so the
// forge lists that one label in every state.
func TestRevertLabelListsClosedPullRequests(t *testing.T) {
	r := newRig(t)
	items, err := r.g.Labeled(t.Context(), "factory/revert")
	if err != nil || len(items) != 1 || !items[0].PullRequest || items[0].Number != 901 {
		t.Fatalf("%+v %v", items, err)
	}
}

// Task 7.2 (R52): Files reads both sides of a changed diff, and refuses a comparison it
// cannot see whole — a file too large for the contents API, a diff past one page.
func TestFilesReadsBothSidesOfADiff(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	b, h, err := r.g.Files(ctx, "old", "new")
	if err != nil {
		t.Fatal(err)
	}
	if b["docs/a.md"] != aBase || h["docs/a.md"] != aHead {
		t.Fatalf("base %q head %q", b["docs/a.md"], h["docs/a.md"])
	}
	if _, _, err := r.g.Files(ctx, "old", "mid"); err == nil || !strings.Contains(err.Error(), "big.png") {
		t.Errorf("an unfetchable file must be an error, not a skip: %v", err)
	}
	if _, _, err := r.g.Files(ctx, "mid", "new"); err == nil || !strings.Contains(err.Error(), "page") {
		t.Errorf("a truncated compare must be an error: %v", err)
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

// TW4 (the 7.4 review): the mint body is pinned per connection. The factory connection keeps
// its App's scope; the merger connection carries the merger profile — checks and statuses read,
// contents and pull requests write — and a mint asking beyond the App's scope fails, as GitHub
// refuses it (the fail-loud contract of app.go's permissions()).
func TestEachConnectionMintsItsOwnScope(t *testing.T) {
	r := newRig(t) // the factory connection: its mint pins the factory scope
	r.gh.setWantPerms(MergerPermissions())
	o := r.opts
	o.Permissions = MergerPermissions()
	if _, err := Connect(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	got := r.gh.mintedPerms()
	if len(got) != 2 {
		t.Fatalf("two connections, two mints: %d", len(got))
	}
	if !maps.Equal(got[0], permissions()) {
		t.Fatalf("the factory connection mints its App's scope: %v", got[0])
	}
	if !maps.Equal(got[1], MergerPermissions()) {
		t.Fatalf("the merger connection mints the merger profile: %v", got[1])
	}
}

func TestATokenBeyondTheAppsScopeFailsToMint(t *testing.T) {
	r := newRig(t)
	o := r.opts
	o.Permissions = MergerPermissions() // checks:read is beyond the factory App
	if _, err := Connect(t.Context(), o); err == nil || !strings.Contains(err.Error(), "mint an installation token") {
		t.Fatalf("a token beyond the App's scope must fail the mint: %v", err)
	}
}
