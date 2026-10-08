// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/fanout"
	"github.com/Smana/agent-platform/internal/ghidentity"
	"github.com/Smana/agent-platform/internal/repoaccess"
)

// gitHubWorld is the D7 gate the tests run under: each principal's Sub is linked to the GitHub
// login of the same name, except the unlinked ones, and reads lists the logins that may read
// each repository; a nil reads lets every login read everything. down fails ZITADEL and GitHub
// alike, as an outage past the cache would.
type gitHubWorld struct {
	mu       sync.Mutex
	reads    map[string][]string
	unlinked map[string]bool
	down     bool
	now      time.Time
	ids      []string // a linked Sub's GitHub id is its index + 1
	links    int      // ZITADEL link reads
}

func world(reads map[string][]string, unlinked ...string) *gitHubWorld {
	w := &gitHubWorld{reads: reads, unlinked: map[string]bool{}, now: time.Now()}
	for _, u := range unlinked {
		w.unlinked[u] = true
	}
	return w
}

func (w *gitHubWorld) clock() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now
}

func (w *gitHubWorld) set(fn func()) { w.mu.Lock(); defer w.mu.Unlock(); fn() }

// gate wires the world into s as the broker wires ZITADEL and GitHub.
func (w *gitHubWorld) gate(s *Server) {
	s.Identity = &ghidentity.Resolver{IDPID: "github-idp", TTL: 5 * time.Minute, Now: w.clock,
		Links: func(_ context.Context, sub string) ([]ghidentity.Link, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.links++
			if w.down {
				return nil, errors.New("zitadel down")
			}
			if w.unlinked[sub] {
				return []ghidentity.Link{{IDPID: "google-idp", UserID: "g-" + sub}}, nil
			}
			i := slices.Index(w.ids, sub)
			if i < 0 {
				w.ids, i = append(w.ids, sub), len(w.ids)
			}
			return []ghidentity.Link{{IDPID: "github-idp", UserID: strconv.Itoa(i + 1)}}, nil
		},
		LoginOf: func(_ context.Context, _ string, id int64) (string, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.down {
				return "", errors.New("github down")
			}
			return w.ids[id-1], nil
		}}
	s.Access = &repoaccess.Checker{TTL: 5 * time.Minute, Now: w.clock,
		Perm: func(_ context.Context, owner, repo, login string) (string, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.down {
				return "", errors.New("github down")
			}
			if w.reads == nil || slices.Contains(w.reads[owner+"/"+repo], login) {
				return "read", nil
			}
			return "none", nil
		}}
}

func (w *gitHubWorld) option() option {
	return func(s *Server, _ *fanout.Hub, _ *hubView) { w.gate(s) }
}

// The rooms of the access tests: A on Smana/a (roomID, so the log serves it), B on Smana/b,
// and C, created before rooms carried a repository.
const (
	roomB = "4kq7x2mb"
	roomC = "5kq7x2mc"
)

func repoRoom(id, repo string) *v1alpha1.Room {
	return &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: namespace},
		Spec:   v1alpha1.RoomSpec{Owner: "human:own", Driver: "system:factory", DataClass: "public", Repository: repo},
		Status: v1alpha1.RoomStatus{Phase: "Active", Driver: "system:factory"}}
}

func roomClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// abc serves rooms A, B and C.
func abc(t *testing.T) option {
	return func(s *Server, _ *fanout.Hub, _ *hubView) {
		s.Rooms = roomClient(t, repoRoom(roomID, "Smana/a"), repoRoom(roomB, "Smana/b"), repoRoom(roomC, ""))
	}
}

// get is a plain GET as user, with extra header pairs; it returns the status and the body.
func get(t *testing.T, e env, path string, h http.Header) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = h
	r, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

// listed is the ids GET /api/rooms shows h.
func listed(t *testing.T, e env, h http.Header) []string {
	t.Helper()
	code, body := get(t, e, "/api/rooms", h)
	var rows []roomRow
	if err := json.Unmarshal([]byte(body), &rows); err != nil || code != http.StatusOK {
		t.Fatalf("GET /api/rooms: %d %s", code, body)
	}
	ids := []string{}
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

// opens reports the status of a WebSocket upgrade to room, and its plain-text refusal; the
// handler refuses before the upgrade, so a plain GET carries the same answer.
func opens(t *testing.T, e env, room string, h http.Header) (int, string) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(e.ts.URL, "http") + "/v1/ws?room=" + room
	if _, code, err := connectTo(t, u, h); err == nil {
		return http.StatusSwitchingProtocols, ""
	} else if code == 0 {
		t.Fatalf("dial %s: %v", room, err)
	}
	return get(t, e, "/v1/ws?room="+room, h)
}

