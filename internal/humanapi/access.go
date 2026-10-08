// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"time"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
)

// accessWait bounds one request's GitHub checks inside routeTimeout, so a slow GitHub still
// leaves time to write the refusal.
const accessWait = 10 * time.Second

// reasonAccessUnverified is the 503 when ZITADEL or GitHub cannot answer past the cache. It never
// names the repository: the caller may not be allowed to know which one the room is on.
const reasonAccessUnverified = "access_unverified"

// admits applies D7 before any standing rule: an admin sees every room; anyone else sees a room
// only if they can read its repository on GitHub. A room without a repository is admins-only, as
// is every room while the GitHub check is not wired. An error means the check could not be made,
// and the caller fails closed.
func (s *Server) admits(ctx context.Context, room *v1alpha1.Room, p authn.Principal) (bool, error) {
	if s.Groups.IsAdmin(p) {
		return true, nil
	}
	if room.Spec.Repository == "" || s.Access == nil || s.Identity == nil {
		return false, nil
	}
	login, err := s.Identity.Login(ctx, p.Sub, room.Spec.Repository)
	if err != nil || login == "" {
		return false, err
	}
	return s.Access.CanRead(ctx, room.Spec.Repository, login)
}
