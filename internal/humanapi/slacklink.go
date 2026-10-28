// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Smana/maku/internal/authn"
	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/mcpwire"
	"github.com/Smana/maku/internal/store"
)

// The Slack self-serve link (docs/superpowers/specs/2026-10-28-slack-self-serve-link-design.md):
// the Slack App mints a one-time code for the Slack user who asked, and a signed-in human confirms
// it on /link/slack, which links that Slack user to their ZITADEL account. The factory resolves
// the account's GitHub login at each use; nothing here grants a thing by itself.

// SlackLinkTTL is how long a code may be confirmed.
const SlackLinkTTL = 10 * time.Minute

// SlackLinkStore is the part of the store the link reads and writes; *store.Store has it.
type SlackLinkStore interface {
	MintSlackCode(ctx context.Context, codeSHA256 []byte, c store.SlackCode, now time.Time) error
	SlackCode(ctx context.Context, codeSHA256 []byte, now time.Time) (store.SlackCode, error)
	LinkSlack(ctx context.Context, codeSHA256 []byte, team, sub string, now time.Time) (store.SlackCode, error)
	SlackLinksOf(ctx context.Context, sub string) ([]store.SlackLink, error)
	UnlinkSlack(ctx context.Context, team, user, sub string) (bool, error)
}

// SlackLinker is human.slackLink: who may mint codes (the Slack App's sponsored principal,
// system:<name>), for which workspace, and the repository whose installation names a login.
type SlackLinker struct {
	Store      SlackLinkStore
	Principal  string
	TeamID     string
	Repository string
	Rand       io.Reader // nil: crypto/rand
}

// The link's outcomes, rooms_slack_links_total{outcome}: a closed list.
const (
	linkMinted       = "link_code"
	linkLinked       = "linked"
	linkUnlinked     = "unlinked"
	linkCodeInvalid  = "code_invalid"
	linkNoGitHub     = "not_linked_github"
	linkWrongTeam    = "wrong_team"
	linkTooMany      = "too_many_links"
	linkAlready      = "already_linked"
	linkNotWeb       = "not_web"
	linkTooManyCodes = "too_many_codes"
	linkBadOrigin    = "bad_origin"
	linkError        = "error"
)

var (
	slackTeamRE = regexp.MustCompile(`^T[A-Z0-9]{2,20}$`)
	slackUserRE = regexp.MustCompile(`^[UW][A-Z0-9]{2,20}$`)
	// linkCodeRE is a code as minted: 16 random bytes in lowercase base32, unpadded.
	linkCodeRE = regexp.MustCompile(`^[a-z2-7]{26}$`)
	linkCode   = base32.StdEncoding.WithPadding(base32.NoPadding)
)

const maxSlackName = 80

func codeSHA256(code string) []byte {
	sum := sha256.Sum256([]byte(code))
	return sum[:]
}

// slackName is a Slack display name as the page may show it: printable, one line, at most 80
// runes. It is display only: html/template escapes it, and nothing is decided on it.
func slackName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > maxSlackName {
		s = string(r[:maxSlackName])
	}
	return s
}

func (s *Server) linkCounted(ctx context.Context, outcome string) {
	if s.Metrics != nil && s.Metrics.SlackLinks != nil {
		s.Metrics.SlackLinks.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	}
}

// mintsLinks is whether p is the Slack App's principal, the one that may mint and unlink.
func (s *Server) mintsLinks(p authn.Principal) bool {
	return s.SlackLink != nil && p.Sponsored && p.ID == s.SlackLink.Principal
}

