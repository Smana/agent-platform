// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/fanout"
	"github.com/Smana/agent-platform/internal/store"
)

// memLinks is the store's link tables, kept to the store's rules (store/slacklinks_test.go proves
// those against PostgreSQL).
type memLinks struct {
	mu    sync.Mutex
	codes map[string]*memCode
	links map[string]store.SlackLink // team/user
	subs  map[string]string          // team/user -> sub
}

type memCode struct {
	store.SlackCode
	used bool
}

func newMemLinks() *memLinks {
	return &memLinks{codes: map[string]*memCode{}, links: map[string]store.SlackLink{}, subs: map[string]string{}}
}

func (m *memLinks) MintSlackCode(_ context.Context, h []byte, c store.SlackCode, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, old := range m.codes {
		if old.Team == c.Team && old.User == c.User {
			delete(m.codes, k)
		}
	}
	m.codes[hex.EncodeToString(h)] = &memCode{SlackCode: c}
	return nil
}

func (m *memLinks) SlackCode(_ context.Context, h []byte, now time.Time) (store.SlackCode, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.codes[hex.EncodeToString(h)]
	if !ok || c.used || !c.Expires.After(now) {
		return store.SlackCode{}, store.ErrCodeInvalid
	}
	return c.SlackCode, nil
}

func (m *memLinks) LinkSlack(_ context.Context, h []byte, team, sub string, now time.Time) (store.SlackCode, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.codes[hex.EncodeToString(h)]
	if !ok || c.used || !c.Expires.After(now) || c.Team != team {
		return store.SlackCode{}, store.ErrCodeInvalid
	}
	key := c.Team + "/" + c.User
	if held, ok := m.subs[key]; ok && held != sub {
		return store.SlackCode{}, store.ErrAlreadyLinked
	}
	c.used = true
	m.links[key], m.subs[key] = store.SlackLink{Team: c.Team, User: c.User, Handle: c.Handle, Name: c.Name, LinkedAt: now}, sub
	return c.SlackCode, nil
}

func (m *memLinks) SlackLinksOf(_ context.Context, sub string) ([]store.SlackLink, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.SlackLink
	for k, s := range m.subs {
		if s == sub {
			out = append(out, m.links[k])
		}
	}
	return out, nil
}

func (m *memLinks) UnlinkSlack(_ context.Context, team, user, sub string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := team + "/" + user
	if s, ok := m.subs[key]; !ok || (sub != "" && s != sub) {
		return false, nil
	}
	delete(m.subs, key)
	delete(m.links, key)
	return true, nil
}

func (m *memLinks) linkedTo(team, user string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.subs[team+"/"+user]
}

const linkOrigin = "https://rooms.example.test"

var otherBot = authn.Principal{Kind: envelope.ActorSystem, ID: "system:other-bot", Sponsored: true, AccessToken: "t",
	Expiry: time.Now().Add(24 * time.Hour)}

// slackLinkOn turns the link on with links as the store, the Slack App as its principal.
func slackLinkOn(links *memLinks) option {
	return func(s *Server, _ *fanout.Hub, _ *hubView) {
		s.PublicURL = linkOrigin
		s.SlackLink = &SlackLinker{Store: links, Principal: "system:slack-app", TeamID: "T01ABC", Repository: "Smana/ai-native-ref"}
	}
}

// mint asks slack_link_code as the Slack App and returns the code from its url; the user's handle
// is "h-<name>".
func mint(t *testing.T, e env, user, name string) string {
	t.Helper()
	got := call(t, e, bearer("slack-app"), "slack_link_code", map[string]any{"team": "T01ABC", "user": user, "handle": "h-" + name, "name": name})
	var out struct{ URL, ExpiresAt string }
	if got.isError || json.Unmarshal(got.structured, &out) != nil {
		t.Fatalf("mint: %+v", got)
	}
	code, ok := strings.CutPrefix(out.URL, linkOrigin+"/link/slack?code=")
	if !ok || !linkCodeRE.MatchString(code) {
		t.Fatalf("the url is not the page with a code: %q", out.URL)
	}
	return code
}