func admin(user string) http.Header { return header(user, "X-Test-Admin", "1") }

// D7: a member sees a room only if GitHub lets them read its repository; an admin sees all.
func TestRoomVisibilityFollowsGitHub(t *testing.T) {
	w := world(map[string][]string{"Smana/a": {"dev1"}}, "nolink")
	e := setup(t, abc(t), w.option())
	_, missing := opens(t, e, "6kq7x2md", header("dev1"))
	for _, tc := range []struct {
		name   string
		h      http.Header
		listed []string
		open   map[string]bool
	}{
		{"a member who can read Smana/a", header("dev1"), []string{roomID}, map[string]bool{roomID: true, roomB: false, roomC: false}},
		{"a member who can read neither", header("dev2"), []string{}, map[string]bool{roomID: false, roomB: false, roomC: false}},
		{"a member with no GitHub link", header("nolink"), []string{}, map[string]bool{roomID: false, roomB: false, roomC: false}},
		{"an admin, with no GitHub link", admin("nolink"), []string{roomID, roomB, roomC}, map[string]bool{roomID: true, roomB: true, roomC: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := listed(t, e, tc.h); !slices.Equal(got, tc.listed) {
				t.Errorf("lists %v, want %v", got, tc.listed)
			}
			for room, ok := range tc.open {
				code, body := opens(t, e, room, tc.h)
				switch {
				case ok && code != http.StatusSwitchingProtocols:
					t.Errorf("%s: %d %q, want the upgrade", room, code, body)
				case !ok && (code != http.StatusNotFound || body != missing):
					t.Errorf("%s: %d %q, want a missing room's %q", room, code, body, missing)
				}
			}
		})
	}
	if missing != "no such room\n" {
		t.Fatalf("a missing room answers %q", missing)
	}
}

// With neither GitHub check wired, rooms are admins-only: fail closed, never open to all.
func TestNoAccessCheckIsAdminsOnly(t *testing.T) {
	e := setup(t, abc(t), func(s *Server, _ *fanout.Hub, _ *hubView) { s.Identity, s.Access = nil, nil })
	if got := listed(t, e, header("dev1")); len(got) != 0 {
		t.Fatalf("a member lists %v", got)
	}
	if code, _ := opens(t, e, roomID, header("dev1")); code != http.StatusNotFound {
		t.Fatalf("a member opens a room: %d", code)
	}
	if got := listed(t, e, admin("boss")); len(got) != 3 {
		t.Fatalf("an admin lists %v", got)
	}
	e = setup(t, abc(t), w0().option(), func(s *Server, _ *fanout.Hub, _ *hubView) { s.Access = nil })
	if got := listed(t, e, header("dev1")); len(got) != 0 {
		t.Fatalf("without the permission check a member lists %v", got)
	}
	e = setup(t, abc(t), w0().option(), func(s *Server, _ *fanout.Hub, _ *hubView) { s.Identity = nil })
	if got := listed(t, e, header("dev1")); len(got) != 0 {
		t.Fatalf("without the identity a member lists %v", got)
	}
}

// w0 lets every linked login read everything.
func w0() *gitHubWorld { return world(nil) }

