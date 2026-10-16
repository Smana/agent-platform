// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Smana/agent-platform/internal/factory/api"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/intake"
	"github.com/Smana/agent-platform/internal/factory/reconciler"
	"github.com/Smana/agent-platform/internal/factory/triage"
	"github.com/Smana/agent-platform/internal/ghidentity"
	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/repoaccess"
	"github.com/Smana/agent-platform/internal/version"
)

// repoConn is one repository's two GitHub connections (v2 D8): the factory App's and the merger's.
type repoConn struct {
	gh     *forge.GitHub
	merger forge.Merger
}

// repoConns holds a connection pair per listed repository, keyed by GitHub's case-insensitive
// name. Every lookup that misses answers "no connection", so nothing acts on an unlisted one.
type repoConns struct {
	order []string
	by    map[string]repoConn
}

// connectRepositories connects both Apps on every listed repository. A repository whose
// installation is missing fails the start, never a silent skip (v2 D8).
func connectRepositories(ctx context.Context, log *slog.Logger, cfg *config.Config) (*repoConns, error) {
	cs := &repoConns{by: map[string]repoConn{}}
	for _, r := range cfg.Repos() {
		gh, err := forge.Connect(ctx, forge.Options{Repository: r.Name, AppIDFile: cfg.GitHub.AppIDFile,
			PrivateKeyFile: cfg.GitHub.PrivateKeyFile, APIURL: githubAPI, UserAgent: "agent-factory/" + version.Version,
			HTTP: httpx.New(githubTimeout, nil), Now: time.Now})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.Name, err)
		}
		// R16 (owner, 2026-09-27): merge-side calls use the merger App's key, which no other
		// workload holds. TW4: its token asks the merger profile, not the factory App's scope.
		merger, err := connectMerger(ctx, log, cfg, r.Name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.Name, err)
		}
		cs.order = append(cs.order, r.Name)
		cs.by[strings.ToLower(r.Name)] = repoConn{gh: gh, merger: merger}
	}
	return cs, nil
}

func (c *repoConns) names() []string { return c.order }

func (c *repoConns) get(name string) (repoConn, bool) {
	rc, ok := c.by[strings.ToLower(name)]
	return rc, ok
}

func (c *repoConns) task(name string) (reconciler.TaskForge, forge.Merger, bool) {
	rc, ok := c.get(name)
	return rc.gh, rc.merger, ok
}

func (c *repoConns) class(name string) (triage.ClassForge, bool) {
	rc, ok := c.get(name)
	return rc.gh, ok
}

func (c *repoConns) schedule(name string) (intake.ScheduleForge, intake.ScheduleMerger, bool) {
	rc, ok := c.get(name)
	return rc.gh, rc.merger, ok
}

// ping keeps every installation token warm; the first failure is the answer.
func (c *repoConns) ping(ctx context.Context) error {
	var errs []error
	for _, name := range c.order {
		rc, _ := c.get(name)
		errs = append(errs, rc.gh.Ping(ctx))
	}
	return errors.Join(errs...)
}

// healthy: every repository's connection answered recently.
func (c *repoConns) healthy(now time.Time) bool {
	for _, name := range c.order {
		if rc, _ := c.get(name); !rc.gh.Healthy(now) {
			return false
		}
	}
	return true
}

// permissions is GitHub's collaborator permission through the factory App on the repository asked
// about (v2 D4, D5), cached for api.access.ttl or the most D4 trusts. A repository with no
// connection is one nobody writes through the factory.
func (c *repoConns) permissions(access *config.Access) *repoaccess.Checker {
	ttl := config.MaxAccessTTL
	if access != nil {
		ttl = access.TTL.Duration
	}
	return &repoaccess.Checker{TTL: ttl, Now: time.Now,
		Perm: func(ctx context.Context, owner, repo, login string) (string, error) {
			rc, ok := c.get(owner + "/" + repo)
			if !ok {
				return "", repoaccess.ErrNoRepository
			}
			return rc.gh.Permission(ctx, login)
		}}
}

// access is v2 D4's check for the run-request API: the caller's GitHub login from their ZITADEL
// link (the broker's reader secret), then write on the repository. Without api.access it is nil,
// and the API refuses every human run.
func (c *repoConns) access(log *slog.Logger, cfg *config.Config, perms *repoaccess.Checker) api.WriteChecker {
	a := cfg.API.Access
	if a == nil {
		log.Warn("api.access is not set: the run-request API refuses every human run (repository_permission, v2 D4)")
		return nil
	}
	id := &ghidentity.Resolver{IDPID: githubIDP, TTL: a.TTL.Duration, Now: time.Now,
		Links: readerLinks(httpx.New(zitadelTimeout, nil), cfg.API.HumanIssuer, a.ReaderFile),
		LoginOf: func(ctx context.Context, repo string, gid int64) (string, error) {
			rc, ok := c.get(repo)
			if !ok {
				return "", repoaccess.ErrNoRepository
			}
			return rc.gh.UserLogin(ctx, gid)
		}}
	return api.GitHubAccess{Identity: id, Perms: perms}
}
