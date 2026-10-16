// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

const (
	repoA = "Smana/cloud-native-ref"
	repoB = "Smana/ai-native-ref"
)

// writers is D4's GitHub answer by principal, then repository; err answers every lookup.
type writers struct {
	can map[string][]string
	err error
}

func (w writers) CanWrite(_ context.Context, p authn.Principal, repository string) (bool, error) {
	if w.err != nil {
		return false, w.err
	}
	for _, r := range w.can[p.ID] {
		if strings.EqualFold(r, repository) {
			return true, nil
		}
	}
	return false, nil
}

// twoRepos is a server listing repoA and repoB, alice a writer on both unless w says otherwise.
func twoRepos(t *testing.T, st *store, w *writers, objs ...client.Object) http.Handler {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(objs...).Build()
	s := mkServer(t, st, c, false, false, "7f3cq2xz", 5_000_000)
	s.Cfg.API.Repositories = []string{repoA, repoB}
	if w == nil {
		w = &writers{can: map[string][]string{"human:291": {repoA, repoB}, "human:1": {repoA, repoB}}}
	}
	s.Access = *w
	return s.Handler()
}

func roomOn(repo string) *v1alpha1.Room {
	r := aliceRoom()
	r.Spec.Repository = repo
	return r
}

func on(repo string, edit func(map[string]any)) map[string]any {
	b := base()
	b["repository"] = repo
	if edit != nil {
		edit(b)
	}
	return b
}

func refusedWith(t *testing.T, w interface {
	Result() *http.Response
}, code int, reason string, st *store) {
	t.Helper()
	res := w.Result()
	defer func() { _ = res.Body.Close() }()
	var sb strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := res.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if res.StatusCode != code || !strings.Contains(sb.String(), reason) || len(st.created) != 0 {
		t.Fatalf("want %d %s and no run, got %d %s, %d created", code, reason, res.StatusCode, sb.String(), len(st.created))
	}
}

// D3: a room's repository wins. A driver of a room on A cannot start a run on B with A's brief,
// and a fork, which copies its source room's repository, is bound the same way.
func TestARoomOfOneRepositoryStartsNoRunOnAnother(t *testing.T) {
	withRoom := func(b map[string]any) { b["roomRef"] = "3kq7x2ma" }
	for name, tc := range map[string]struct {
		room *v1alpha1.Room
		body map[string]any
		code int
		why  string
	}{
		"a room on A, a run on B":           {roomOn(repoA), on(repoB, withRoom), http.StatusBadRequest, "repository_differs_from_the_room"},
		"a fork of a room on B, a run on A": {roomOn(repoB), on(repoA, withRoom), http.StatusBadRequest, "repository_differs_from_the_room"},
		"a room with no repository":         {roomOn(""), on(repoA, withRoom), http.StatusBadRequest, "repository_differs_from_the_room"},
		"a room on A, a run on A":           {roomOn(repoA), on(repoA, withRoom), http.StatusCreated, ""},
		"GitHub's case-insensitive name":    {roomOn("smana/AI-native-ref"), on(repoB, withRoom), http.StatusCreated, ""},
	} {
		t.Run(name, func(t *testing.T) {
			st := &store{}
			w := post(twoRepos(t, st, nil, tc.room), "alice", tc.body)
			if tc.code != http.StatusCreated {
				refusedWith(t, w, tc.code, tc.why, st)
				return
			}
			if w.Code != tc.code || len(st.created) != 1 || st.created[0].Repository != tc.body["repository"] {
				t.Fatalf("%d %s %+v", w.Code, w.Body, st.created)
			}
		})
	}
}

