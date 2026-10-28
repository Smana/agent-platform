// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/config"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/roomctrl"
	"github.com/Smana/agent-platform/internal/runrequest"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/summary"
	"github.com/Smana/agent-platform/internal/version"
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
		s.log().Warn("room not created", "room", room.Name, "repository", in.Repository, "err", err)
		http.Error(w, "could not create the room", createStatus(err))
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]string{"id": room.Name})
}

// createStatus maps a Room Create error to a status that says whether a retry can help (#146):
// 503 only when the API server is unreachable or overloaded. Forbidden is the broker's own
// grant missing, so it is a 500 like any other error a retry will not fix.
func createStatus(err error) int {
	var netErr net.Error
	switch {
	case apierrors.IsInvalid(err):
		return http.StatusBadRequest
	case apierrors.IsAlreadyExists(err):
		return http.StatusConflict
	case apierrors.IsServerTimeout(err), apierrors.IsTimeout(err), apierrors.IsTooManyRequests(err),
		apierrors.IsServiceUnavailable(err), errors.As(err, &netErr), errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// roomctlSetup is GET /api/roomctl: the values of `roomctl configure` and the broker's
// version, whose roomctl release the UI's CLI setup view installs. clientID is "" while the broker has no roomctl client.
// roomctl asks for the project's audience with projectID: ZITADEL adds the
// project to aud only then, and the broker refuses a token without it (ruling AS).
// It also carries the room page's dashboard templates, unfilled: not secrets, but of no use to
// roomctl configure, so the unauthenticated route leaves them out.
func (s *Server) roomctlSetup(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.principal(w, r); !ok {
		return
	}
	s.writeSetup(w, s.Dashboards)
}

// publicSetup is GET /.well-known/roomctl: the same values with no sign-in, so `roomctl
// configure --url` needs nothing else (#48). The issuer, the client id and the project id are
// public by design (a native app has no secret) and the CLI setup view shows them to any member.
// The proxy in front must let this path through unauthenticated.
func (s *Server) publicSetup(w http.ResponseWriter, _ *http.Request) { s.writeSetup(w, nil) }

// Dashboard is one link template the room page fills with a room's ids (human.dashboards):
// {room}, {run} and {task}, each URL-encoded, and a link whose value is missing hidden.
type Dashboard struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

func (s *Server) writeSetup(w http.ResponseWriter, dashboards []Dashboard) {
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
		Version   string `json:"version"`
		// HumanMode says whether roomctl needs an IdP and a token. Additive: a broker without the
		// field, and a roomctl that ignores it, read each other as before (OIDC).
		HumanMode string `json:"humanMode"`
		// Dashboards are the templates, absent when none is configured.
		Dashboards []Dashboard `json:"dashboards,omitempty"`
	}{s.PublicURL, s.Issuer, read(s.RoomctlClient), read(s.ProjectID), version.Version, cmp.Or(s.HumanMode, config.HumanModeOIDC), dashboards})
}

