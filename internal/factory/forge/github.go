// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/go-github/v92/github"
	"github.com/shurcooL/githubv4"
)

// freshFor is how recent a success must be for /readyz's "App token fresh" (§6.5).
const freshFor = 3 * time.Minute

var repoRE = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

// Options is how Connect reaches GitHub as the factory App.
type Options struct {
	Repository     string // owner/name: config.repository
	AppIDFile      string // config.github.appIDFile, read at each token mint
	PrivateKeyFile string // config.github.privateKeyFile, read at each token mint
	APIURL         string // https://api.github.com/ in the cluster; a test server in tests
	UserAgent      string
	HTTP           *http.Client // httpx.New: the one audited egress client
	Now            func() time.Time
	// Permissions is the installation token's scope (TW4). nil keeps the factory App's
	// permissions(); the merger connection passes MergerPermissions(), so no factory token
	// ever asks for checks:read the factory App does not hold — a mint beyond the App's
	// scope fails, which would break the factory with it.
	Permissions map[string]string
}

// GitHub is the forge on one repository, as one App installation.
type GitHub struct {
	rest        *github.Client
	v4          *githubv4.Client
	tokens      *tokens
	api         *url.URL
	plain       *http.Client // o.HTTP, never carrying the token: artifact downloads off the API host
	agent       string
	owner, name string
	now         func() time.Time
	last        atomic.Int64 // unix nanoseconds of the last successful call; 0 before any
}

// Connect authenticates as the App, finds its installation on the repository and mints the first
// installation token, so a key GitHub refuses or an App not installed fails startup. Every later
// call goes through o.HTTP, as that installation. It refuses anything but an https API URL.
func Connect(ctx context.Context, o Options) (*GitHub, error) {
	owner, name, _ := strings.Cut(o.Repository, "/")
	api, err := url.Parse(o.APIURL)
	switch {
	case !repoRE.MatchString(o.Repository):
		return nil, fmt.Errorf("forge: repository %q is not owner/name", o.Repository)
	case err != nil || api.Scheme != "https" || api.Host == "" || api.User != nil:
		return nil, errors.New("forge: the API URL must be https://<host>/, without credentials")
	case o.HTTP == nil || o.HTTP.Transport == nil:
		return nil, errors.New("forge: an HTTP client from httpx.New is required")
	case o.Now == nil:
		return nil, errors.New("forge: a clock is required")
	case o.UserAgent == "":
		return nil, errors.New("forge: GitHub requires a user agent")
	}
	if !strings.HasSuffix(api.Path, "/") {
		api.Path += "/"
	}
	g := &GitHub{api: api, plain: o.HTTP, agent: o.UserAgent, owner: owner, name: name, now: o.Now,
		tokens: &tokens{api: api, hc: o.HTTP, owner: owner, name: name, agent: o.UserAgent,
			idFile: o.AppIDFile, keyFile: o.PrivateKeyFile, now: o.Now, perms: o.Permissions}}
	hc := g.authed(o.HTTP)
	base := api.String()
	if g.rest, err = github.NewClient(github.WithHTTPClient(hc), github.WithURLs(&base, &base),
		github.WithUserAgent(o.UserAgent)); err != nil {
		return nil, fmt.Errorf("forge: %w", err)
	}
	v4hc := *hc
	v4hc.Transport = statusRT{base: hc.Transport}
	g.v4 = githubv4.NewEnterpriseClient(api.JoinPath("graphql").String(), &v4hc)
	if _, err := g.tokens.get(ctx); err != nil {
		return nil, fmt.Errorf("forge: %w", err)
	}
	g.mark(nil)
	return g, nil
}

// authed is base with the installation token added on requests to the API host only.
func (g *GitHub) authed(base *http.Client) *http.Client {
	rt := base.Transport
	if rt == nil {
		rt = http.DefaultTransport // never in the binary: Connect requires httpx's transport
	}
	return &http.Client{Timeout: base.Timeout, CheckRedirect: base.CheckRedirect, Jar: base.Jar,
		Transport: &auth{base: rt, host: g.api.Host, agent: g.agent, tokens: g.tokens}}
}

func (g *GitHub) mark(err error) {
	if err == nil {
		g.last.Store(g.now().UnixNano())
	}
}

// Healthy backs /readyz ("App token fresh", §6.5): a call succeeded in the freshFor before now.
func (g *GitHub) Healthy(now time.Time) bool {
	t := g.last.Load()
	if t == 0 {
		return false
	}
	d := now.Sub(time.Unix(0, t))
	return d >= 0 && d < freshFor
}

// Ping runs on every replica every minute; the rate-limit endpoint costs no quota.
func (g *GitHub) Ping(ctx context.Context) error {
	_, _, err := g.rest.RateLimit.Get(ctx)
	g.mark(err)
	return wrap("ping", err)
}

// Labeled lists the issues and pull requests carrying label: one page of 100, since the
// factory removes the trigger label on every decision (R4).
func (g *GitHub) Labeled(ctx context.Context, label string) ([]Item, error) {
	state := "open"
	if label == "factory/revert" { // merged PRs are closed: the revert label lands on them (§6.4)
		state = "all"
	}
	iss, _, err := g.rest.Issues.ListByRepo(ctx, g.owner, g.name, &github.IssueListByRepoOptions{
		State: state, Labels: []string{label}, ListOptions: github.ListOptions{PerPage: 100}})
	g.mark(err)
	if err != nil {
		return nil, wrap("list labelled issues", err)
	}
	out := make([]Item, 0, len(iss))
	for _, i := range iss {
		out = append(out, Item{Number: i.GetNumber(), PullRequest: i.IsPullRequest()})
	}
	return out, nil
}