// D3: a resumed run keeps its repository: the branch's last run, or its room, names it.
func TestAResumedRunKeepsItsRepository(t *testing.T) {
	resume := func(b map[string]any) { b["resumeBranch"] = "agent/aaaaaaaa" }
	stopped := func(repo string, at time.Time) runs.Run {
		return runs.Run{ID: "aaaaaaaa", Branch: "agent/aaaaaaaa", Repository: repo, Phase: "Failed", Principal: "human:291", Created: at}
	}
	roomAA := roomOn(repoA)
	roomAA.Name = "aaaaaaaa"
	for name, tc := range map[string]struct {
		existing []runs.Run
		objs     []client.Object
		repo     string
		code     int
	}{
		"its run was on A, the resume on B":        {[]runs.Run{stopped(repoA, now.Add(-time.Hour))}, nil, repoB, http.StatusBadRequest},
		"its last run was on A, an older one on B": {[]runs.Run{stopped(repoB, now.Add(-2*time.Hour)), stopped(repoA, now.Add(-time.Hour))}, nil, repoB, http.StatusBadRequest},
		"its room is on A, the resume on B":        {nil, []client.Object{roomAA}, repoB, http.StatusBadRequest},
		"its run was on A, the resume on A":        {[]runs.Run{stopped(repoA, now.Add(-time.Hour))}, nil, repoA, http.StatusCreated},
		"nothing names it: the D4 check stands":    {nil, nil, repoB, http.StatusCreated},
		"its room on A, its run on A, resume on A": {[]runs.Run{stopped(repoA, now.Add(-time.Hour))}, []client.Object{roomAA}, repoA, http.StatusCreated},
	} {
		t.Run(name, func(t *testing.T) {
			st := &store{existing: tc.existing}
			w := post(twoRepos(t, st, nil, tc.objs...), "alice", on(tc.repo, resume))
			if tc.code != http.StatusCreated {
				refusedWith(t, w, tc.code, "repository_differs_from_the_branch", st)
				return
			}
			if w.Code != tc.code || st.created[0].Repository != tc.repo {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
}

// D4: write on the repository, from GitHub, for every human admission — room or not, resumed or
// not, admins included — and a lookup that fails refuses.
func TestEveryHumanRunNeedsWriteOnItsRepository(t *testing.T) {
	readOnly := &writers{can: map[string][]string{"human:291": {repoA}, "human:1": {repoA}}}
	broken := &writers{err: errors.New("github down")}
	for name, tc := range map[string]struct {
		w    *writers
		tok  string
		body map[string]any
		objs []client.Object
	}{
		"roomless, read only on B":           {readOnly, "alice", on(repoB, nil), nil},
		"an admin gets no bypass":            {readOnly, "admin", on(repoB, nil), nil},
		"a room on B, read only on B":        {readOnly, "alice", on(repoB, func(b map[string]any) { b["roomRef"] = "3kq7x2ma" }), []client.Object{roomOn(repoB)}},
		"a resume on B, read only on B":      {readOnly, "alice", on(repoB, func(b map[string]any) { b["resumeBranch"] = "agent/aaaaaaaa" }), nil},
		"the lookup fails":                   {broken, "alice", on(repoA, nil), nil},
		"no GitHub link, no write (not set)": {&writers{}, "alice", on(repoA, nil), nil},
	} {
		t.Run(name, func(t *testing.T) {
			st := &store{}
			refusedWith(t, post(twoRepos(t, st, tc.w, tc.objs...), "alice"[:0]+tc.tok, tc.body), http.StatusForbidden, "repository_permission", st)
		})
	}
	st := &store{}
	if w := post(twoRepos(t, st, readOnly), "alice", on(repoA, nil)); w.Code != http.StatusCreated {
		t.Fatalf("write on A admits: %d %s", w.Code, w.Body)
	}
}

// D4 fails closed: a server built without the GitHub check refuses every human.
func TestNoPermissionCheckRefusesHumans(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme()).Build()
	s := mkServer(t, &store{}, c, false, false, "7f3cq2xz", 5_000_000)
	s.Access = nil
	st := &store{}
	s.Runs = st
	refusedWith(t, post(s.Handler(), "alice", base()), http.StatusForbidden, "repository_permission", st)
}

// D6: a system caller starts runs only on the repositories its config entry lists.
func TestSystemCallersAreScopedPerRepository(t *testing.T) {
	sys := authn.Principal{Kind: envelope.ActorSystem, ID: "system:runlore"}
	for name, tc := range map[string]struct {
		allowed map[string][]string
		repo    string
		code    int
	}{
		"listed for A, asks A":    {map[string][]string{"system:runlore": {repoA}}, repoA, http.StatusCreated},
		"listed for A, asks B":    {map[string][]string{"system:runlore": {repoA}}, repoB, http.StatusForbidden},
		"no entry: no repository": {nil, repoA, http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			st := &store{}
			c := fake.NewClientBuilder().WithScheme(scheme()).Build()
			s := mkServer(t, st, c, false, false, "7f3cq2xz", 5_000_000)
			s.Cfg.API.Repositories = []string{repoA, repoB}
			s.Cfg.API.SystemRepositories = tc.allowed
			s.Auth = byToken{"sys": sys}
			w := post(s.Handler(), "sys", on(tc.repo, nil))
			if tc.code != http.StatusCreated {
				refusedWith(t, w, tc.code, "repository_permission", st)
				return
			}
			if w.Code != tc.code {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
}

var _ = runtime.NewScheme

// spec.repository becomes the run's octo-sts audience: only the config's spelling is admitted.
func TestTheRepositoryIsSpelledAsListed(t *testing.T) {
	st := &store{}
	refusedWith(t, post(twoRepos(t, st, nil), "alice", on("smana/AI-native-ref", nil)), http.StatusForbidden, "repository_not_allowed", st)
}

// v2 D7: a repository whose fence file does not name this factory starts no API run, whoever asks.
func TestARepositoryNotOwnedStartsNoRun(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme()).Build()
	st := &store{}
	s := mkServer(t, st, c, false, false, "7f3cq2xz", 5_000_000)
	s.Cfg.API.Repositories = []string{repoA, repoB}
	s.Access = writers{can: map[string][]string{"human:291": {repoA, repoB}}}
	s.Owned = func(r string) bool { return r == repoA }
	refusedWith(t, post(s.Handler(), "alice", on(repoB, nil)), http.StatusConflict, "repository_not_owned", st)
	if w := post(s.Handler(), "alice", on(repoA, nil)); w.Code != http.StatusCreated {
		t.Fatalf("an owned repository admits: %d %s", w.Code, w.Body)
	}
	s.Owned = nil // fail closed: no fence, no repository
	st.created = nil
	refusedWith(t, post(s.Handler(), "alice", on(repoA, nil)), http.StatusConflict, "repository_not_owned", st)
}
