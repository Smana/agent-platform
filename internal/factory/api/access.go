// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"

	factoryv1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
)

// GitHubAccess is v2 D4's WriteChecker: the principal's GitHub login from their ZITADEL link (the
// broker forwards the token, never a login), then that login's permission on the repository.
// No link is no write; a lookup that fails is an error, which the server refuses.
type GitHubAccess struct {
	Identity interface {
		Login(ctx context.Context, sub, repository string) (string, error)
	}
	Perms interface {
		CanWrite(ctx context.Context, repository, login string) (bool, error)
	}
}

// CanWrite implements WriteChecker.
func (a GitHubAccess) CanWrite(ctx context.Context, p authn.Principal, repository string) (bool, error) {
	if p.Sub == "" {
		return false, nil
	}
	login, err := a.Identity.Login(ctx, p.Sub, repository)
	if err != nil {
		return false, fmt.Errorf("access: %w", err)
	}
	if login == "" {
		return false, nil
	}
	return a.Perms.CanWrite(ctx, repository, login)
}

// Person implements PersonResolver (v2 per-person caps D1): github:<login> from the same link
// CanWrite reads, "" with no sub or no link.
func (a GitHubAccess) Person(ctx context.Context, p authn.Principal, repository string) (string, error) {
	if p.Sub == "" {
		return "", nil
	}
	login, err := a.Identity.Login(ctx, p.Sub, repository)
	if err != nil {
		return "", fmt.Errorf("person: %w", err)
	}
	if login == "" {
		return "", nil
	}
	return factoryv1.Person(login), nil
}