// LabelEvents are the times label was added to an issue, oldest first; at most ten pages of 100.
// Past them it returns what it read and ErrEventsTruncated: GitHub lists events oldest first, so
// the unread ones are the newest.
func (g *GitHub) LabelEvents(ctx context.Context, number int, label string) ([]LabelEvent, error) {
	var out []LabelEvent
	opt := &github.ListOptions{PerPage: 100}
	truncated := true
	for range 10 {
		evs, resp, err := g.rest.Issues.ListIssueEvents(ctx, g.owner, g.name, number, opt)
		g.mark(err)
		if err != nil {
			return nil, wrap("list issue events", err)
		}
		for _, e := range evs {
			if e.GetEvent() == "labeled" && e.GetLabel().GetName() == label {
				out = append(out, LabelEvent{Actor: e.GetActor().GetLogin(), Label: label, At: e.GetCreatedAt().Time})
			}
		}
		if resp.NextPage == 0 {
			truncated = false
			break
		}
		opt.Page = resp.NextPage
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	if truncated {
		return out, ErrEventsTruncated
	}
	return out, nil
}

// Comment posts body on an issue or pull request.
func (g *GitHub) Comment(ctx context.Context, number int, body string) error {
	_, _, err := g.rest.Issues.CreateComment(ctx, g.owner, g.name, number, github.IssueCommentRequest{Body: body})
	g.mark(err)
	return wrap("comment", err)
}

// recentComments is how many of an issue's newest comments RecentComments returns.
const recentComments = 50

// RecentComments are the 50 newest comments, newest first (R22 searches them for its marker).
// GitHub lists an issue's comments by ascending id and ignores sort and direction there (review
// R1), so the newest are on the last page, and on the one before it when the last is short: one
// to three calls.
func (g *GitHub) RecentComments(ctx context.Context, number int) ([]Comment, error) {
	page := func(n int) ([]*github.IssueComment, *github.Response, error) {
		cs, resp, err := g.rest.Issues.ListComments(ctx, g.owner, g.name, number,
			&github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100, Page: n}})
		g.mark(err)
		if err != nil {
			return nil, nil, wrap("list comments", err)
		}
		return cs, resp, nil
	}
	cs, resp, err := page(1)
	if err != nil {
		return nil, err
	}
	if last := resp.LastPage; last > 1 {
		if cs, _, err = page(last); err != nil {
			return nil, err
		}
		if len(cs) < recentComments {
			prev, _, err := page(last - 1)
			if err != nil {
				return nil, err
			}
			cs = append(prev, cs...)
		}
	}
	cs = cs[max(len(cs)-recentComments, 0):]
	out := make([]Comment, 0, len(cs))
	for i := len(cs) - 1; i >= 0; i-- {
		c := cs[i]
		out = append(out, Comment{ID: c.GetID(), Author: c.GetUser().GetLogin(), Body: c.GetBody(), At: c.GetCreatedAt().Time,
			Edited: c.GetUpdatedAt().After(c.GetCreatedAt().Time)})
	}
	return out, nil
}

// AddLabels adds labels to an issue or pull request.
func (g *GitHub) AddLabels(ctx context.Context, number int, labels ...string) error {
	_, _, err := g.rest.Issues.AddLabelsToIssue(ctx, g.owner, g.name, number, labels)
	g.mark(err)
	return wrap("add labels", err)
}

// Repository is the owner/name this connection is bound to.
func (g *GitHub) Repository() string { return g.owner + "/" + g.name }

// githubLogin is GitHub's login syntax, checked before a login is spliced into a path.
var githubLogin = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

// Permission is login's permission on the repository as GitHub's collaborator API states it
// (admin, maintain, write, triage, read or none); GitHub's 404 — a login it does not know, or one
// with no access — is "none" (v2 D4, D5). Any other failure is an error, never an answer.
func (g *GitHub) Permission(ctx context.Context, login string) (string, error) {
	if !githubLogin.MatchString(login) {
		return "", errors.New("forge: not a GitHub login")
	}
	p, resp, err := g.rest.Repositories.GetPermissionLevel(ctx, g.owner, g.name, login)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		g.mark(nil)
		return "none", nil
	}
	g.mark(err)
	switch {
	case err != nil:
		return "", wrap("permission", err)
	case p.GetPermission() == "":
		return "", errors.New("forge: permission: the reply has no permission")
	}
	return p.GetPermission(), nil
}

// UserLogin is GitHub user id's current login. The id is stable across a rename, the login is
// not, so the D4 lookup resolves the id each time its cache expires.
func (g *GitHub) UserLogin(ctx context.Context, id int64) (string, error) {
	if id <= 0 {
		return "", errors.New("forge: not a GitHub user id")
	}
	u, _, err := g.rest.Users.GetByID(ctx, id)
	g.mark(err)
	if err != nil {
		return "", wrap("user", err)
	}
	if u.GetLogin() == "" {
		return "", errors.New("forge: user: the reply has no login")
	}
	return u.GetLogin(), nil
}