func linkReq(t *testing.T, e env, method, path string, h http.Header, form url.Values) (int, string) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(t.Context(), method, e.ts.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = h.Clone()
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

// Only the Slack App's principal sees and may call the two link tools; another sponsored
// principal and a human do not.
func TestSlackLinkToolsAreTheAppsOnly(t *testing.T) {
	links := newMemLinks()
	e := setup(t, mcpOn, withSponsored(sponsoredP, otherBot), slackLinkOn(links))
	if got := toolNames(t, e, bearer("slack-app")); !strings.HasSuffix(got, ",slack_link_code,slack_unlink") {
		t.Fatalf("the App's tools: %s", got)
	}
	if got := toolNames(t, e, bearer("other-bot")); strings.Contains(got, "slack_") {
		t.Fatalf("another sponsored principal sees the link tools: %s", got)
	}
	if got := toolNames(t, e, viaRoomctl("dev")); strings.Contains(got, "slack_") {
		t.Fatalf("a human sees the link tools: %s", got)
	}
	_, out := rpc(t, e, bearer("other-bot"), "tools/call", map[string]any{"name": "slack_link_code",
		"arguments": map[string]any{"team": "T01ABC", "user": "U02DEF"}})
	if rpcCode(out) == 0 {
		t.Fatalf("another principal minted a code: %v", out)
	}
	if got := call(t, e, bearer("slack-app"), "slack_link_code", map[string]any{"team": "T09OTHER", "user": "U02DEF"}); !got.isError ||
		!strings.HasPrefix(got.text, "wrong_team") {
		t.Fatalf("another workspace: %+v", got)
	}
}

// The whole flow: a code shows the Slack user and the account's GitHub login, a POST from this
// origin links them once, a replay is refused, and the account unlinks its own link only.
func TestSlackLinkConfirmAndUnlink(t *testing.T) {
	links := newMemLinks()
	e := setup(t, mcpOn, withSponsored(sponsoredP), slackLinkOn(links))
	code := mint(t, e, "U02DEF", "dana\u202e<b>")
	status, page := linkReq(t, e, http.MethodGet, "/link/slack?code="+code, header("dana"), nil)
	if status != http.StatusOK || !strings.Contains(page, "Link Slack user <strong>@h-dana&lt;b&gt; (U02DEF)</strong> in workspace T01ABC") ||
		!strings.Contains(page, "Display name, set by that user: dana&lt;b&gt;") ||
		!strings.Contains(page, "(GitHub: <strong>@dana</strong>)") || !strings.Contains(page, "Never confirm a link someone sent you") {
		t.Fatalf("the confirmation (name inert, no bidi control): %d\n%s", status, page)
	}
	if links.linkedTo("T01ABC", "U02DEF") != "" {
		t.Fatal("a GET linked")
	}
	form := url.Values{"code": {code}}
	for name, h := range map[string]http.Header{
		"no Origin":      header("dana"),
		"another Origin": header("dana", "Origin", "https://evil.example"),
		// P18: roomctl's bearer (a local agent's) can forge any Origin, so it never links.
		"a roomctl token": header("dana", "Origin", linkOrigin, "X-Test-Client", "roomctl"),
	} {
		if status, _ := linkReq(t, e, http.MethodPost, "/link/slack", h, form); status != http.StatusForbidden {
			t.Fatalf("%s: %d", name, status)
		}
	}
	if links.linkedTo("T01ABC", "U02DEF") != "" {
		t.Fatal("a refused POST linked")
	}
	h := header("dana", "Origin", linkOrigin+":443") // a default port is the same origin
	status, page = linkReq(t, e, http.MethodPost, "/link/slack", h, form)
	if status != http.StatusOK || !strings.Contains(page, "Linked: Slack user @h-dana&lt;b&gt; (U02DEF) now files tasks sponsored by @dana") ||
		links.linkedTo("T01ABC", "U02DEF") != "dana" {
		t.Fatalf("confirm: %d %q\n%s", status, links.linkedTo("T01ABC", "U02DEF"), page)
	}
	if _, page := linkReq(t, e, http.MethodPost, "/link/slack", h, form); !strings.Contains(page, "expired or was already used") {
		t.Fatalf("a replay: %s", page)
	}
	// A link is never moved to another account: unlink first.
	moved := mint(t, e, "U02DEF", "dana")
	if _, page := linkReq(t, e, http.MethodPost, "/link/slack", header("eve", "Origin", linkOrigin), url.Values{"code": {moved}}); !strings.Contains(page, "linked to another account") ||
		links.linkedTo("T01ABC", "U02DEF") != "dana" {
		t.Fatalf("a link moved: %s", page)
	}
	// Someone else cannot unlink it, nor a roomctl token; its own web session can.
	unlink := url.Values{"team": {"T01ABC"}, "user": {"U02DEF"}}
	if status, _ := linkReq(t, e, http.MethodPost, "/link/slack/unlink", header("dana", "Origin", linkOrigin, "X-Test-Client", "roomctl"), unlink); status != http.StatusForbidden {
		t.Fatalf("a roomctl unlink: %d", status)
	}
	if _, page := linkReq(t, e, http.MethodPost, "/link/slack/unlink", header("eve", "Origin", linkOrigin), unlink); !strings.Contains(page, "Nothing to unlink") ||
		links.linkedTo("T01ABC", "U02DEF") != "dana" {
		t.Fatalf("another account unlinked: %s", page)
	}
	if _, page := linkReq(t, e, http.MethodPost, "/link/slack/unlink", h, unlink); !strings.Contains(page, "Unlinked") ||
		links.linkedTo("T01ABC", "U02DEF") != "" {
		t.Fatalf("unlink: %s", page)
	}
}

// The page's refusals: an unknown or expired code, a code of another workspace, an account with
// no GitHub link, and the App's own unlink.
func TestSlackLinkRefusals(t *testing.T) {
	links := newMemLinks()
	w := world(nil, "nogh")
	e := setup(t, mcpOn, withSponsored(sponsoredP), slackLinkOn(links), w.option())
	if _, page := linkReq(t, e, http.MethodGet, "/link/slack?code=aaaaaaaaaaaaaaaaaaaaaaaaaa", header("dana"), nil); !strings.Contains(page, "expired or was already used") {
		t.Fatalf("an unknown code: %s", page)
	}
	if _, page := linkReq(t, e, http.MethodGet, "/link/slack?code=../x", header("dana"), nil); !strings.Contains(page, "expired or was already used") {
		t.Fatalf("a malformed code: %s", page)
	}
	code := mint(t, e, "U02DEF", "")
	if _, page := linkReq(t, e, http.MethodGet, "/link/slack?code="+code, header("nogh"), nil); !strings.Contains(page, "no GitHub link") ||
		strings.Contains(page, `name="code"`) {
		t.Fatalf("an account without GitHub: %s", page)
	}
	if _, page := linkReq(t, e, http.MethodPost, "/link/slack", header("nogh", "Origin", linkOrigin), url.Values{"code": {code}}); !strings.Contains(page, "no GitHub link") ||
		links.linkedTo("T01ABC", "U02DEF") != "" {
		t.Fatalf("an account without GitHub linked: %s", page)
	}
	e.srv.Now = func() time.Time { return time.Now().Add(SlackLinkTTL + time.Second) }
	if _, page := linkReq(t, e, http.MethodGet, "/link/slack?code="+code, header("dana"), nil); !strings.Contains(page, "expired") {
		t.Fatalf("an expired code: %s", page)
	}
	e.srv.Now = nil
	// A code the store holds for another workspace (minted before a teamID change).
	other := "bbbbbbbbbbbbbbbbbbbbbbbbbb"
	_ = links.MintSlackCode(t.Context(), codeSHA256(other), store.SlackCode{Team: "T09OTHER", User: "U02DEF", Expires: time.Now().Add(time.Minute)}, time.Now())
	if _, page := linkReq(t, e, http.MethodGet, "/link/slack?code="+other, header("dana"), nil); !strings.Contains(page, "another Slack workspace") {
		t.Fatalf("another workspace's code: %s", page)
	}
	// The App unlinks at the Slack user's request, whoever's account it was.
	code = mint(t, e, "U02DEF", "")
	if status, _ := linkReq(t, e, http.MethodPost, "/link/slack", header("dana", "Origin", linkOrigin), url.Values{"code": {code}}); status != http.StatusOK {
		t.Fatal(status)
	}
	got := call(t, e, bearer("slack-app"), "slack_unlink", map[string]any{"team": "T01ABC", "user": "U02DEF"})
	if got.isError || !bytes.Contains(got.structured, []byte(`"unlinked":true`)) || links.linkedTo("T01ABC", "U02DEF") != "" {
		t.Fatalf("the App's unlink: %+v", got)
	}
}

// Off, the page is the SPA's 404 and the tools are not listed.
func TestSlackLinkOff(t *testing.T) {
	e := setup(t, mcpOn, withSponsored(sponsoredP))
	if status, _ := linkReq(t, e, http.MethodGet, "/link/slack", header("dana"), nil); status != http.StatusNotFound {
		t.Fatalf("off: %d", status)
	}
	if got := toolNames(t, e, bearer("slack-app")); regexp.MustCompile(`slack_`).MatchString(got) {
		t.Fatalf("off, tools: %s", got)
	}
}