// roomRow is one row of GET /api/rooms.
type roomRow struct {
	ID    string `json:"id"`
	URL   string `json:"url"` // the room's page
	Phase string `json:"phase"`
	// State is the coarse state (summary.Coarse) of the task's phase, else the room's.
	State     string `json:"state,omitempty"`
	Owner     string `json:"owner"`
	Driver    string `json:"driver"`
	DataClass string `json:"dataClass"`
	LastSeq   int64  `json:"lastSeq"`
	// LastEventAt is when the room's log last moved; absent when the room has no events or it could not be read.
	LastEventAt time.Time `json:"lastEventAt,omitzero"`
	Repository  string    `json:"repository"`
	NeedsMe     bool      `json:"needsMe"`
	// The task the room works on (#50): Phase above is the room's, Task the task's.
	Issue int    `json:"issue,omitempty"`
	Title string `json:"title,omitempty"`
	Task  string `json:"task,omitempty"`
	PR    int    `json:"pr,omitempty"`
	// Reason is why the task waits, from its last facts: only while it waits, as the summary's.
	Reason   string `json:"reason,omitempty"`
	NeedsYou int    `json:"needsYou"` // how many things wait for this caller
	// Need is the first of them, in the summary's order: absent when none waits.
	Need *summary.Need `json:"need,omitempty"`
	// Step is the one thing to do for the room and where: approve on its page, or merge or close
	// the PR on GitHub. Absent when nothing waits for a human.
	Step *summary.Step `json:"step,omitempty"`
	You  wire.You      `json:"you"`
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

// factsFn is a room's last task facts, read at most once however many callers ask: the hold, the
// answer, the wait reason and the first need all want them, and a row that needs none reads none.
type factsFn func() *envelope.TaskFacts

// taskFacts is room's last task facts: one indexed read, which carries what the Room status does
// not project (the wait reason). The returned func reads on its first call; its value is nil when
// the facts cannot be read: the room's summary still says it.
func (s *Server) taskFacts(ctx context.Context, room *v1alpha1.Room) factsFn {
	var once sync.Once
	var facts *envelope.TaskFacts
	return func() *envelope.TaskFacts {
		once.Do(func() {
			raw, err := s.Log.LastTaskState(ctx, room.Name, roomctrl.ForkSeq(room))
			if err != nil {
				s.log().Warn("room row: task facts unread", "room", room.Name, "err", err)
				return
			}
			var f envelope.TaskFacts
			if raw != nil && json.Unmarshal(raw, &f) == nil {
				facts = &f
			}
		})
		return facts
	}
}

// held reports the summary's hold need for sub (N3, #78). The projected task phase screens out
// every room but a Queued one sub could release, so only those read the room's last task facts.
// An unreadable row is no need: the room's summary still says it.
func held(room *v1alpha1.Room, sub policy.Subject, url string, facts factsFn) (summary.Need, bool) {
	if t := room.Status.Task; t == nil || t.Phase != "Queued" || !policy.Allowed(sub, policy.StartRun) {
		return summary.Need{}, false
	}
	return summary.Hold(facts(), sub, url)
}

// answer is the summary's answer need for sub: the facts of an Escalated room wait on the issue's
// author, and the room's owner sees it. Only an Escalated room reads its facts for it.
func answer(room *v1alpha1.Room, sub policy.Subject, facts factsFn) (summary.Need, bool) {
	if t := room.Status.Task; t == nil || t.Phase != "Escalated" || !policy.Allowed(sub, policy.Close) {
		return summary.Need{}, false
	}
	return summary.Answer(facts(), sub)
}

// rowStep is the row's step for the caller: a merge or an answer needs the right to close, as
// the summary's needs do; a sealed room has none.
func rowStep(room *v1alpha1.Room, sub policy.Subject, row roomRow, repo string, approvals bool) *summary.Step {
	in := summary.StepIn{Phase: row.Task, Reason: row.Reason, PR: row.PR, PRURL: prURLOf(repo, row.PR), Issue: row.Issue,
		Approvals: approvals, CanClose: policy.Allowed(sub, policy.Close), RoomURL: row.URL,
		Sealed: room.Status.Phase == "Closed" || room.Status.Phase == "Sealed"}
	if repo != "" && row.Issue > 0 {
		in.IssueURL = fmt.Sprintf("https://github.com/%s/issues/%d", repo, row.Issue)
	}
	return summary.StepFor(in)
}

// prURLOf is the pull request's URL in repo, "" when either is unknown.
func prURLOf(repo string, n int) string {
	if repo == "" || n <= 0 {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", repo, n)
}

// prNeed is the summary's merge need for sub, from the projected task: the Room status holds the
// PR's number, not its facts, so the URL is built from the room's repository.
func prNeed(room *v1alpha1.Room, sub policy.Subject) (summary.Need, bool) {
	t := room.Status.Task
	if t == nil || t.PR <= 0 {
		return summary.Need{}, false
	}
	pr := &envelope.PRFact{Number: int(t.PR), URL: prURLOf(room.Spec.Repository, int(t.PR))}
	return summary.Merge(&envelope.TaskFacts{Phase: t.Phase, PR: pr}, sub)
}

// liveApprovals are the approvals of room that sub could decide in the web UI and whose deadline
// has not passed: a lapsed one the sweep's approval_decided{expired} has not landed on is no need,
// as the summary drops it. The table is read only for a room with one pending that web could decide.
// expired is how many it dropped: the room's own count, which the sweep has not caught up with,
// less those is what waits. An unread table drops none.
func (s *Server) liveApprovals(ctx context.Context, room *v1alpha1.Room, web policy.Subject) (live []store.Approval, expired int) {
	if room.Status.PendingApprovals <= 0 || !policy.Allowed(web, policy.Decide) {
		return nil, 0
	}
	open, err := s.Log.OpenApprovals(ctx, room.Name)
	if err != nil {
		s.log().Warn("room row: approvals unread", "room", room.Name, "err", err)
		return nil, 0
	}
	now := s.now()
	for _, a := range open {
		if !a.ExpiresAt.IsZero() && now.After(a.ExpiresAt) {
			expired++
			continue
		}
		live = append(live, a)
	}
	return live, expired
}

// needs counts what waits for sub in room, as the room's summary lists it: approvals sub could
// decide in the web UI (web is sub as that UI), a hold sub could release, a merge sub owns, an
// answer the issue's author owes.
func needs(room *v1alpha1.Room, sub, web policy.Subject, expired int, facts factsFn) int {
	n := 0
	if policy.Allowed(web, policy.Decide) {
		n = max(0, int(room.Status.PendingApprovals)-expired)
	}
	if _, ok := held(room, sub, "", facts); ok {
		n++
	}
	if _, ok := prNeed(room, sub); ok {
		n++
	}
	if _, ok := answer(room, sub, facts); ok {
		n++
	}
	return n
}

// firstNeed is the first thing that waits for the caller, in the summary's order: an approval
// (the lowest id, ULIDs sorting by time, as the summary's sorted keys), a hold, a merge, an answer.
// nil when nothing waits.
func firstNeed(room *v1alpha1.Room, sub policy.Subject, url string, live []store.Approval, facts factsFn) *summary.Need {
	var first *store.Approval
	for i := range live {
		if first == nil || live[i].ID < first.ID {
			first = &live[i]
		}
	}
	if first != nil {
		return &summary.Need{Kind: "approval", ID: first.ID, What: summary.DescribeApproval(first.Class, first.Action),
			Deadline: first.ExpiresAt, URL: url + "#" + first.ID}
	}
	if n, ok := held(room, sub, url, facts); ok {
		return &n
	}
	if n, ok := prNeed(room, sub); ok {
		return &n
	}
	if n, ok := answer(room, sub, facts); ok {
		return &n
	}
	return nil
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
	q := r.URL.Query()
	rows, access, err := s.roomRows(ctx, p, &roomFilter{repo: q.Get("repo"), mine: q.Get("mine") == "1", needsMe: q.Get("needs_me") == "1"})
	if err != nil {
		http.Error(w, err.Error(), roomStatus(err))
		return
	}
	w.Header().Set(accessHeader, access)
	writeJSON(w, rows)
}

// roomRows is the room list p reads under f, and the X-Rooms-Access value that says why it may be
// short. Its one error is errRoomsUnavailable.
func (s *Server) roomRows(ctx context.Context, p authn.Principal, f *roomFilter) ([]roomRow, string, error) {
	var rooms v1alpha1.RoomList
	if err := s.Rooms.List(ctx, &rooms, client.InNamespace(s.Namespace)); err != nil {
		return nil, "", errRoomsUnavailable
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
		if f.mine && !f.isMine(ctx, s, p, room) {
			continue
		}
		// After mine: a row filtered out never costs held's store read.
		facts := s.taskFacts(ctx, room) // one read serves the count, the reason and the need
		live, expired := s.liveApprovals(ctx, room, web)
		count := needs(room, sub, web, expired, facts)
		if f.needsMe && count == 0 {
			continue
		}
		row := roomRow{ID: room.Name, URL: s.PublicURL + "/r/" + room.Name, Phase: room.Status.Phase, Owner: room.Spec.Owner,
			Driver: room.Status.Driver, DataClass: room.Spec.DataClass, LastSeq: room.Status.LastSeq, Repository: repo,
			NeedsMe: count > 0, NeedsYou: count, You: you}
		if t := room.Status.Task; t != nil {
			row.Task, row.Issue, row.Title, row.PR = t.Phase, int(t.Issue), t.IssueTitle, int(t.PR)
		}
		row.State = summary.Coarse(row.Task, row.Phase, s.nextRun(room.Name))
		if waits[row.Task] {
			row.Reason = waitReason(facts)
		}
		row.Need = firstNeed(room, sub, row.URL, live, facts)
		row.Step = rowStep(room, sub, row, repo, len(live) > 0)
		out = append(out, row)
	}
	s.stampLastEvent(ctx, out)
	return out, access, nil
}

// waits are the task phases whose rows carry a reason, as the summary's status does.
var waits = map[string]bool{"Queued": true, "AwaitingCI": true, "AwaitingHuman": true, "Escalated": true}

// maxRowReason bounds a row's reason, in runes: the factory writes a code.
const maxRowReason = 64

// waitReason is the reason in room's last task facts, "" when they cannot be read.
func waitReason(facts factsFn) string {
	f := facts()
	if f == nil {
		return ""
	}
	if r := []rune(f.Reason); len(r) > maxRowReason {
		return string(r[:maxRowReason])
	}
	return f.Reason
}

// nextRun reports a live run in room that is not running yet: requested, pending or suspended.
func (s *Server) nextRun(room string) bool {
	for _, r := range s.Runs.InRoom(room) {
		if r.Live() && r.Phase != "Running" {
			return true
		}
	}
	return false
}

// stampLastEvent sets each row's LastEventAt from one store read for every listed room. The list
// does not fail on it: an unread time leaves the field zero.
func (s *Server) stampLastEvent(ctx context.Context, rows []roomRow) {
	if len(rows) == 0 {
		return
	}
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	at, err := s.Log.LastEventAt(ctx, ids)
	if err != nil {
		s.log().Warn("room list: last event unread", "err", err)
		return
	}
	for i := range rows {
		rows[i].LastEventAt = at[rows[i].ID]
	}
}