// slackLinkTools are the Slack App's two link tools, listed to it alone.
func (s *Server) slackLinkTools() []mcpTool {
	return []mcpTool{
		{name: "slack_link_code", call: s.slackLinkCode,
			description: "Mint a one-time link for a Slack user to link their Slack user to their rooms account. Post the url to that user only.",
			schema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["team","user"],"properties":{` +
				`"team":{"type":"string","pattern":"^T[A-Z0-9]{2,20}$"},"user":{"type":"string","pattern":"^[UW][A-Z0-9]{2,20}$"},` +
				`"handle":{"type":"string","maxLength":256},"name":{"type":"string","maxLength":256}}}`)},
		{name: "slack_unlink", call: s.slackUnlink,
			description: "Remove a Slack user's link, at that Slack user's own request.",
			schema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["team","user"],"properties":{` +
				`"team":{"type":"string","pattern":"^T[A-Z0-9]{2,20}$"},"user":{"type":"string","pattern":"^[UW][A-Z0-9]{2,20}$"}}}`)},
	}
}

type slackUserArgs struct {
	Team   string `json:"team"`
	User   string `json:"user"`
	Handle string `json:"handle"`
	Name   string `json:"name"`
}

func (s *Server) slackUserArgs(args json.RawMessage) (slackUserArgs, error) {
	var a slackUserArgs
	if mcpwire.DecodeArgs(args, &a) != nil || !slackTeamRE.MatchString(a.Team) || !slackUserRE.MatchString(a.User) {
		return a, toolError("invalid_arguments: team is T…, user is U… or W…")
	}
	if a.Team != s.SlackLink.TeamID {
		return a, toolError(linkWrongTeam + ": this workspace is not the one the factory serves")
	}
	return a, nil
}

// slackLinkCode is slack_link_code: a fresh code for the Slack user, replacing their earlier one.
func (s *Server) slackLinkCode(ctx context.Context, p authn.Principal, args json.RawMessage) (any, error) {
	if !s.mintsLinks(p) {
		return nil, toolError("unknown tool: slack_link_code")
	}
	a, err := s.slackUserArgs(args)
	if err != nil {
		s.linkCounted(ctx, linkWrongTeam)
		return nil, err
	}
	raw := make([]byte, 16)
	rd := s.SlackLink.Rand
	if rd == nil {
		rd = rand.Reader
	}
	if _, err := io.ReadFull(rd, raw); err != nil {
		return nil, err
	}
	code := strings.ToLower(linkCode.EncodeToString(raw))
	now := s.now()
	exp := now.Add(SlackLinkTTL)
	err = s.SlackLink.Store.MintSlackCode(ctx, codeSHA256(code), store.SlackCode{Team: a.Team, User: a.User, Handle: slackName(a.Handle), Name: slackName(a.Name), Expires: exp}, now)
	if errors.Is(err, store.ErrTooManyCodes) {
		s.linkCounted(ctx, linkTooManyCodes)
		return nil, toolError(linkTooManyCodes + ": too many links are waiting; try again in a few minutes")
	}
	if err != nil {
		s.linkCounted(ctx, linkError)
		return nil, err
	}
	s.linkCounted(ctx, linkMinted)
	return map[string]any{"url": strings.TrimSuffix(s.PublicURL, "/") + "/link/slack?code=" + code,
		"expiresAt": exp.UTC().Format(time.RFC3339)}, nil
}

// slackUnlink is slack_unlink: the Slack user's link goes, whoever's account it was to.
func (s *Server) slackUnlink(ctx context.Context, p authn.Principal, args json.RawMessage) (any, error) {
	if !s.mintsLinks(p) {
		return nil, toolError("unknown tool: slack_unlink")
	}
	a, err := s.slackUserArgs(args)
	if err != nil {
		return nil, err
	}
	gone, err := s.SlackLink.Store.UnlinkSlack(ctx, a.Team, a.User, "")
	if err != nil {
		s.linkCounted(ctx, linkError)
		return nil, err
	}
	if gone {
		s.linkCounted(ctx, linkUnlinked)
	}
	return map[string]any{"unlinked": gone}, nil
}

// linkPage is what /link/slack renders.
type linkPage struct {
	Notice, Refusal string
	Confirm         *linkConfirm
	Links           []store.SlackLink
	Login           string
}

type linkConfirm struct {
	Code, Team, User, Handle, Name string
}