// Past the cache, ZITADEL or GitHub failing hides the room: the list skips it without a trace,
// and the WebSocket says the access could not be verified, naming no repository.
func TestAccessFailsClosedPastTheCache(t *testing.T) {
	w := world(map[string][]string{"Smana/a": {"dev1"}})
	e := setup(t, abc(t), w.option())
	if got := listed(t, e, header("dev1")); !slices.Equal(got, []string{roomID}) {
		t.Fatalf("lists %v", got)
	}
	w.set(func() { w.down, w.now = true, w.now.Add(4*time.Minute) })
	if got := listed(t, e, header("dev1")); !slices.Equal(got, []string{roomID}) {
		t.Fatalf("a fresh cache must stand: %v", got)
	}
	if code, _ := opens(t, e, roomID, header("dev1")); code != http.StatusSwitchingProtocols {
		t.Fatalf("a fresh cache must stand: %d", code)
	}
	w.set(func() { w.now = w.now.Add(2 * time.Minute) })
	if code, body := get(t, e, "/api/rooms", header("dev1")); code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Fatalf("past the cache: %d %s", code, body)
	}
	if code, body := opens(t, e, roomID, header("dev1")); code != http.StatusServiceUnavailable || body != "access_unverified\n" {
		t.Fatalf("past the cache: %d %q", code, body)
	}
	if got := listed(t, e, admin("boss")); len(got) != 3 {
		t.Fatalf("an admin needs no GitHub: %v", got)
	}
	if code, _ := opens(t, e, roomB, admin("boss")); code != http.StatusSwitchingProtocols {
		t.Fatalf("an admin needs no GitHub: %d", code)
	}
}

// One list checks each repository once: rooms sharing an unverifiable one cost one try.
func TestAListChecksEachRepositoryOnce(t *testing.T) {
	w := world(nil)
	w.down = true
	e := setup(t, w.option(), func(s *Server, _ *fanout.Hub, _ *hubView) {
		s.Rooms = roomClient(t, repoRoom(roomID, "Smana/a"), repoRoom(roomB, "Smana/a"), repoRoom(roomC, "Smana/a"))
	})
	if got := listed(t, e, header("dev1")); len(got) != 0 {
		t.Fatalf("lists %v", got)
	}
	w.set(func() {
		if w.links != 1 {
			t.Fatalf("%d ZITADEL reads for one repository", w.links)
		}
	})
}

// POST /api/rooms takes a repository the caller can read; an unreadable one is as unknown as a
// missing room.
func TestCreateRoomNeedsAReadableRepository(t *testing.T) {
	w := world(map[string][]string{"Smana/a": {"dev1"}}, "nolink")
	srv := &Server{Humans: headerAuth{}, Groups: groups, Namespace: namespace, Actor: &Actor{Rooms: roomClient(t)}}
	w.gate(srv)
	post := func(h http.Header, body string) (int, string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/api/rooms", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header = h
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	for _, tc := range []struct {
		name string
		h    http.Header
		body string
		want int
	}{
		{"no repository", header("dev1"), `{"dataClass":"public"}`, http.StatusBadRequest},
		{"an empty repository", header("dev1"), `{"dataClass":"public","repository":""}`, http.StatusBadRequest},
		{"a repository the member reads", header("dev1"), `{"dataClass":"public","repository":"Smana/a"}`, http.StatusCreated},
		{"a repository the member cannot read", header("dev1"), `{"dataClass":"public","repository":"Smana/b"}`, http.StatusNotFound},
		{"a member with no GitHub link", header("nolink"), `{"dataClass":"public","repository":"Smana/a"}`, http.StatusNotFound},
		{"an admin, any repository", admin("boss"), `{"dataClass":"public","repository":"Smana/b"}`, http.StatusCreated},
		{"an admin still names one", admin("boss"), `{"dataClass":"public"}`, http.StatusBadRequest},
	} {
		if code, body := post(tc.h, tc.body); code != tc.want {
			t.Errorf("%s: %d %q, want %d", tc.name, code, body, tc.want)
		}
	}
	w.set(func() { w.down = true })
	if code, _ := post(header("dev2"), `{"dataClass":"public","repository":"Smana/a"}`); code != http.StatusServiceUnavailable {
		t.Fatalf("GitHub down: %d", code)
	}
	var made v1alpha1.RoomList
	if err := srv.Actor.Rooms.List(t.Context(), &made); err != nil || len(made.Items) != 2 {
		t.Fatalf("%d rooms made, %v", len(made.Items), err)
	}
	for _, r := range made.Items {
		if r.Spec.Repository == "" {
			t.Fatalf("a room without its repository: %+v", r.Spec)
		}
	}
}

// A fork is its parent's repository's: it is never more visible than the room it came from.
func TestAForkKeepsItsParentsRepository(t *testing.T) {
	a, _, room, _ := forkFixture(t, nil)
	room.Spec.Repository = "Smana/a"
	if s := forkedSpec(t, a, room, forkAs(a, room, "human:bob", false, Action{Seq: 2})); s.Repository != "Smana/a" {
		t.Fatalf("the fork's repository is %q", s.Repository)
	}
}