// RemoveLabel removes a label; one already gone (404) is not an error.
func (g *GitHub) RemoveLabel(ctx context.Context, number int, label string) error {
	resp, err := g.rest.Issues.RemoveLabelForIssue(ctx, g.owner, g.name, number, label)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		g.mark(nil)
		return nil
	}
	g.mark(err)
	return wrap("remove a label", err)
}

// CreateIssue opens an issue with these labels and returns it, with its number and the numeric id
// the sub-issues API takes (FA-8: the RunLore intake proposes one per finding; splitter SL8: a split
// opens its parts). issues:write is in the factory App's scope already.
func (g *GitHub) CreateIssue(ctx context.Context, title, body string, labels []string) (Issue, error) {
	iss, _, err := g.rest.Issues.Create(ctx, g.owner, g.name, github.CreateIssueRequest{
		Title: title, Body: new(body), Labels: labels})
	g.mark(err)
	if err != nil {
		return Issue{}, wrap("create an issue", err)
	}
	return restIssue(iss), nil
}

// restIssue is a REST issue as the forge reads one.
func restIssue(i *github.Issue) Issue {
	out := Issue{Number: i.GetNumber(), ID: i.GetID(), URL: i.GetHTMLURL(), Title: i.GetTitle(), Body: i.GetBody(),
		State: strings.ToUpper(i.GetState()), Author: i.GetUser().GetLogin()}
	for _, l := range i.Labels {
		out.Labels = append(out.Labels, l.GetName())
	}
	return out
}

// IssuesLabelled are the issues carrying label, open or closed: one page of 100, since a split
// opens at most five parts under one label. Pull requests are left out.
func (g *GitHub) IssuesLabelled(ctx context.Context, label string) ([]Issue, error) {
	iss, _, err := g.rest.Issues.ListByRepo(ctx, g.owner, g.name, &github.IssueListByRepoOptions{
		State: "all", Labels: []string{label}, ListOptions: github.ListOptions{PerPage: 100}})
	g.mark(err)
	if err != nil {
		return nil, wrap("list labelled issues", err)
	}
	var out []Issue
	for _, i := range iss {
		if !i.IsPullRequest() {
			out = append(out, restIssue(i))
		}
	}
	return out, nil
}

// AddSubIssue shows child under parent as a GitHub sub-issue, by the child's numeric id: a view for
// people (splitter SL8); the labels are the record.
func (g *GitHub) AddSubIssue(ctx context.Context, parent int, childID int64) error {
	_, _, err := g.rest.SubIssue.Add(ctx, g.owner, g.name, int64(parent), github.SubIssueRequest{SubIssueID: childID})
	g.mark(err)
	return wrap("add a sub-issue", err)
}

// CloseIssue closes an issue as completed (splitter SL12: a parent whose parts all merged).
func (g *GitHub) CloseIssue(ctx context.Context, number int) error {
	_, _, err := g.rest.Issues.Update(ctx, g.owner, g.name, number,
		github.UpdateIssueRequest{State: new("closed"), StateReason: new("completed")})
	g.mark(err)
	return wrap("close an issue", err)
}

// ClosePR closes a pull request without merging it (§6.3's stale close).
func (g *GitHub) ClosePR(ctx context.Context, number int) error {
	_, _, err := g.rest.PullRequests.Edit(ctx, g.owner, g.name, number, &github.PullRequest{State: new("closed")})
	g.mark(err)
	return wrap("close a pull request", err)
}

// PullRequestForBranch is the newest pull request from branch in the repository, 0 when none.
func (g *GitHub) PullRequestForBranch(ctx context.Context, branch string) (int, error) {
	if branch == "" {
		return 0, errors.New("forge: no branch") // an empty head filter would match every pull request
	}
	prs, _, err := g.rest.PullRequests.List(ctx, g.owner, g.name, &github.PullRequestListOptions{
		State: "all", Head: g.owner + ":" + branch, ListOptions: github.ListOptions{PerPage: 10}})
	g.mark(err)
	if err != nil {
		return 0, wrap("list pull requests", err)
	}
	if len(prs) == 0 {
		return 0, nil
	}
	return prs[0].GetNumber(), nil // newest first
}

// AgentPulls are the repository's open pull requests from `agent/` branches (R51's orphan scan),
// newest first, one page of 100: the head branch, its labels, and whether the head is this
// repository's own branch. GitHub's pullRequests connection does not accept headRefPrefix (it is
// a search argument; verified against the live API 2026-10-04), so the newest 100 open PRs of ANY
// prefix are fetched and filtered client-side to `agent/` heads. Tradeoff: an `agent/` PR pushed
// out of the newest-100 window by 100+ newer non-agent PRs is missed for that poll; the window
// slides every poll and labelling shrinks the set, so any backlog is walked, and this
// repository's volume makes the miss theoretical. The scan judges the rest.
func (g *GitHub) AgentPulls(ctx context.Context) ([]AgentPull, error) {
	var q struct {
		Repository struct {
			PullRequests struct {
				Nodes []struct {
					Number         int
					HeadRefName    string
					HeadRepository *struct {
						Owner struct{ Login string }
						Name  string
					} `graphql:"headRepository"`
					Labels struct{ Nodes []struct{ Name string } } `graphql:"labels(first: 30)"`
				}
			} `graphql:"pullRequests(states: [OPEN], first: 100, orderBy: {field: CREATED_AT, direction: DESC})"`
		} `graphql:"repository(owner: $owner, name: $name)"`
	}
	vars := map[string]any{"owner": githubv4.String(g.owner), "name": githubv4.String(g.name)}
	err := g.v4.Query(ctx, &q, vars)
	g.mark(err)
	if err != nil {
		return nil, wrap("list agent pull requests", err)
	}
	out := make([]AgentPull, 0, len(q.Repository.PullRequests.Nodes))
	for _, p := range q.Repository.PullRequests.Nodes {
		if !strings.HasPrefix(p.HeadRefName, "agent/") { // the connection cannot filter heads; do it here
			continue
		}
		// A deleted head repository is nobody's branch: never the factory's.
		own := p.HeadRepository != nil && SameLogin(p.HeadRepository.Owner.Login, g.owner) && p.HeadRepository.Name == g.name
		ap := AgentPull{Number: p.Number, HeadRef: p.HeadRefName, Fork: !own}
		for _, l := range p.Labels.Nodes {
			ap.Labels = append(ap.Labels, l.Name)
		}
		out = append(out, ap)
	}
	return out, nil
}

