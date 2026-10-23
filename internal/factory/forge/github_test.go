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
	"net/url"
	"os"
	"path/filepath"
	"reflect"
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
 "author":{"__typename":"User","login":"dev1"},
 "labels":{"nodes":[{"name":"factory/ready"}]},
 "timelineItems":{"nodes":[{"createdAt":"2026-09-27T09:04:00Z"}]}}}}}`

// The two sides of docs/a.md the Files test reads (task 7.2, R52).
const (
	aBase = "see [the guide](docs/old.md).\n"
	aHead = "see [the guide](docs/new.md).\n"
)

// cloud-native-ref#2515: the PR retargets one link in docs/b.md; after its branch was cut, main
// took another PR's change to the same file. Main's tip is not what the PR changes.
const (
	bFork = "see [the guide](docs/old.md).\n\n    url: http://a\n"
	bTip  = "see [the guide](docs/old.md).\n\n    url: http://b\n"
	bHead = "see [the guide](docs/new.md).\n\n    url: http://a\n"
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

// The breaker's ledger read (v2 D1): pull requests carrying one label, newest first.
const labelledJSON = `{"data":{"repository":{"pullRequests":{"nodes":[
 {"number":41,"state":"MERGED","mergedAt":"2026-10-15T09:00:00Z"},
 {"number":40,"state":"OPEN","mergedAt":null}]}}}}`

// The follow-up reads: merged pull requests found by search, a bot author without its suffix.
const mergedJSON = `{"data":{"search":{"nodes":[
 {"number":51,"author":{"__typename":"Bot","login":"renovate"},"headRefName":"renovate/x","title":"chore(deps): bump x",
  "mergedAt":"2026-10-17T08:00:00Z","mergeCommit":{"oid":"m51"},"labels":{"nodes":[{"name":"factory/merged:deps-patch"},{"name":"dependencies"}]}},
 {"number":52,"author":{"__typename":"User","login":"Smana"},"headRefName":"fix/y","title":"fix: y",
  "mergedAt":"2026-10-18T08:00:00Z","mergeCommit":null,"labels":{"nodes":[]}}]}}}`

const blameJSON = `{"data":{"repository":{"object":{"parents":{"nodes":[{"blame":{"ranges":[
 {"startingLine":1,"endingLine":9,"commit":{"oid":"aaa"}},{"startingLine":10,"endingLine":12,"commit":{"oid":"bbb"}}]}}]}}}}}`

const eventsJSON = `[
 {"event":"labeled","label":{"name":"factory/ready"},"actor":{"login":"someone"},"created_at":"2026-09-27T09:00:00Z"},
 {"event":"labeled","label":{"name":"bug"},"actor":{"login":"Smana"},"created_at":"2026-09-27T09:01:00Z"},
 {"event":"unlabeled","label":{"name":"factory/ready"},"actor":{"login":"ogenki-agent-factory[bot]"},"created_at":"2026-09-27T09:02:00Z"},
 {"event":"labeled","label":{"name":"factory/ready"},"actor":{"login":"Smana"},"created_at":"2026-09-27T09:03:00Z"}]`

const appID = "123456"

// rollupJSON is the merger's one-query read of a head commit (task 7.1, R16): three check runs
// in three states and one policy-bot commit status whose creator is a Bot.
const rollupJSON = `{"data":{"repository":{"pullRequest":{"commits":{"nodes":[{"commit":{"statusCheckRollup":{"contexts":{"nodes":[
   {"__typename":"CheckRun","name":"Pre-commit checks","status":"COMPLETED","conclusion":"SUCCESS","checkSuite":{"app":{"databaseId":15368}}},
   {"__typename":"CheckRun","name":"Kubernetes validation","status":"IN_PROGRESS","conclusion":null},
   {"__typename":"CheckRun","name":"Security scanning","status":"COMPLETED","conclusion":"FAILURE"},
   {"__typename":"CheckRun","name":"External links","status":"COMPLETED","conclusion":"SKIPPED","checkSuite":{"app":{"databaseId":15368}}},
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
	// fence is .github/agent-factory.yaml on the default branch: "" is absent, "!" a 500.
	fence string
	steer map[string]string // default-branch steering notes by path; "!" fails the read
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
			case strings.Contains(string(b), `"expectedHeadOid":"text409"`):
				// 200 with GraphQL errors whose text mentions a status: not a conflict.
				_, _ = io.WriteString(w, `{"errors":[{"message":"build 4095 of 405 failed with 409 files"}]}`)
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
		switch {
		case strings.Contains(string(b), "search(type: ISSUE"):
			var in struct{ Variables map[string]any }
			_ = json.Unmarshal(b, &in)
			q, _ := in.Variables["q"].(string)
			if strings.Contains(q, "merged:>=2020-01-01") { // a window of 100: the search may hold more
				var ns []string
				for i := range 100 {
					ns = append(ns, fmt.Sprintf(`{"number":%d,"headRefName":"x","title":"t","mergedAt":"2026-10-16T10:00:00Z"}`, i+1))
				}
				_, _ = io.WriteString(w, `{"data":{"search":{"nodes":[`+strings.Join(ns, ",")+`]}}}`)
				return
			}
			if strings.Contains(q, "merged:>=2021-01-01") { // a pull request with more labels than were read
				_, _ = io.WriteString(w, `{"data":{"search":{"nodes":[{"number":9,"headRefName":"x","title":"t",`+
					`"labels":{"nodes":[{"name":"a"}],"pageInfo":{"hasNextPage":true}}}]}}}`)
				return
			}
			_, _ = io.WriteString(w, mergedJSON)
			return
		case strings.Contains(string(b), "blame(path:"):
			var in struct{ Variables map[string]any }
			_ = json.Unmarshal(b, &in)
			switch in.Variables["path"] {
			case "gone.yaml": // blame null
				_, _ = io.WriteString(w, `{"data":{"repository":{"object":{"parents":{"nodes":[{"blame":null}]}}}}}`)
			case "boom.yaml":
				_, _ = io.WriteString(w, `{"errors":[{"message":"Something went wrong"}]}`)
			case "missing.yaml":
				_, _ = io.WriteString(w, `{"errors":[{"type":"NOT_FOUND","message":"No such path in tree: missing.yaml"}]}`)
			default:
				_, _ = io.WriteString(w, blameJSON)
			}
			return
		}
		if strings.Contains(string(b), "pullRequests(labels: $labels") {
			_, _ = io.WriteString(w, labelledJSON)
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
	// v2 D4/D5: a login's permission on the repository, and a user id's current login.
	mux.HandleFunc("GET /repos/Smana/demo/collaborators/{login}/permission", g.authed(func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("login") {
		case "writer":
			_, _ = io.WriteString(w, `{"permission":"write","user":{"login":"writer"}}`)
		case "flaky":
			http.Error(w, `{"message":"Server Error"}`, http.StatusInternalServerError)
		case "empty":
			_, _ = io.WriteString(w, `{"permission":""}`)
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	mux.HandleFunc("GET /user/{id}", g.authed(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "583231" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"login":"octocat","id":583231}`)
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
		if q.Get("labels") == "factory/child-of:#9" { // a list longer than one page
			w.Header().Set("Link", `<https://api.github.com/repos/Smana/demo/issues?page=2>; rel="next"`)
			_, _ = io.WriteString(w, `[{"number":41,"id":4101,"state":"open"}]`)
			return
		}
		if q.Get("labels") == "factory/child-of:#7" {
			if q.Get("state") != "all" {
				t.Errorf("a split's parts are read open or closed: %v", q)
			}
			_, _ = io.WriteString(w, `[{"number":31,"id":3101,"state":"open","title":"Part 1 of 2 of #7",`+
				`"body":"…\n<!-- agent-factory split=3buqdlot/1 -->","user":{"login":"ogenki-agent-factory[bot]"},`+
				`"labels":[{"name":"factory/child-of:#7"}],"html_url":"https://github.com/Smana/demo/issues/31"},`+
				`{"number":12,"pull_request":{"url":"x"}}]`)
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
	mux.HandleFunc("POST /repos/Smana/demo/issues", g.authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Title, Body string
			Labels      []string
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Title == "refused" {
			http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
			return
		}
		if in.Title != "Part 1 of 2 of #7" || !slices.Equal(in.Labels, []string{"factory/child-of:#7", "class:docs"}) {
			t.Errorf("create %+v", in)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 31, "id": 3101, "state": "open", "title": in.Title, "body": in.Body,
			"user": map[string]any{"login": "ogenki-agent-factory[bot]"}, "html_url": "https://github.com/Smana/demo/issues/31",
			"labels": []map[string]string{{"name": in.Labels[0]}, {"name": in.Labels[1]}}})
	}))
	mux.HandleFunc("POST /repos/Smana/demo/issues/{n}/sub_issues", g.authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			SubIssueID int64 `json:"sub_issue_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if r.PathValue("n") == "8" {
			http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
			return
		}
		if r.PathValue("n") != "7" || in.SubIssueID != 3101 {
			t.Errorf("sub-issue %s %+v", r.PathValue("n"), in)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":3101,"number":31}`)
	}))
	mux.HandleFunc("PATCH /repos/Smana/demo/issues/{n}", g.authed(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if r.PathValue("n") == "8" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		if r.PathValue("n") != "7" || in["state"] != "closed" || in["state_reason"] != "completed" {
			t.Errorf("close %s %v", r.PathValue("n"), in)
		}
		_, _ = io.WriteString(w, `{"number":7,"state":"closed"}`)
	}))
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
	// #76: a branch's activity, newest first, cursor-paged. agent/3buqdlot was created, pushed 119
	// times, and an older life of the branch lies past its creation on page 2; agent/ghost has an
	// actor GitHub no longer names; agent/endless never reaches a creation; agent/broken fails.
	mux.HandleFunc("GET /repos/Smana/demo/activity", g.authed(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("per_page") != "100" || q.Get("direction") == "asc" {
			t.Errorf("activity is read newest first, 100 a page: %s", r.URL.RawQuery)
		}
		next := func(cursor string) {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repositories/1/activity?per_page=100&ref=%s&after=%s>; rel="next"`,
				g.srv.URL, url.QueryEscape(q.Get("ref")), cursor))
		}
		ev := func(i int, typ, actor string) string {
			before, a := fmt.Sprintf("c%03d", i-1), fmt.Sprintf(`{"login":%q,"type":"Bot"}`, actor)
			if typ == "branch_creation" {
				before = "0000000000000000000000000000000000000000"
			}
			if actor == "" {
				a = "null"
			}
			return fmt.Sprintf(`{"id":%d,"activity_type":%q,"ref":%q,"before":%q,"after":"c%03d","timestamp":"2026-10-07T19:%02d:00Z","actor":%s}`,
				i, typ, q.Get("ref"), before, i, i%60, a)
		}
		var evs []string
		switch q.Get("ref") + "@" + q.Get("after") {
		case "refs/heads/agent/3buqdlot@":
			for i := 119; i >= 20; i-- {
				evs = append(evs, ev(i, "push", "ogenki-agents[bot]"))
			}
			next("p2")
		case "refs/heads/agent/3buqdlot@p2":
			for i := 19; i >= 1; i-- {
				evs = append(evs, ev(i, "push", "ogenki-agents[bot]"))
			}
			evs = append(evs, ev(0, "branch_creation", "ogenki-agents[bot]"), ev(900, "branch_deletion", "Smana"))
			next("p3")
		case "refs/heads/agent/ghost@":
			evs = append(evs, ev(1, "force_push", ""), ev(0, "branch_creation", "ogenki-agents[bot]"))
		case "refs/heads/agent/broken@":
			http.Error(w, `{"message":"Server Error"}`, http.StatusInternalServerError)
			return
		default:
			if !strings.HasPrefix(q.Get("ref"), "refs/heads/agent/endless") {
				t.Errorf("read past the branch's creation: %s", r.URL.RawQuery)
			}
			for i := 100; i >= 1; i-- {
				evs = append(evs, ev(i, "push", "ogenki-agents[bot]"))
			}
			next(q.Get("after") + "n")
		}
		_, _ = io.WriteString(w, "["+strings.Join(evs, ",")+"]")
	}))
	// A pull request's commits, oldest first, paged, and never past GitHub's 250: PR 12 has 130,
	// PR 14 has 300, of which GitHub serves only the first 250.
	mux.HandleFunc("GET /repos/Smana/demo/pulls/{n}/commits", g.authed(func(w http.ResponseWriter, r *http.Request) {
		total := min(map[string]int{"12": 130, "14": 300}[r.PathValue("n")], 250)
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if per == 0 {
			per = 30
		}
		page = max(page, 1)
		if page*per < total {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/Smana/demo/pulls/%s/commits?per_page=%d&page=%d>; rel="next"`,
				g.srv.URL, r.PathValue("n"), per, page+1))
		}
		var cs []string
		for i := (page-1)*per + 1; i <= min(page*per, total); i++ {
			cs = append(cs, fmt.Sprintf(`{"sha":"c%03d","author":{"login":"ogenki-agents[bot]"},"commit":{"message":"fix %d\n\nAgent-Run: 7f3cq2xz"}}`, i, i))
		}
		_, _ = io.WriteString(w, "["+strings.Join(cs, ",")+"]")
	}))
	// A pull request's files, 100 a page: #20 has two, #21 has GitHub's 3,000 and more unlisted.
	mux.HandleFunc("GET /repos/Smana/demo/pulls/{n}/files", g.authed(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("n") == "20" {
			_, _ = io.WriteString(w, `[{"filename":"a.yaml","status":"modified","changes":2,"patch":"@@ -1 +1 @@\n-a\n+b"},`+
				`{"filename":"new.yaml","previous_filename":"old.yaml","status":"renamed"},`+
				`{"filename":"big.yaml","status":"modified","changes":9000},{"filename":"img.png","status":"modified"},`+
				`{"filename":"empty.yaml","status":"added"}]`)
			return
		}
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(page, 1)
		if page*per < 3000 {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/Smana/demo/pulls/21/files?per_page=%d&page=%d>; rel="next"`, g.srv.URL, per, page+1))
		}
		var fs []string
		for i := (page-1)*per + 1; i <= min(page*per, 3000); i++ {
			fs = append(fs, fmt.Sprintf(`{"filename":"f%04d.yaml","status":"modified"}`, i))
		}
		_, _ = io.WriteString(w, "["+strings.Join(fs, ",")+"]")
	}))
	mux.HandleFunc("POST /repos/Smana/demo/issues/7/labels", g.authed(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	}))
	mux.HandleFunc("GET /repos/Smana/demo/compare/{basehead}", g.authed(func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("basehead") {
		case "old...new": // one modified file: both sides have content.
			_, _ = io.WriteString(w, `{"merge_base_commit":{"sha":"old"},"files":[{"filename":"docs/a.md","status":"modified","additions":2,"deletions":1}]}`)
		case "old...mid": // a file over the contents API's megabyte: GitHub lists it without content.
			_, _ = io.WriteString(w, `{"merge_base_commit":{"sha":"old"},"files":[{"filename":"docs/big.png","status":"modified"}]}`)
		case "main...behind": // a branch cut at "fork", before main moved on: three-dot lists its own change.
			_, _ = io.WriteString(w, `{"merge_base_commit":{"sha":"fork"},"files":[{"filename":"docs/b.md","status":"modified"}]}`)
		case "main...nobase": // a compare without a merge base cannot say which side is the PR's.
			_, _ = io.WriteString(w, `{"files":[{"filename":"docs/b.md","status":"modified"}]}`)
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
		case p == "docs/b.md" && (ref == "fork" || ref == "main" || ref == "behind"):
			text := map[string]string{"fork": bFork, "main": bTip, "behind": bHead}[ref]
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "b.md", "path": p, "type": "file",
				"encoding": "base64", "size": len(text), "content": base64.StdEncoding.EncodeToString([]byte(text))})
		case p == ".github/agent-factory.yaml" && ref == "":
			g.mu.Lock()
			body := g.fence
			g.mu.Unlock()
			switch body {
			case "":
				http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			case "!":
				http.Error(w, `{"message":"Server Error"}`, http.StatusInternalServerError)
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"name": "agent-factory.yaml", "path": p, "type": "file",
					"encoding": "base64", "size": len(body), "content": base64.StdEncoding.EncodeToString([]byte(body))})
			}
		case strings.HasPrefix(p, ".github/agent-factory/steering/") && ref == "":
			g.mu.Lock()
			body, ok := g.steer[p]
			g.mu.Unlock()
			switch {
			case body == "big":
				_ = json.NewEncoder(w).Encode(map[string]any{"name": "x.md", "path": p, "type": "file", "sha": "5e1f0c2a9b8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f",
					"encoding": "none", "size": 2 << 20, "content": ""})
			case body == "dir":
				_ = json.NewEncoder(w).Encode([]map[string]any{{"name": "x.md", "path": p + "/x.md", "type": "file"}})
			case body == "symlink" || body == "submodule":
				_ = json.NewEncoder(w).Encode(map[string]any{"name": "x.md", "path": p, "type": body, "sha": "5e1f0c2a9b8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f",
					"size": 12, "target": "../elsewhere.md"})
			case body == "linked":
				_ = json.NewEncoder(w).Encode(map[string]any{"name": "other.md", "path": "docs/other.md", "type": "file", "sha": "5e1f0c2a9b8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f",
					"encoding": "base64", "size": 2, "content": base64.StdEncoding.EncodeToString([]byte("x\n"))})
			case body == "denied":
				http.Error(w, `{"message":"Forbidden"}`, http.StatusForbidden)
			case !ok:
				http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			case body == "!":
				http.Error(w, `{"message":"Server Error"}`, http.StatusInternalServerError)
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"name": "x.md", "path": p, "type": "file", "sha": "5e1f0c2a9b8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f",
					"encoding": "base64", "size": len(body), "content": base64.StdEncoding.EncodeToString([]byte(body))})
			}
		case p == "docs/big.png":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "big.png", "path": p, "type": "file", "size": 2_000_000})
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	mux.HandleFunc("POST /repos/Smana/demo/actions/jobs/{id}/rerun", g.authed(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "403" {
			http.Error(w, `{"message":"Workflow run is still in progress"}`, http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
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

func TestPullsWithLabelReadsEveryStateWithItsMergeTime(t *testing.T) {
	r := newRig(t)
	ps, err := r.g.PullsWithLabel(t.Context(), "factory/auto-merged:docs-links")
	if err != nil {
		t.Fatal(err)
	}
	want := []LabelledPull{{Number: 41, State: "MERGED", MergedAt: time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)}, {Number: 40, State: "OPEN"}}
	if !slices.Equal(ps, want) {
		t.Fatalf("%+v", ps)
	}
	if b := r.gh.graphqlBodies(); !strings.Contains(b[len(b)-1], `"labels":["factory/auto-merged:docs-links"]`) {
		t.Fatalf("the label is a variable, never spliced into the query: %s", b[len(b)-1])
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

// #74: every commit of the pull request, across pages, with its author's login and message.
func TestPullRequestCommitsReadsEveryPage(t *testing.T) {
	r := newRig(t)
	cs, err := r.g.PullRequestCommits(t.Context(), 12)
	if err != nil || len(cs) != 130 {
		t.Fatalf("%d %v", len(cs), err)
	}
	if c := cs[129]; c.SHA != "c130" || c.Author != "ogenki-agents[bot]" || c.Trailer("Agent-Run") != "7f3cq2xz" {
		t.Fatalf("%+v", c)
	}
}

// GitHub serves at most 250 of a pull request's commits: a list that reaches the cap may be
// partial, so the caller is told rather than handed it as the whole.
func TestPullRequestCommitsAtGitHubsCapAreTruncated(t *testing.T) {
	r := newRig(t)
	cs, err := r.g.PullRequestCommits(t.Context(), 14)
	if !errors.Is(err, ErrCommitsTruncated) || len(cs) != 250 {
		t.Fatalf("%d %v", len(cs), err)
	}
}

// #76: a branch's activity, newest first, across cursor pages, back to its latest creation and no
// further: what came before is an earlier branch of the same name.
func TestBranchActivityReadsBackToTheCreation(t *testing.T) {
	r := newRig(t)
	ps, err := r.g.BranchActivity(t.Context(), "agent/3buqdlot")
	if err != nil || len(ps) != 120 {
		t.Fatalf("%d %v", len(ps), err)
	}
	if p := ps[0]; p.Type != "push" || p.Actor != "ogenki-agents[bot]" || p.Before != "c118" || p.After != "c119" ||
		!p.At.Equal(time.Date(2026, 10, 7, 19, 59, 0, 0, time.UTC)) {
		t.Fatalf("%+v", p)
	}
	if p := ps[119]; p.Type != "branch_creation" || p.After != "c000" {
		t.Fatalf("the creation is the last read: %+v", p)
	}
	if ps, err = r.g.BranchActivity(t.Context(), "agent/ghost"); err != nil || ps[0].Actor != "" || ps[0].Type != "force_push" {
		t.Fatalf("an actor GitHub does not name reads as none: %+v %v", ps, err)
	}
}

// A branch whose creation is past the page cap, or a read GitHub refuses, is never a whole history.
func TestBranchActivityPastTheCapOrRefusedIsNotWhole(t *testing.T) {
	r := newRig(t)
	if _, err := r.g.BranchActivity(t.Context(), "agent/endless"); !errors.Is(err, ErrActivityTruncated) {
		t.Fatalf("%v", err)
	}
	if _, err := r.g.BranchActivity(t.Context(), "agent/broken"); err == nil || errors.Is(err, ErrActivityTruncated) {
		t.Fatalf("%v", err)
	}
}

// The issue's author is a login, for the room list's "mine" filter.
func TestIssueAuthorIsItsLogin(t *testing.T) {
	r := newRig(t)
	if iss, err := r.g.Issue(t.Context(), 7); err != nil || iss.Author != "dev1" {
		t.Fatalf("author %q %v", iss.Author, err)
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
	// Conclusion is the raw word: a skipped job reads State SUCCESS but is not a pass (holdoutChecks).
	want := []Check{{Name: "Pre-commit checks", State: "SUCCESS", App: ActionsApp, Conclusion: "SUCCESS"}, {Name: "Kubernetes validation", State: "PENDING"},
		{Name: "Security scanning", State: "FAILURE", Conclusion: "FAILURE"}, {Name: "External links", State: "SUCCESS", App: ActionsApp, Conclusion: "SKIPPED"}}
	if len(c.Runs) != 4 || c.Runs[0] != want[0] || c.Runs[1] != want[1] || c.Runs[2] != want[2] || c.Runs[3] != want[3] {
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
	err := r.g.Merge(ctx, "PR_1", "text409")
	if err == nil || errors.Is(err, ErrHeadMoved) || errors.Is(err, ErrNotMergeable) {
		t.Fatalf("a 409 in the error text is not a conflict: %v", err)
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
	d, err := r.g.Diff(ctx, "old", "new")
	if err != nil {
		t.Fatal(err)
	}
	if d.Base["docs/a.md"] != aBase || d.Head["docs/a.md"] != aHead {
		t.Fatalf("base %q head %q", d.Base["docs/a.md"], d.Head["docs/a.md"])
	}
	if want := (FileStat{Path: "docs/a.md", Status: "modified", Additions: 2, Deletions: 1}); len(d.Files) != 1 || d.Files[0] != want {
		t.Fatalf("files %+v, want [%+v]: GitHub's own counts, which policy-bot's modified_lines sums", d.Files, want)
	}
	if _, err := r.g.Diff(ctx, "old", "mid"); err == nil || !strings.Contains(err.Error(), "big.png") {
		t.Errorf("an unfetchable file must be an error, not a skip: %v", err)
	}
	if _, err := r.g.Diff(ctx, "mid", "new"); err == nil || !strings.Contains(err.Error(), "page") {
		t.Errorf("a truncated compare must be an error: %v", err)
	}
}

// cloud-native-ref#2515: compare's file list is three-dot — changed since the merge base — so the
// base side is read there too. Read at main's tip, another PR's change to the same file lands in
// the diff and the links-only test fails a pure link retarget as class_mismatch.
func TestFilesReadsTheBaseAtTheMergeBase(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	d, err := r.g.Diff(ctx, "main", "behind")
	if err != nil {
		t.Fatal(err)
	}
	if d.Base["docs/b.md"] != bFork || d.Head["docs/b.md"] != bHead {
		t.Fatalf("base %q head %q: the base must be the merge base, not main's tip", d.Base["docs/b.md"], d.Head["docs/b.md"])
	}
	if _, err := r.g.Diff(ctx, "main", "nobase"); err == nil || !strings.Contains(err.Error(), "merge base") {
		t.Errorf("a compare without a merge base must be an error: %v", err)
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
	r.gh.setWantPerms(MergerPermissions(false, false))
	o := r.opts
	o.Permissions = MergerPermissions(false, false)
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
	if !maps.Equal(got[1], MergerPermissions(false, false)) {
		t.Fatalf("the merger connection mints the merger profile: %v", got[1])
	}
}

func TestATokenBeyondTheAppsScopeFailsToMint(t *testing.T) {
	r := newRig(t)
	o := r.opts
	o.Permissions = MergerPermissions(false, false) // checks:read is beyond the factory App
	if _, err := Connect(t.Context(), o); err == nil || !strings.Contains(err.Error(), "mint an installation token") {
		t.Fatalf("a token beyond the App's scope must fail the mint: %v", err)
	}
}

// RerunCheck re-runs the Actions job of the check run id; a refusal (the run is still going) is an error.
func TestRerunCheckPostsTheJobRerun(t *testing.T) {
	r := newRig(t)
	if err := r.g.RerunCheck(t.Context(), 4242); err != nil {
		t.Fatal(err)
	}
	if err := r.g.RerunCheck(t.Context(), 403); err == nil {
		t.Fatal("a refused re-run must be an error")
	}
	r.gh.mu.Lock()
	defer r.gh.mu.Unlock()
	if !slices.Contains(r.gh.calls, "POST /repos/Smana/demo/actions/jobs/4242/rerun") {
		t.Fatalf("calls: %v", r.gh.calls)
	}
}

func TestMergerPermissionsAskForActionsOnlyToRerun(t *testing.T) {
	if _, ok := MergerPermissions(false, false)["actions"]; ok {
		t.Fatal("re-run off must not ask for actions")
	}
	if got := MergerPermissions(true, false)["actions"]; got != "write" {
		t.Fatalf("actions: %q", got)
	}
}

// v2 D1: the breaker's labels are written only once a class is live, so a shadow-only App that
// was never granted issues: write keeps minting.
func TestMergerPermissionsAskForIssuesOnlyToLabel(t *testing.T) {
	if _, ok := MergerPermissions(false, false)["issues"]; ok {
		t.Fatal("no live class must not ask for issues")
	}
	if got := MergerPermissions(false, true)["issues"]; got != "write" {
		t.Fatalf("issues: %q", got)
	}
}

type closeSpy struct {
	io.Reader
	closed *bool
}

func (c closeSpy) Close() error { *c.closed = true; return nil }

type spyRT struct {
	base   http.RoundTripper
	closed bool
}

func (s *spyRT) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := s.base.RoundTrip(r)
	if err == nil {
		resp.Body = closeSpy{Reader: resp.Body, closed: &s.closed}
	}
	return resp, err
}

func TestStatusRTErrorsOn4xxAndFollowsRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/ok", http.StatusTemporaryRedirect)
		case "/ok":
			_, _ = io.WriteString(w, `{"data":{}}`)
		default:
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, "secret body")
		}
	}))
	defer srv.Close()
	spy := &spyRT{base: srv.Client().Transport}
	hc := &http.Client{Transport: statusRT{base: spy}}

	resp, err := hc.Do(mustReq(t, srv.URL+"/start"))
	if err != nil {
		t.Fatalf("a redirect must be followed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("redirect ended at %d", resp.StatusCode)
	}

	spy.closed = false
	_, err = hc.Do(mustReq(t, srv.URL+"/conflict")) //nolint:bodyclose // the error path has no response
	var se *statusError
	if !errors.As(err, &se) || se.code != http.StatusConflict {
		t.Fatalf("a 409 is a statusError: %v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("the body must not reach the error: %v", err)
	}
	if !spy.closed {
		t.Fatal("the body must be closed")
	}
}

func mustReq(t *testing.T, u string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// v2 D4/D5: GitHub's collaborator permission, 404 meaning none; anything else unanswered is an
// error, never an answer, and a login is checked before it reaches the path.
func TestPermissionIsGitHubsAnswer(t *testing.T) {
	r := newRig(t)
	for login, want := range map[string]string{"writer": "write", "stranger": "none"} {
		if got, err := r.g.Permission(t.Context(), login); got != want || err != nil {
			t.Errorf("%s: %q %v", login, got, err)
		}
	}
	for _, login := range []string{"flaky", "empty", "../../x", ""} {
		if got, err := r.g.Permission(t.Context(), login); err == nil {
			t.Errorf("%q: %q answered", login, got)
		}
	}
}

func TestUserLoginReadsTheCurrentLogin(t *testing.T) {
	r := newRig(t)
	if got, err := r.g.UserLogin(t.Context(), 583231); got != "octocat" || err != nil {
		t.Fatalf("%q %v", got, err)
	}
	for _, id := range []int64{7, 0, -1} {
		if got, err := r.g.UserLogin(t.Context(), id); err == nil {
			t.Errorf("%d: %q", id, got)
		}
	}
	if r.g.Repository() != "Smana/demo" {
		t.Fatal(r.g.Repository())
	}
}

// v2 D7: the fence file on the default branch; absent is an answer, a failure is not.
func TestFenceFileReadsTheDefaultBranch(t *testing.T) {
	r := newRig(t)
	if body, found, err := r.g.FenceFile(t.Context()); found || err != nil || len(body) != 0 {
		t.Fatalf("absent: %q %v %v", body, found, err)
	}
	r.gh.mu.Lock()
	r.gh.fence = "factory: aws-0\n"
	r.gh.mu.Unlock()
	if body, found, err := r.g.FenceFile(t.Context()); !found || err != nil || string(body) != "factory: aws-0\n" {
		t.Fatalf("present: %q %v %v", body, found, err)
	}
	r.gh.mu.Lock()
	r.gh.fence = "!"
	r.gh.mu.Unlock()
	if _, _, err := r.g.FenceFile(t.Context()); err == nil {
		t.Fatal("a failed read is an error")
	}
}

// Scoper design SC3: a steering note on the default branch, with its blob sha; absent is an
// answer, a failure is not.
func TestDefaultBranchFileReadsASteeringNote(t *testing.T) {
	r := newRig(t)
	const p = ".github/agent-factory/steering/docs.md"
	if _, _, found, err := r.g.DefaultBranchFile(t.Context(), p); found || err != nil {
		t.Fatalf("absent: %v %v", found, err)
	}
	r.gh.mu.Lock()
	r.gh.steer = map[string]string{p: "## Verify\n- x\n"}
	r.gh.mu.Unlock()
	text, sha, found, err := r.g.DefaultBranchFile(t.Context(), p)
	if !found || err != nil || text != "## Verify\n- x\n" || sha != "5e1f0c2a9b8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f" {
		t.Fatalf("present: %q %q %v %v", text, sha, found, err)
	}
	r.gh.mu.Lock()
	r.gh.steer[p] = "!"
	r.gh.mu.Unlock()
	if _, _, _, err := r.g.DefaultBranchFile(t.Context(), p); err == nil {
		t.Fatal("a failed read is an error")
	}
}

// A path that is never a readable note (a directory, a file over 1 MB, a symlink, a submodule) is
// ErrNotAFile: retrying cannot change it, so the caller proceeds unsteered.
func TestDefaultBranchFileNeverReadable(t *testing.T) {
	const p = ".github/agent-factory/steering/docs.md"
	for name, body := range map[string]string{"an oversized file": "big", "a directory": "dir", "a symlink": "symlink", "a submodule": "submodule", "a symlink resolved to its target": "linked"} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.gh.mu.Lock()
			r.gh.steer = map[string]string{p: body}
			r.gh.mu.Unlock()
			if _, _, found, err := r.g.DefaultBranchFile(t.Context(), p); !errors.Is(err, ErrNotAFile) || found {
				t.Fatalf("found %v err %v, want ErrNotAFile", found, err)
			}
		})
	}
}

// A 403, a 5xx or a network failure is retryable: an error that is not ErrNotAFile, never "no note" (SC4).
func TestDefaultBranchFileFailsClosed(t *testing.T) {
	const p = ".github/agent-factory/steering/docs.md"
	for name, body := range map[string]string{"a 403": "denied", "a 500": "!"} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.gh.mu.Lock()
			r.gh.steer = map[string]string{p: body}
			r.gh.mu.Unlock()
			if _, _, found, err := r.g.DefaultBranchFile(t.Context(), p); err == nil || found || errors.Is(err, ErrNotAFile) {
				t.Fatalf("found %v err %v, want a retryable error", found, err)
			}
		})
	}
}

// Splitter design SL8, SL12: a part's issue is created with its labels and read back with the
// numeric id the sub-issues API takes; the link posts that id; a parent closes as completed; the
// parts are listed open or closed, pull requests left out.
func TestIssuesAreOpenedLinkedAndClosed(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	iss, err := r.g.CreateIssue(ctx, "Part 1 of 2 of #7", "body", []string{"factory/child-of:#7", "class:docs"})
	if err != nil || iss.Number != 31 || iss.ID != 3101 || iss.State != "OPEN" || iss.Author != "ogenki-agent-factory[bot]" ||
		!slices.Equal(iss.Labels, []string{"factory/child-of:#7", "class:docs"}) {
		t.Fatalf("%+v %v", iss, err)
	}
	if err := r.g.AddSubIssue(ctx, 7, iss.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.g.CloseIssue(ctx, 7); err != nil {
		t.Fatal(err)
	}
	all, err := r.g.IssuesLabelled(ctx, "factory/child-of:#7")
	if err != nil || len(all) != 1 || all[0].Number != 31 || all[0].ID != 3101 || !strings.Contains(all[0].Body, "split=3buqdlot/1") {
		t.Fatalf("%+v %v", all, err)
	}
}

// A list that does not fit one page is an error, never a short list read as a missing part (a
// duplicate child); a refused create, link or close returns the forge's error.
func TestIssueCallsFailLoudly(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	if _, err := r.g.IssuesLabelled(ctx, "factory/child-of:#9"); err == nil {
		t.Error("a second page was ignored")
	}
	if _, err := r.g.CreateIssue(ctx, "refused", "b", nil); err == nil {
		t.Error("a 422 on create was swallowed")
	}
	if err := r.g.AddSubIssue(ctx, 8, 1); err == nil {
		t.Error("a 422 on the link was swallowed")
	}
	if err := r.g.CloseIssue(ctx, 8); err == nil {
		t.Error("a 404 on close was swallowed")
	}
}

func TestMergedSinceSearchesMergedPullRequestsSince(t *testing.T) {
	r := newRig(t)
	since := time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	ps, err := r.g.MergedSince(t.Context(), since)
	if err != nil {
		t.Fatal(err)
	}
	b := r.gh.graphqlBodies()
	var in struct{ Variables map[string]any }
	if err := json.Unmarshal([]byte(b[len(b)-1]), &in); err != nil || in.Variables["q"] != "repo:Smana/demo is:pr is:merged merged:>=2026-10-16T10:00:00Z" {
		t.Fatalf("the search sent: %v %s", err, b[len(b)-1])
	}
	want := []MergedPull{
		{Number: 51, Author: "renovate[bot]", HeadRef: "renovate/x", MergeCommitSHA: "m51", Title: "chore(deps): bump x",
			MergedAt: time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC), Labels: []string{"factory/merged:deps-patch", "dependencies"}},
		{Number: 52, Author: "Smana", HeadRef: "fix/y", Title: "fix: y", MergedAt: time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC)},
	}
	if len(ps) != 2 || !reflect.DeepEqual(ps, want) {
		t.Fatalf("%+v", ps)
	}
}

// A hundred results may be the first hundred of more: the caller narrows since, it is never
// handed a window that looks whole.
func TestMergedSinceAtTheWindowIsTruncated(t *testing.T) {
	r := newRig(t)
	_, err := r.g.MergedSince(t.Context(), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	if !errors.Is(err, ErrMergedTruncated) {
		t.Fatalf("%v", err)
	}
}

func TestPullFilesMapsThePatchAndTheRename(t *testing.T) {
	r := newRig(t)
	fs, err := r.g.PullFiles(t.Context(), 20)
	if err != nil {
		t.Fatal(err)
	}
	want := []FilePatch{{Path: "a.yaml", Status: "modified", Patch: "@@ -1 +1 @@\n-a\n+b"},
		{Path: "new.yaml", Previous: "old.yaml", Status: "renamed"},
		{Path: "big.yaml", Status: "modified", PatchMissing: true}, {Path: "img.png", Status: "modified", PatchMissing: true},
		{Path: "empty.yaml", Status: "added"}}
	if !reflect.DeepEqual(fs, want) {
		t.Fatalf("%+v", fs)
	}
}

// GitHub lists 3,000 files at most: a list that reaches it may be missing some, so it is an error.
func TestPullFilesAtGitHubsCapIsAnError(t *testing.T) {
	r := newRig(t)
	if fs, err := r.g.PullFiles(t.Context(), 21); err == nil || fs != nil {
		t.Fatalf("%d files, %v", len(fs), err)
	}
}

func TestBlameAtParentMapsTheRanges(t *testing.T) {
	r := newRig(t)
	rs, err := r.g.BlameAtParent(t.Context(), "m51", "a.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rs, []BlameRange{{Start: 1, End: 9, Commit: "aaa"}, {Start: 10, End: 12, Commit: "bbb"}}) {
		t.Fatalf("%+v", rs)
	}
	b := r.gh.graphqlBodies()
	if last := b[len(b)-1]; !strings.Contains(last, "parents(first: 1)") || !strings.Contains(last, "blame(path:") {
		t.Fatalf("the query: %s", last)
	}
}

// A file the parent does not have, and a null blame, are ErrNoBlame: "nothing blamed" is never
// told apart from "no overlap" by an empty answer. Any other failure is a plain error.
func TestBlameAtParentOfAMissingFileIsErrNoBlameAndOtherFailuresAreErrors(t *testing.T) {
	r := newRig(t)
	for _, p := range []string{"gone.yaml", "missing.yaml"} {
		if rs, err := r.g.BlameAtParent(t.Context(), "m51", p); !errors.Is(err, ErrNoBlame) || rs != nil {
			t.Fatalf("%s: %+v %v", p, rs, err)
		}
	}
	if rs, err := r.g.BlameAtParent(t.Context(), "m51", "boom.yaml"); err == nil || errors.Is(err, ErrNoBlame) || rs != nil {
		t.Fatalf("%+v %v", rs, err)
	}
}

// More labels than the 20 read: a label may be unseen, so the read is truncated.
func TestMergedSinceWithUnreadLabelsIsTruncated(t *testing.T) {
	r := newRig(t)
	if ps, err := r.g.MergedSince(t.Context(), time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)); !errors.Is(err, ErrMergedTruncated) || ps != nil {
		t.Fatalf("%+v %v", ps, err)
	}
}
