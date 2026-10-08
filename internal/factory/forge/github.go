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
}

// GitHub is the forge on one repository, as one App installation.
type GitHub struct {
	rest        *github.Client
	v4          *githubv4.Client
	tokens      *tokens
	api         *url.URL
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
	g := &GitHub{api: api, agent: o.UserAgent, owner: owner, name: name, now: o.Now,
		tokens: &tokens{api: api, hc: o.HTTP, owner: owner, name: name, agent: o.UserAgent,
			idFile: o.AppIDFile, keyFile: o.PrivateKeyFile, now: o.Now}}
	hc := g.authed(o.HTTP)
	base := api.String()
	if g.rest, err = github.NewClient(github.WithHTTPClient(hc), github.WithURLs(&base, &base),
		github.WithUserAgent(o.UserAgent)); err != nil {
		return nil, fmt.Errorf("forge: %w", err)
	}
	g.v4 = githubv4.NewEnterpriseClient(api.JoinPath("graphql").String(), hc)
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

// Labeled lists the open issues and pull requests carrying label: one page of 100, since the
// factory removes the trigger label on every decision (R4).
func (g *GitHub) Labeled(ctx context.Context, label string) ([]Item, error) {
	iss, _, err := g.rest.Issues.ListByRepo(ctx, g.owner, g.name, &github.IssueListByRepoOptions{
		State: "open", Labels: []string{label}, ListOptions: github.ListOptions{PerPage: 100}})
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
	out := Issue{Number: i.Number, URL: i.URL, Title: i.Title, Body: i.Body, State: i.State}
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

func wrap(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("forge: %s: %w", what, err)
}