// PullsWithLabel lists the newest 100 pull requests carrying label, by creation, in every state.
// The breaker reads a window of 10 merges, so a class's 100 newest labelled ones hold it.
func (g *GitHub) PullsWithLabel(ctx context.Context, label string) ([]LabelledPull, error) {
	var q struct {
		Repository struct {
			PullRequests struct {
				Nodes []struct {
					Number   int
					State    string
					MergedAt *githubv4.DateTime
				}
			} `graphql:"pullRequests(labels: $labels, first: 100, orderBy: {field: CREATED_AT, direction: DESC})"`
		} `graphql:"repository(owner: $owner, name: $name)"`
	}
	vars := map[string]any{"owner": githubv4.String(g.owner), "name": githubv4.String(g.name),
		"labels": []githubv4.String{githubv4.String(label)}}
	err := g.v4.Query(ctx, &q, vars)
	g.mark(err)
	if err != nil {
		return nil, wrap("list labelled pull requests", err)
	}
	out := make([]LabelledPull, 0, len(q.Repository.PullRequests.Nodes))
	for _, p := range q.Repository.PullRequests.Nodes {
		lp := LabelledPull{Number: p.Number, State: p.State}
		if p.MergedAt != nil {
			lp.MergedAt = p.MergedAt.Time
		}
		out = append(out, lp)
	}
	return out, nil
}

// actor is GraphQL's Actor interface. A bot's login has no "[bot]" there; REST has it.
type actor struct {
	Typename string `graphql:"__typename"`
	Login    string
}

func (a *actor) login() string {
	if a == nil {
		return ""
	}
	if a.Typename == "Bot" {
		return a.Login + "[bot]"
	}
	return a.Login
}

func (g *GitHub) vars(number int) (map[string]any, error) {
	if number < 1 || number > math.MaxInt32 {
		return nil, fmt.Errorf("forge: %d is not an issue or pull request number", number)
	}
	return map[string]any{"owner": githubv4.String(g.owner), "name": githubv4.String(g.name),
		"number": githubv4.Int(int32(number))}, nil
}

// Issue is one GraphQL snapshot of an issue: text, labels, the body's last edit and the last
// title rename.
func (g *GitHub) Issue(ctx context.Context, number int) (Issue, error) {
	var q struct {
		Repository struct {
			Issue struct {
				Number       int
				URL          string `graphql:"url"`
				Title        string
				Body         string
				State        string
				Author       *actor
				LastEditedAt *githubv4.DateTime
				Labels       struct{ Nodes []struct{ Name string } } `graphql:"labels(first: 50)"`
				Renames      struct {
					Nodes []struct {
						RenamedTitleEvent struct{ CreatedAt githubv4.DateTime } `graphql:"... on RenamedTitleEvent"`
					}
				} `graphql:"timelineItems(itemTypes: [RENAMED_TITLE_EVENT], last: 1)"`
			} `graphql:"issue(number: $number)"`
		} `graphql:"repository(owner: $owner, name: $name)"`
	}
	vars, err := g.vars(number)
	if err != nil {
		return Issue{}, err
	}
	err = g.v4.Query(ctx, &q, vars)
	g.mark(err)
	if err != nil {
		return Issue{}, wrap("read an issue", err)
	}
	i := q.Repository.Issue
	out := Issue{Number: i.Number, URL: i.URL, Title: i.Title, Body: i.Body, State: i.State, Author: i.Author.login()}
	if i.LastEditedAt != nil {
		out.LastEditedAt = i.LastEditedAt.Time
	}
	if n := i.Renames.Nodes; len(n) == 1 {
		out.TitleEditedAt = n[0].RenamedTitleEvent.CreatedAt.Time
	}
	for _, l := range i.Labels.Nodes {
		out.Labels = append(out.Labels, l.Name)
	}
	return out, nil
}

