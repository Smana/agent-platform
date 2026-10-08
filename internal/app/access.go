// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Smana/agent-platform/internal/config"
	"github.com/Smana/agent-platform/internal/ghidentity"
	"github.com/Smana/agent-platform/internal/github"
	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/repoaccess"
)

// zitadelTimeout bounds one ZITADEL link read.
const zitadelTimeout = 10 * time.Second

// gitHubUsers is what D7 asks the factory App; *github.App has it.
type gitHubUsers interface {
	UserLogin(ctx context.Context, owner, repo string, id int64) (string, error)
	Permission(ctx context.Context, owner, repo, login string) (string, error)
}

// accessApp is the factory App D7 reads through, mounted at dir (ROOMS_GITHUB_APP_DIR); nil when
// unset. Its own instance: an access check never waits on the verdict poster's token mint.
func accessApp(dir string) gitHubUsers {
	if dir == "" {
		return nil
	}
	return &github.App{Dir: dir, API: githubAPI, HC: httpx.New(posterGitHubTimeout, nil), Now: time.Now}
}

// roomAccess is D7's check from human.access: the caller's GitHub login from their ZITADEL
// link, then its permission on the room's repository, both through gh and cached for the
// config's TTL. Without human.access it returns nils, and the human API shows rooms to admins only.
func roomAccess(h config.HumanConfig, gh gitHubUsers, zitadel *http.Client, now func() time.Time) (*ghidentity.Resolver, *repoaccess.Checker, error) {
	if h.Access == nil {
		return nil, nil, nil
	}
	if gh == nil {
		return nil, nil, errors.New("human.access needs the factory App: ROOMS_GITHUB_APP_DIR is not set")
	}
	ttl := h.Access.TTL.Duration
	id := &ghidentity.Resolver{IDPID: githubIDP, TTL: ttl, Now: now, Links: readerLinks(zitadel, h.Issuer, h.Access.ReaderFile),
		LoginOf: func(ctx context.Context, repo string, id int64) (string, error) {
			owner, name, _ := strings.Cut(repo, "/")
			login, err := gh.UserLogin(ctx, owner, name, id)
			return login, unseen(err)
		}}
	perm := func(ctx context.Context, owner, repo, login string) (string, error) {
		p, err := gh.Permission(ctx, owner, repo, login)
		return p, unseen(err)
	}
	return id, &repoaccess.Checker{Perm: perm, TTL: ttl, Now: now}, nil
}

// unseen marks a repository the App cannot see, or cannot name in a path, as one nobody reads
// through it (repoaccess.ErrNoRepository): a typo or an uninstalled repository is a 404, never a
// 503 that a retry would not heal.
func unseen(err error) error {
	if errors.Is(err, github.ErrNoInstallation) || errors.Is(err, github.ErrNotAPullRequest) {
		return fmt.Errorf("%w: %w", repoaccess.ErrNoRepository, err)
	}
	return err
}

// githubIDP names the GitHub IdP inside the resolver. ZITADEL mints its real id anew on every
// build, so the id comes from the reader secret at use, and readerLinks relabels the matching
// links with this constant.
const githubIDP = "github"

// zitadelReader is the reader secret: the read-only machine user's PAT and the GitHub IdP's id.
// Its tokenId is the provisioning script's, unused here.
type zitadelReader struct {
	PAT         string `json:"pat"`
	GitHubIDPID string `json:"githubIdpId"`
}

// readerLinks lists a user's links to the GitHub IdP with the reader secret at path, read on
// every call, so a rotated PAT or a rebuilt IdP needs no restart. A secret that is missing or
// incomplete is an error: the check fails closed.
func readerLinks(hc *http.Client, issuer, path string) func(ctx context.Context, user string) ([]ghidentity.Link, error) {
	return func(ctx context.Context, user string) ([]ghidentity.Link, error) {
		raw, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return nil, fmt.Errorf("zitadel reader: %w", err)
		}
		var r zitadelReader
		if json.Unmarshal(raw, &r) != nil || r.PAT == "" || r.GitHubIDPID == "" {
			// Never the decoder's text: it may quote the PAT.
			return nil, errors.New(`zitadel reader: not {"pat", "githubIdpId"} JSON`)
		}
		links, err := ghidentity.ZitadelLinks(hc, issuer, r.PAT)(ctx, user)
		if err != nil {
			return nil, err
		}
		var out []ghidentity.Link
		for _, l := range links {
			if l.IDPID == r.GitHubIDPID {
				out = append(out, ghidentity.Link{IDPID: githubIDP, UserID: l.UserID})
			}
		}
		return out, nil
	}
}
