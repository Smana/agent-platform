// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/runrequest"
	"github.com/Smana/agent-platform/internal/wire"
)

func roleName(r policy.Role) string {
	switch r {
	case policy.Watcher:
		return "watcher"
	case policy.Collaborator:
		return "collaborator"
	case policy.Owner:
		return "owner"
	}
	return "none"
}

// you resolves p's standing in room. Only the web client is the web UI; an
// unreadable client id ("") makes nobody one.
func (s *Server) you(room *v1alpha1.Room, p authn.Principal, driver string) (policy.Subject, wire.You) {
	web := s.WebClient()
	sub := s.Groups.Resolve(room, p, driver, web != "" && p.ClientID == web)
	return sub, wire.You{Principal: p.ID, Role: roleName(sub.Role), Approver: sub.Approver, Driver: sub.Driver, WebUI: sub.WebUI}
}

// repositoryRE is the Room CRD's repository pattern.
var repositoryRE = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

// createRoom is POST /api/rooms: an agents member creates a room, owned and
// driven by them (§1 Groups), on a repository they can read (D7). A
// state-changing POST on a cookie session: oauth2-proxy's SameSite=Strict
// cookie and the Origin check (T9) stop a cross-site form posting it.
func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	var in struct {
		DataClass  string `json:"dataClass"`
		Repository string `json:"repository"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil || (in.DataClass != "public" && in.DataClass != "internal") ||
		!repositoryRE.MatchString(in.Repository) || !memberPrincipal.MatchString(p.ID) {
		http.Error(w, "dataClass is public or internal; repository is owner/name", http.StatusBadRequest)
		return
	}
	if s.Actor == nil || s.Actor.Rooms == nil {
		http.Error(w, "rooms cannot be created here", http.StatusServiceUnavailable)
		return
	}
	// Each room is a Room CR and a database row: the same per-human budget as
	// acts (§4, review 4.4 M4).
	if s.Actor.limited(p.ID) {
		if s.Actor.OnReject != nil {
			s.Actor.OnReject(r.Context(), rejectRateLimited)
		}
		http.Error(w, rejectRateLimited, http.StatusTooManyRequests)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), routeTimeout)
	defer cancel()
	// No retention: the CRD's default applies (OD-17).
	room := &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: runrequest.NewID(), Namespace: s.Namespace},
		Spec: v1alpha1.RoomSpec{Owner: p.ID, Driver: p.ID, DataClass: in.DataClass, Repository: in.Repository}}
	actx, acancel := context.WithTimeout(ctx, accessWait)
	ok, err := s.admits(actx, room, p)
	acancel()
	if err != nil {
		s.log().Warn("room access unverified", "repository", in.Repository, "err", err)
		http.Error(w, reasonAccessUnverified, http.StatusServiceUnavailable)
		return
	}
	if !ok {
		http.Error(w, "no such repository", http.StatusNotFound)
		return
	}
	if err := s.Actor.Rooms.Create(ctx, room); err != nil {
		code := http.StatusServiceUnavailable
		if apierrors.IsInvalid(err) {
			code = http.StatusBadRequest
		}
		http.Error(w, "could not create the room", code)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]string{"id": room.Name})
}

// roomctlSetup is GET /api/roomctl: the values of `roomctl configure`, for the
// UI's CLI setup view. clientID is "" while the broker has no roomctl client.
// roomctl asks for the project's audience with projectID: ZITADEL adds the
// project to aud only then, and the broker refuses a token without it (ruling AS).
func (s *Server) roomctlSetup(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.principal(w, r); !ok {
		return
	}
	read := func(f func() string) string {
		if f == nil {
			return ""
		}
		return f()
	}
	writeJSON(w, struct {
		URL       string `json:"url"`
		Issuer    string `json:"issuer"`
		ClientID  string `json:"clientID"`
		ProjectID string `json:"projectID"`
	}{s.PublicURL, s.Issuer, read(s.RoomctlClient), read(s.ProjectID)})
}

// roomRow is one row of GET /api/rooms.
type roomRow struct {
	ID         string   `json:"id"`
	Phase      string   `json:"phase"`
	Owner      string   `json:"owner"`
	Driver     string   `json:"driver"`
	DataClass  string   `json:"dataClass"`
	LastSeq    int64    `json:"lastSeq"`
	Repository string   `json:"repository"`
	NeedsMe    bool     `json:"needsMe"`
	You        wire.You `json:"you"`
}

// roomFilter is the query of GET /api/rooms. The caller's GitHub login is read at most once per
// request once it succeeds, on the first room that needs it. A failure is retried on a later
// room (the App may see one repository and not another), at most maxLoginTries times, so a
// broken GitHub never costs a call per room.
type roomFilter struct {
	repo          string
	mine, needsMe bool
	login         string
	resolved      bool
	tries         int
}

const maxLoginTries = 3

// isMine reports whether the caller's linked GitHub login is on the room's task. Without a
// login, or when ZITADEL cannot say, nothing is theirs.
func (f *roomFilter) isMine(ctx context.Context, s *Server, p authn.Principal, room *v1alpha1.Room) bool {
	if !f.resolved && s.Identity != nil && room.Spec.Repository != "" && f.tries < maxLoginTries {
		f.tries++
		login, err := s.Identity.Login(ctx, p.Sub, room.Spec.Repository)
		if err != nil {
			s.log().Warn("mine filter: login unresolved", "err", err)
		} else {
			f.login, f.resolved = login, true
		}
	}
	t := room.Status.Task
	if f.login == "" || t == nil {
		return false
	}
	return slices.ContainsFunc(append([]string{t.IssueAuthor, t.LabelledBy, t.PRAuthor}, t.PRReviewers...),
		func(u string) bool { return strings.EqualFold(u, f.login) })
}

// listRooms is GET /api/rooms: every room of the namespace the caller may read
// (D7, then §1 Groups), from the Room CRs' projected status. A room whose access
// cannot be verified is left out like an unreadable one: neither the count nor
// an error may tell that it exists. X-Rooms-Access says why, about the caller.
func (s *Server) listRooms(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), routeTimeout)
	defer cancel()
	var rooms v1alpha1.RoomList
	if err := s.Rooms.List(ctx, &rooms, client.InNamespace(s.Namespace)); err != nil {
		http.Error(w, "rooms unavailable", http.StatusServiceUnavailable)
		return
	}
	actx, acancel := context.WithTimeout(ctx, accessWait)
	defer acancel()
	access := accessOK
	if !s.Groups.IsAdmin(p) && (s.Access == nil || s.Identity == nil) {
		access = accessUnverified
	}
	// The gate depends on the repository alone, and a failed check is not cached: once per
	// repository, so rooms sharing an unreachable one cost one timeout, not one each.
	readable := map[string]bool{}
	q := r.URL.Query()
	f := &roomFilter{repo: q.Get("repo"), mine: q.Get("mine") == "1", needsMe: q.Get("needs_me") == "1"}
	out := []roomRow{}
	for i := range rooms.Items {
		room := &rooms.Items[i]
		repo := room.Spec.Repository
		ok, seen := readable[repo]
		if !seen {
			v, err := s.gate(actx, room, p)
			switch {
			case v == unlinked:
				access = accessUnlinked // the hint that helps: no check can pass until they link
			case err != nil:
				s.log().Warn("room access unverified", "repository", repo, "err", err)
				if access == accessOK {
					access = accessUnverified
				}
			}
			ok = v == admitted
			readable[repo] = ok
		}
		if !ok {
			continue
		}
		sub, you := s.you(room, p, room.Status.Driver)
		if !policy.Allowed(sub, policy.Read) {
			continue
		}
		// Filters run after the gate and the Read check, so one never reveals an unreadable room.
		if f.repo != "" && !strings.EqualFold(repo, f.repo) {
			continue
		}
		// Deciding happens in the web UI: judge it as the web UI would, as the summary's needsYou does.
		web := sub
		web.WebUI = true
		needsMe := room.Status.PendingApprovals > 0 && policy.Allowed(web, policy.Decide)
		if f.needsMe && !needsMe {
			continue
		}
		if f.mine && !f.isMine(ctx, s, p, room) {
			continue
		}
		out = append(out, roomRow{ID: room.Name, Phase: room.Status.Phase, Owner: room.Spec.Owner, Driver: room.Status.Driver,
			DataClass: room.Spec.DataClass, LastSeq: room.Status.LastSeq, Repository: repo, NeedsMe: needsMe, You: you})
	}
	w.Header().Set(accessHeader, access)
	writeJSON(w, out)
}