// PullRequest is one GraphQL query per PR per poll (§1: "each active PR every 30 s in one
// GraphQL query"): state, reviews, comments and the head commit's message.
func (g *GitHub) PullRequest(ctx context.Context, number int) (PR, error) {
	var q struct {
		Repository struct {
			PullRequest struct {
				ID               string
				Number           int
				URL              string `graphql:"url"`
				Title            string
				State            string
				HeadRefName      string
				HeadRefOid       string
				Author           *actor
				MergedBy         *actor
				MergedAt         *githubv4.DateTime
				MergeCommit      *struct{ Oid string }
				AutoMergeRequest *struct{ EnabledAt githubv4.DateTime }
				Labels           struct{ Nodes []struct{ Name string } } `graphql:"labels(first: 30)"`
				Reviews          struct {
					Nodes []struct {
						DatabaseID  int64 `graphql:"databaseId"`
						State       string
						Body        string
						SubmittedAt *githubv4.DateTime
						Author      *actor
						Comments    struct {
							Nodes []struct {
								Path string
								Line *int
								Body string
							}
						} `graphql:"comments(first: 30)"`
					}
				} `graphql:"reviews(last: 20)"`
				Comments struct {
					Nodes []struct {
						DatabaseID   int64 `graphql:"databaseId"`
						Body         string
						CreatedAt    githubv4.DateTime
						LastEditedAt *githubv4.DateTime
						Author       *actor
					}
				} `graphql:"comments(last: 30)"`
				Commits struct {
					Nodes []struct {
						Commit struct {
							Message       string
							CommittedDate githubv4.DateTime
						}
					}
				} `graphql:"commits(last: 1)"`
			} `graphql:"pullRequest(number: $number)"`
		} `graphql:"repository(owner: $owner, name: $name)"`
	}
	vars, err := g.vars(number)
	if err != nil {
		return PR{}, err
	}
	err = g.v4.Query(ctx, &q, vars)
	g.mark(err)
	if err != nil {
		return PR{}, wrap("read a pull request", err)
	}
	p := q.Repository.PullRequest
	out := PR{Number: p.Number, NodeID: p.ID, URL: p.URL, Title: p.Title, State: p.State, Author: p.Author.login(),
		HeadRef: p.HeadRefName, HeadSHA: p.HeadRefOid, MergedBy: p.MergedBy.login(), AutoMerge: p.AutoMergeRequest != nil}
	if p.MergeCommit != nil {
		out.MergeCommitSHA = p.MergeCommit.Oid
	}
	if p.MergedAt != nil {
		out.MergedAt = p.MergedAt.Time
	}
	for _, l := range p.Labels.Nodes {
		out.Labels = append(out.Labels, l.Name)
	}
	for _, r := range p.Reviews.Nodes {
		rv := Review{ID: r.DatabaseID, Author: r.Author.login(), State: r.State, Body: r.Body}
		if r.SubmittedAt != nil {
			rv.At = r.SubmittedAt.Time
		}
		for _, c := range r.Comments.Nodes {
			line := 0
			if c.Line != nil {
				line = *c.Line
			}
			rv.Comments = append(rv.Comments, ReviewComment{Path: c.Path, Line: line, Body: c.Body})
		}
		out.Reviews = append(out.Reviews, rv)
	}
	for _, c := range p.Comments.Nodes {
		out.Comments = append(out.Comments, Comment{ID: c.DatabaseID, Author: c.Author.login(), Body: c.Body, At: c.CreatedAt.Time,
			Edited: c.LastEditedAt != nil})
	}
	if len(p.Commits.Nodes) == 1 {
		out.HeadMessage, out.HeadCommittedAt = p.Commits.Nodes[0].Commit.Message, p.Commits.Nodes[0].Commit.CommittedDate.Time
	}
	return out, nil
}

// checkState maps a check run's status and conclusion to SUCCESS | FAILURE | CANCELLED | PENDING. Both the
// GraphQL rollup and the REST check runs use the same words; REST spells them lowercase.
func checkState(status, conclusion string) string {
	if status != "COMPLETED" {
		return "PENDING"
	}
	switch conclusion {
	case "SUCCESS", "NEUTRAL", "SKIPPED":
		return "SUCCESS"
	case "CANCELLED":
		return "CANCELLED"
	}
	return "FAILURE"
}

// RerunCheck re-runs the Actions job whose id is the check run's. Re-running a job whose run is
// not complete is refused by GitHub (403), which the reconciler reads as a failed attempt.
func (g *GitHub) RerunCheck(ctx context.Context, id int64) error {
	_, err := g.rest.Actions.RerunJobByID(ctx, g.owner, g.name, id)
	g.mark(err)
	if err != nil {
		return wrap("re-run a check", err)
	}
	return nil
}

