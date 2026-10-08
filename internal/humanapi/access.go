// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"errors"
	"time"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/repoaccess"
)

// accessWait bounds one request's GitHub checks inside routeTimeout, so a slow GitHub still
// leaves time to write the refusal.
const accessWait = 10 * time.Second

// reasonAccessUnverified is the 503 when ZITADEL or GitHub cannot answer past the cache. It never
// names the repository: the caller may not be allowed to know which one the room is on.
const reasonAccessUnverified = "access_unverified"

// verdict is D7's answer for one caller and one room.
type verdict int

const (
	denied   verdict = iota // GitHub says no, or the room is admins-only
	admitted                // an admin, or a reader of the room's repository
	unlinked                // denied: the caller has no GitHub link to check
)

// admits applies D7 before any standing rule: an admin sees every room; anyone else sees a room
// only if they can read its repository on GitHub. A room without a repository is admins-only, as
// is every room while the GitHub check is not wired. An error means the check could not be made,
// and the caller fails closed. A repository the App cannot see is unreadable, not an error.
func (s *Server) admits(ctx context.Context, room *v1alpha1.Room, p authn.Principal) (bool, error) {
	v, err := s.gate(ctx, room, p)
	return v == admitted, err
}

// gate is admits, saying also when the caller is denied for having no GitHub link.
func (s *Server) gate(ctx context.Context, room *v1alpha1.Room, p authn.Principal) (verdict, error) {
	if s.Groups.IsAdmin(p) {
		return admitted, nil
	}
	if room.Spec.Repository == "" || s.Access == nil || s.Identity == nil {
		return denied, nil
	}
	login, err := s.Identity.Login(ctx, p.Sub, room.Spec.Repository)
	switch {
	case errors.Is(err, repoaccess.ErrNoRepository):
		return denied, nil
	case err != nil:
		return denied, err
	case login == "":
		return unlinked, nil
	}
	ok, err := s.Access.CanRead(ctx, room.Spec.Repository, login)
	if !ok {
		return denied, err
	}
	return admitted, err
}

// The X-Rooms-Access header of GET /api/rooms (ruling R21): why a list may be short, about the
// caller alone, so it tells no room's existence. roomctl turns it into a hint.
const (
	accessHeader     = "X-Rooms-Access"
	accessOK         = "ok"
	accessUnlinked   = "unlinked"   // not an admin, and no GitHub link on the ZITADEL user
	accessUnverified = "unverified" // a check failed past the cache, or none is wired
)