// The link page: no script, the room UI's CSP, and forms that post to this origin only.
var linkTemplate = template.Must(template.New("link").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Link Slack</title><link rel="stylesheet" href="/link/slack.css"></head>
<body><main>
<h1>Link Slack to your account</h1>
{{if .Notice}}<p class="notice" role="status">{{.Notice}}</p>{{end}}
{{if .Refusal}}<p class="refusal" role="alert">{{.Refusal}}</p>{{end}}
{{with .Confirm}}
<section class="confirm">
<p>Link Slack user <strong>{{if .Handle}}@{{.Handle}} {{end}}({{.User}})</strong> in workspace {{.Team}} to your account (GitHub: <strong>@{{$.Login}}</strong>)?</p>
{{if .Name}}<p>Display name, set by that user: {{.Name}}</p>{{end}}
<p class="warn">Never confirm a link someone sent you. Confirm only if you posted <code>@factory link</code> yourself and this Slack user is you:
once linked, its check marks in Slack file factory tasks sponsored by @{{$.Login}}. Linking grants nothing else: you must still maintain the repository.</p>
<form method="post" action="/link/slack"><input type="hidden" name="code" value="{{.Code}}"><button type="submit">Link</button></form>
</section>
{{end}}
<h2>Your Slack links</h2>
{{if .Links}}<ul>{{range .Links}}
<li>{{if .Handle}}@{{.Handle}} {{end}}({{.User}}){{if .Name}}, display name {{.Name}}{{end}}, workspace {{.Team}}, since {{.LinkedAt.Format "2006-01-02"}}
<form method="post" action="/link/slack/unlink"><input type="hidden" name="team" value="{{.Team}}"><input type="hidden" name="user" value="{{.User}}"><button type="submit">Unlink</button></form></li>
{{end}}</ul>{{else}}<p>None. Mention <code>@factory link</code> in Slack to make one.</p>{{end}}
</main></body></html>
`))

const linkCSS = `body{font:16px/1.5 system-ui,sans-serif;margin:0;padding:16px;background:#fff;color:#1b1b1b}
main{max-width:40rem;margin:0 auto}.notice{color:#0b5d1e}.refusal{color:#a4000f}.warn{color:#6b4e00}
form{display:inline;margin-left:.5rem}button{font:inherit;padding:.3rem .9rem;cursor:pointer}
@media (prefers-color-scheme:dark){body{background:#101418;color:#e6e6e6}.notice{color:#7ad48f}.refusal{color:#ff8a8a}.warn{color:#e0c060}}
`

// slackLinkRoutes adds /link/slack's routes.
func (s *Server) slackLinkRoutes(mux *http.ServeMux) {
	mux.Handle("GET /link/slack", withCSP(sameOriginReferrer(http.HandlerFunc(s.linkGet))))
	mux.Handle("POST /link/slack", withCSP(sameOriginReferrer(http.HandlerFunc(s.linkConfirm))))
	mux.Handle("POST /link/slack/unlink", withCSP(sameOriginReferrer(http.HandlerFunc(s.linkUnlink))))
	mux.Handle("GET /link/slack.css", withCSP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = io.WriteString(w, linkCSS)
	})))
}

// linkHuman is the signed-in human behind a link page, or the refusal written. Only a web UI
// session links or unlinks (ruling P18): a maku or MCP bearer can set any Origin, so a local
// agent on the developer's token must never confirm a link in their name.
func (s *Server) linkHuman(w http.ResponseWriter, r *http.Request) (authn.Principal, bool) {
	p, ok := s.principal(w, r)
	if !ok {
		return p, false
	}
	if p.Kind != envelope.ActorHuman || p.Sub == "" || s.WebClient == nil || p.ClientID == "" || p.ClientID != s.WebClient() {
		s.linkCounted(r.Context(), linkNotWeb)
		http.Error(w, "the rooms web UI's session is required", http.StatusForbidden)
		return p, false
	}
	return p, true
}

// sameOriginReferrer overrides the server-wide `Referrer-Policy: no-referrer` on the link routes.
// By the Fetch spec a browser serialises a request's Origin as "null" when its referrer policy is
// no-referrer, so the page's form POST would carry `Origin: null` and sameOrigin would refuse it.
// With same-origin the browser sends the real Origin, and the code (in the page's URL) is never
// sent as a Referer to another site. sameOrigin stays strict: "null" is still refused.
func sameOriginReferrer(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "same-origin")
		h.ServeHTTP(w, r)
	})
}

// origin is u's scheme://host[:port], the default port dropped, as a browser sends Origin.
func origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return strings.ToLower(u.Scheme) + "://" + host
}

// sameOrigin is a state-changing POST's CSRF check, on top of oauth2-proxy's SameSite=Strict
// cookie: the Origin must be present and the broker's own (a browser sends it on every POST).
func (s *Server) sameOrigin(w http.ResponseWriter, r *http.Request) bool {
	if want, o := origin(s.PublicURL), r.Header.Get("Origin"); want != "" && o != "" && origin(o) == want {
		return true
	}
	s.linkCounted(r.Context(), linkBadOrigin)
	http.Error(w, "forbidden: cross-origin", http.StatusForbidden)
	return false
}

func (s *Server) renderLink(w http.ResponseWriter, r *http.Request, p authn.Principal, page linkPage) {
	links, err := s.SlackLink.Store.SlackLinksOf(r.Context(), p.Sub)
	if err != nil {
		s.log().Warn("slack links unreadable", "err", err)
		http.Error(w, "unavailable: try again", http.StatusServiceUnavailable)
		return
	}
	page.Links = links
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // a code rides the URL and the form
	if err := linkTemplate.Execute(w, page); err != nil {
		s.log().Warn("link page", "err", err)
	}
}

// githubLogin is the account's current GitHub login, "" when it has none; ok is false when the
// refusal was written.
func (s *Server) githubLogin(w http.ResponseWriter, r *http.Request, p authn.Principal) (string, bool) {
	if s.Identity == nil {
		return "", true
	}
	ctx, cancel := context.WithTimeout(r.Context(), accessWait)
	defer cancel()
	login, err := s.Identity.Login(ctx, p.Sub, s.SlackLink.Repository)
	if err != nil {
		s.log().Warn("github link unverified", "err", err)
		s.linkCounted(r.Context(), linkError)
		http.Error(w, reasonAccessUnverified, http.StatusServiceUnavailable)
		return "", false
	}
	return login, true
}

// linkCode checks a code for p: live, of the App's workspace, and p's account linked to GitHub.
// It answers the page's refusal, or "" with the code and login.
func (s *Server) linkCode(w http.ResponseWriter, r *http.Request, p authn.Principal, code string) (store.SlackCode, string, string, bool) {
	ctx := r.Context()
	if !linkCodeRE.MatchString(code) {
		s.linkCounted(ctx, linkCodeInvalid)
		return store.SlackCode{}, "", refusalCodeInvalid, true
	}
	c, err := s.SlackLink.Store.SlackCode(ctx, codeSHA256(code), s.now())
	switch {
	case errors.Is(err, store.ErrCodeInvalid):
		s.linkCounted(ctx, linkCodeInvalid)
		return c, "", refusalCodeInvalid, true
	case err != nil:
		s.log().Warn("slack link code unreadable", "err", err)
		s.linkCounted(ctx, linkError)
		http.Error(w, "unavailable: try again", http.StatusServiceUnavailable)
		return c, "", "", false
	case c.Team != s.SlackLink.TeamID:
		s.linkCounted(ctx, linkWrongTeam)
		return c, "", "This link is for another Slack workspace than the one the factory serves.", true
	}
	login, ok := s.githubLogin(w, r, p)
	if !ok {
		return c, "", "", false
	}
	if login == "" {
		s.linkCounted(ctx, linkNoGitHub)
		return c, "", "Your account has no GitHub link: link GitHub in your ZITADEL profile, then open this link again.", true
	}
	return c, login, "", true
}

const refusalCodeInvalid = "This link expired or was already used: mention @factory link in Slack for a new one."

// linkGet is GET /link/slack: with a code, the confirmation; always, the account's links.
func (s *Server) linkGet(w http.ResponseWriter, r *http.Request) {
	p, ok := s.linkHuman(w, r)
	if !ok {
		return
	}
	page := linkPage{}
	if code := r.URL.Query().Get("code"); code != "" {
		c, login, refusal, ok := s.linkCode(w, r, p, code)
		if !ok {
			return
		}
		page.Refusal, page.Login = refusal, login
		if refusal == "" {
			page.Confirm = &linkConfirm{Code: code, Team: c.Team, User: c.User, Handle: c.Handle, Name: c.Name}
		}
	}
	s.renderLink(w, r, p, page)
}

// postForm parses a small urlencoded form.
func postForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") || r.ParseForm() != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	return true
}

// linkConfirm is POST /link/slack: the code is used once and its Slack user linked to p.
func (s *Server) linkConfirm(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}
	p, ok := s.linkHuman(w, r)
	if !ok || !postForm(w, r) {
		return
	}
	ctx := r.Context()
	code := r.PostForm.Get("code")
	_, login, refusal, ok := s.linkCode(w, r, p, code)
	if !ok {
		return
	}
	if refusal != "" {
		s.renderLink(w, r, p, linkPage{Refusal: refusal})
		return
	}
	c, err := s.SlackLink.Store.LinkSlack(ctx, codeSHA256(code), s.SlackLink.TeamID, p.Sub, s.now())
	switch {
	case errors.Is(err, store.ErrCodeInvalid): // used by a racing confirm
		s.linkCounted(ctx, linkCodeInvalid)
		s.renderLink(w, r, p, linkPage{Refusal: refusalCodeInvalid})
		return
	case errors.Is(err, store.ErrAlreadyLinked):
		s.linkCounted(ctx, linkAlready)
		s.renderLink(w, r, p, linkPage{Refusal: "This Slack user is linked to another account: it must mention @factory unlink first, then ask for a new link."})
		return
	case errors.Is(err, store.ErrTooManyLinks):
		s.linkCounted(ctx, linkTooMany)
		s.renderLink(w, r, p, linkPage{Refusal: "Your account holds the most Slack users it may: unlink one below, then open the link again."})
		return
	case err != nil:
		s.log().Warn("slack link failed", "err", err)
		s.linkCounted(ctx, linkError)
		http.Error(w, "unavailable: try again", http.StatusServiceUnavailable)
		return
	}
	s.linkCounted(ctx, linkLinked)
	who := c.User
	if c.Handle != "" {
		who = "@" + c.Handle + " (" + c.User + ")"
	}
	s.renderLink(w, r, p, linkPage{Notice: "Linked: Slack user " + who + " now files tasks sponsored by @" + login +
		", on the repositories @" + login + " maintains."})
}

// linkUnlink is POST /link/slack/unlink: one of p's own links goes.
func (s *Server) linkUnlink(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}
	p, ok := s.linkHuman(w, r)
	if !ok || !postForm(w, r) {
		return
	}
	team, user := r.PostForm.Get("team"), r.PostForm.Get("user")
	if !slackTeamRE.MatchString(team) || !slackUserRE.MatchString(user) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	gone, err := s.SlackLink.Store.UnlinkSlack(r.Context(), team, user, p.Sub)
	if err != nil {
		s.log().Warn("slack unlink failed", "err", err)
		s.linkCounted(r.Context(), linkError)
		http.Error(w, "unavailable: try again", http.StatusServiceUnavailable)
		return
	}
	page := linkPage{Notice: "Nothing to unlink: that Slack user is not linked to your account."}
	if gone {
		s.linkCounted(r.Context(), linkUnlinked)
		page.Notice = "Unlinked: Slack user " + user + " no longer files as you."
	}
	s.renderLink(w, r, p, page)
}