// PullRequestChecks is the head commit's rollup, one GraphQL query. It needs checks: read and
// statuses: read (R16), hence the merger App's own method.
func (g *GitHub) PullRequestChecks(ctx context.Context, number int) (Checks, error) {
	var q struct {
		Repository struct {
			PullRequest struct {
				Commits struct {
					Nodes []struct {
						Commit struct {
							StatusCheckRollup *struct {
								Contexts struct {
									PageInfo struct{ HasNextPage bool }
									Nodes    []struct {
										Typename string `graphql:"__typename"`
										CheckRun struct {
											DatabaseID int64
											Name       string
											Status     string
											Conclusion string
											CheckSuite struct {
												App *struct{ DatabaseID int64 }
											}
										} `graphql:"... on CheckRun"`
										StatusContext struct {
											Context string
											State   string
											Creator *actor
										} `graphql:"... on StatusContext"`
									}
								} `graphql:"contexts(first: 100)"`
							}
						}
					}
				} `graphql:"commits(last: 1)"`
			} `graphql:"pullRequest(number: $number)"`
		} `graphql:"repository(owner: $owner, name: $name)"`
	}
	vars, err := g.vars(number)
	if err != nil {
		return Checks{}, err
	}
	err = g.v4.Query(ctx, &q, vars)
	g.mark(err)
	if err != nil {
		return Checks{}, wrap("read pull request checks", err)
	}
	var out Checks
	if len(q.Repository.PullRequest.Commits.Nodes) == 0 {
		return out, nil
	}
	rollup := q.Repository.PullRequest.Commits.Nodes[0].Commit.StatusCheckRollup
	if rollup == nil {
		return out, nil
	}
	out.Truncated = rollup.Contexts.PageInfo.HasNextPage || len(rollup.Contexts.Nodes) >= 100
	for _, n := range rollup.Contexts.Nodes {
		switch n.Typename {
		case "CheckRun":
			c := Check{ID: n.CheckRun.DatabaseID, Name: n.CheckRun.Name, State: checkState(n.CheckRun.Status, n.CheckRun.Conclusion), Conclusion: n.CheckRun.Conclusion}
			if a := n.CheckRun.CheckSuite.App; a != nil {
				c.App = a.DatabaseID
			}
			out.Runs = append(out.Runs, c)
		case "StatusContext":
			out.Statuses = append(out.Statuses, Status{Context: n.StatusContext.Context, State: n.StatusContext.State, Creator: n.StatusContext.Creator.login()})
		}
	}
	return out, nil
}

// CommitChecks are the check runs on one commit: main's CI after a merge, where no pull request
// carries the verdict anymore.
func (g *GitHub) CommitChecks(ctx context.Context, sha string) ([]Check, error) {
	res, _, err := g.rest.Checks.ListCheckRunsForRef(ctx, g.owner, g.name, sha, &github.ListCheckRunsOptions{ListOptions: github.ListOptions{PerPage: 100}})
	g.mark(err)
	if err != nil {
		return nil, wrap("list check runs", err)
	}
	out := make([]Check, 0, len(res.CheckRuns))
	for _, cr := range res.CheckRuns {
		out = append(out, Check{ID: cr.GetID(), Name: cr.GetName(), State: checkState(strings.ToUpper(cr.GetStatus()), strings.ToUpper(cr.GetConclusion())),
			Conclusion: strings.ToUpper(cr.GetConclusion())})
	}
	return out, nil
}

// Merge merges the pull request now, at the decided head (external review R02, ruling R52):
// mergePullRequest with expectedHeadOid checks the head at merge time, so a push that lands
// after the decision refuses the merge instead of merging on the old decision — which is what
// enablePullRequestAutoMerge's enable-time-only check failed to guarantee. GitHub answers a
// moved head with 409 and a disallowed merge with 405; the sentinels are what the reconciler
// decides on. squash always: the factory's one commit per task is the history.
func (g *GitHub) Merge(ctx context.Context, nodeID, head string) error {
	if head == "" {
		return errors.New("forge: Merge needs the decided head: without expectedHeadOid any push since the decision merges")
	}
	var m struct {
		MergePullRequest struct{ ClientMutationID *string } `graphql:"mergePullRequest(input: $input)"`
	}
	squash := githubv4.PullRequestMergeMethodSquash
	oid := githubv4.GitObjectID(head)
	err := g.v4.Mutate(ctx, &m, githubv4.MergePullRequestInput{PullRequestID: githubv4.ID(nodeID), MergeMethod: &squash, ExpectedHeadOid: &oid}, nil)
	g.mark(err)
	if err == nil {
		return nil
	}
	// These two refusals are decisions, not transport failures, so they read as the sentinels
	// the reconciler branches on. The status comes from the response (statusRT), never from
	// the error text, which a GraphQL message can fill with anything.
	var se *statusError
	if errors.As(err, &se) {
		switch se.code {
		case http.StatusConflict:
			return fmt.Errorf("forge: merge: %w", ErrHeadMoved)
		case http.StatusMethodNotAllowed:
			return fmt.Errorf("forge: merge: %w", ErrNotMergeable)
		}
	}
	return wrap("merge", err)
}

// EnableAutoMerge arms GitHub's native auto-merge — for the revert PR alone (R52): it waits for
// every required check and merges even with the factory down, which for a revert of red main
// (§6.4) is the safety net, not the fail-open the decision path must not be. The revert branch
// is the merger App's own, so no later push can move the head it merges.
func (g *GitHub) EnableAutoMerge(ctx context.Context, nodeID, expectedHeadSHA string) error {
	var m struct {
		EnablePullRequestAutoMerge struct{ ClientMutationID *string } `graphql:"enablePullRequestAutoMerge(input: $input)"`
	}
	squash := githubv4.PullRequestMergeMethodSquash
	in := githubv4.EnablePullRequestAutoMergeInput{PullRequestID: githubv4.ID(nodeID), MergeMethod: &squash}
	if expectedHeadSHA != "" {
		oid := githubv4.GitObjectID(expectedHeadSHA)
		in.ExpectedHeadOid = &oid
	}
	err := g.v4.Mutate(ctx, &m, in, nil)
	g.mark(err)
	return wrap("arm auto-merge", err)
}

// DisableAutoMerge disarms a revert that never went green (§6.4): the stalled revert is handed
// to a maintainer rather than left armed indefinitely. The decision path never arms, so it never
// disarms (R52).
func (g *GitHub) DisableAutoMerge(ctx context.Context, nodeID string) error {
	var m struct {
		DisablePullRequestAutoMerge struct{ ClientMutationID *string } `graphql:"disablePullRequestAutoMerge(input: $input)"`
	}
	err := g.v4.Mutate(ctx, &m, githubv4.DisablePullRequestAutoMergeInput{PullRequestID: githubv4.ID(nodeID)}, nil)
	g.mark(err)
	return wrap("disarm auto-merge", err)
}

// RevertPR produces the revert PR GitHub's own button would (§6.4); its revert-<n>-<head> branch is
// created by the merger App, which is why only that App bypasses agent-merge (R16).
func (g *GitHub) RevertPR(ctx context.Context, nodeID, title, body string) (Revert, error) {
	var m struct {
		RevertPullRequest struct {
			RevertPullRequest struct {
				ID     string
				Number int
				URL    string `graphql:"url"`
			}
		} `graphql:"revertPullRequest(input: $input)"`
	}
	t, b := githubv4.String(title), githubv4.String(body)
	err := g.v4.Mutate(ctx, &m, githubv4.RevertPullRequestInput{PullRequestID: githubv4.ID(nodeID), Title: &t, Body: &b}, nil)
	g.mark(err)
	r := m.RevertPullRequest.RevertPullRequest
	return Revert{Number: r.Number, URL: r.URL, NodeID: r.ID}, wrap("open a revert", err)
}

// maxPRCommits is the most commits GitHub lists for a pull request.
const maxPRCommits = 250

// PullRequestCommits are the pull request's commits, oldest first: three pages of 100 at most.
// A list that reaches GitHub's 250 is returned with ErrCommitsTruncated — it may be all of them,
// or the first 250 of more, and GitHub does not say which.
func (g *GitHub) PullRequestCommits(ctx context.Context, number int) ([]Commit, error) {
	var out []Commit
	opt := &github.ListOptions{PerPage: 100}
	for {
		cs, resp, err := g.rest.PullRequests.ListCommits(ctx, g.owner, g.name, number, opt)
		g.mark(err)
		if err != nil {
			return nil, wrap("list pull request commits", err)
		}
		for _, c := range cs {
			out = append(out, Commit{SHA: c.GetSHA(), Author: c.GetAuthor().GetLogin(), Message: c.GetCommit().GetMessage()})
		}
		if len(out) >= maxPRCommits {
			return out, ErrCommitsTruncated
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opt.Page = resp.NextPage
	}
}

// maxActivityPages bounds BranchActivity: 300 moves of one branch since its creation.
const maxActivityPages = 3

// BranchActivity is the branch's activity newest first, back to its latest creation and no
// further: what precedes it is an earlier branch of the same name. GitHub's repository activity
// API (contents: read) pages by cursor; a history whose creation is not reached within
// maxActivityPages returns ErrActivityTruncated.
func (g *GitHub) BranchActivity(ctx context.Context, branch string) ([]Push, error) {
	var out []Push
	opt := &github.ListRepositoryActivityOptions{Ref: "refs/heads/" + branch, PerPage: 100}
	for range maxActivityPages {
		as, resp, err := g.rest.Repositories.ListRepositoryActivities(ctx, g.owner, g.name, opt)
		g.mark(err)
		if err != nil {
			return nil, wrap("list branch activity", err)
		}
		for _, a := range as {
			out = append(out, Push{Type: a.ActivityType, Actor: a.GetActor().GetLogin(), Before: a.Before, After: a.After,
				At: a.GetTimestamp().Time})
			if a.ActivityType == "branch_creation" {
				return out, nil
			}
		}
		if resp.After == "" {
			return out, nil // the branch's whole recorded history, its creation never recorded
		}
		opt.After = resp.After
	}
	return out, ErrActivityTruncated
}

// OpenPullRequests are the repository's open pull requests, one page of 100: the factory App's
// read of the queue (the merger reads PR state through Merger, not here).
func (g *GitHub) OpenPullRequests(ctx context.Context) ([]PRSummary, error) {
	prs, _, err := g.rest.PullRequests.List(ctx, g.owner, g.name, &github.PullRequestListOptions{State: "open", ListOptions: github.ListOptions{PerPage: 100}})
	g.mark(err)
	if err != nil {
		return nil, wrap("list open pull requests", err)
	}
	out := make([]PRSummary, 0, len(prs))
	for _, p := range prs {
		out = append(out, PRSummary{Number: p.GetNumber(), Author: p.GetUser().GetLogin(), Created: p.GetCreatedAt().Time})
	}
	return out, nil
}

// Diff is the contents of every file head changes since it forked from base, read at their
// merge base and at head — what a squash merge applies, not head against base's tip: an added
// file's base side and a deleted file's head side read "", so the maps' union is the changed
// set. A rename reads as its delete beside its add — which is right: a rename is not a link
// retarget. §5.1's links-only test for the docs classes (R52) decides on these.
func (g *GitHub) Diff(ctx context.Context, base, head string) (Diff, error) {
	cmp, _, err := g.rest.Repositories.CompareCommits(ctx, g.owner, g.name, base, head, &github.ListOptions{PerPage: 100})
	g.mark(err)
	if err != nil {
		return Diff{}, wrap("compare commits", err)
	}
	if len(cmp.Files) == 100 { // one page exactly: unseen files must not let a diff pass as links-only
		return Diff{}, fmt.Errorf("%w: 100 files differ between %s and %s, the links-only test cannot see them all", ErrDiffTooLarge, base, head)
	}
	// The file list is three-dot: changed since the merge base. Reading the base side at base's
	// tip instead would fold in whatever landed there since the branch was cut
	// (cloud-native-ref#2515), so both sides come from the commits the list compares.
	mergeBase := cmp.GetMergeBaseCommit().GetSHA()
	if mergeBase == "" {
		return Diff{}, fmt.Errorf("forge: comparing %s and %s returned no merge base: the links-only test cannot tell the PR's change from %s's", base, head, base)
	}
	d := Diff{Base: make(map[string]string, len(cmp.Files)), Head: make(map[string]string, len(cmp.Files))}
	for _, f := range cmp.Files {
		p := f.GetFilename()
		d.Files = append(d.Files, FileStat{Path: p, Previous: f.GetPreviousFilename(), Status: f.GetStatus(),
			Additions: f.GetAdditions(), Deletions: f.GetDeletions()})
		hc, err := g.fileAt(ctx, p, head)
		if err != nil {
			return Diff{}, err
		}
		bp := f.GetPreviousFilename()
		if bp == "" {
			bp = p
		}
		bc, err := g.fileAt(ctx, bp, mergeBase)
		if err != nil {
			return Diff{}, err
		}
		d.Head[p], d.Base[bp] = hc, bc
	}
	return d, nil
}

// fileAt is one file's content at one commit. Absent there — an added file's base, a deleted
// file's head — reads "". A file GitHub will not inline (over a megabyte, a submodule) is an
// error: the links-only test must see every byte of the diff or refuse to decide it.
func (g *GitHub) fileAt(ctx context.Context, path, ref string) (string, error) {
	fc, _, resp, err := g.rest.Repositories.GetContents(ctx, g.owner, g.name, path, &github.RepositoryContentGetOptions{Ref: ref})
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		g.mark(nil)
		return "", nil
	}
	g.mark(err)
	if err != nil {
		return "", wrap("read a file", err)
	}
	if fc == nil {
		return "", fmt.Errorf("forge: %s at %s is not a file", path, ref)
	}
	text, err := fc.GetContent()
	if err != nil {
		return "", wrap("decode a file", err)
	}
	if text == "" && fc.GetSize() > 0 {
		return "", fmt.Errorf("forge: %s at %s is %d bytes and not readable through the contents API (%s)", path, ref, fc.GetSize(), fc.GetType())
	}
	return text, nil
}

// FenceFile is the ownership fence (v2 D7) on the default branch, found false when the repository
// has none. Any failure other than GitHub's 404 is an error: the caller keeps its last answer.
func (g *GitHub) FenceFile(ctx context.Context) ([]byte, bool, error) {
	fc, _, resp, err := g.rest.Repositories.GetContents(ctx, g.owner, g.name, ".github/agent-factory.yaml", nil)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		g.mark(nil)
		return nil, false, nil
	}
	g.mark(err)
	if err != nil {
		return nil, false, wrap("read the fence file", err)
	}
	if fc == nil {
		return nil, false, errors.New("forge: the fence file is not a file")
	}
	text, err := fc.GetContent()
	if err != nil {
		return nil, false, wrap("decode the fence file", err)
	}
	return []byte(text), true, nil
}

// DefaultBranchFile is path on the default branch, its text and git blob sha; found is false on
// GitHub's 404. A path that is never a readable file (a directory, over 1 MB, a symlink, a
// submodule) is ErrNotAFile, with the sha when GitHub gave one. Any other failure (5xx, 403,
// network) is a retryable error: triage retries rather than read "no note" (SC4).
func (g *GitHub) DefaultBranchFile(ctx context.Context, path string) (text, sha string, found bool, err error) {
	fc, _, resp, err := g.rest.Repositories.GetContents(ctx, g.owner, g.name, path, nil)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		g.mark(nil)
		return "", "", false, nil
	}
	g.mark(err)
	if err != nil {
		return "", "", false, wrap("read "+path, err)
	}
	if fc == nil { // a directory listing
		return "", "", false, fmt.Errorf("forge: %s is a directory: %w", path, ErrNotAFile)
	}
	if t := fc.GetType(); t != "file" {
		return "", fc.GetSHA(), false, fmt.Errorf("forge: %s is a %s: %w", path, t, ErrNotAFile)
	}
	if fc.GetPath() != path { // a symlink to a file: the API answers with the target, whose bytes are not this path's
		return "", fc.GetSHA(), false, fmt.Errorf("forge: %s resolves to %s: %w", path, fc.GetPath(), ErrNotAFile)
	}
	if fc.GetEncoding() == "none" { // the contents API withholds the body of a file over 1 MB
		return "", fc.GetSHA(), false, fmt.Errorf("forge: %s is %d bytes and not readable through the contents API: %w", path, fc.GetSize(), ErrNotAFile)
	}
	if text, err = fc.GetContent(); err != nil {
		return "", "", false, wrap("decode "+path, err)
	}
	if text == "" && fc.GetSize() > 0 {
		return "", fc.GetSHA(), false, fmt.Errorf("forge: %s is %d bytes and not readable through the contents API: %w", path, fc.GetSize(), ErrNotAFile)
	}
	return text, fc.GetSHA(), true, nil
}

func wrap(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("forge: %s: %w", what, err)
}
