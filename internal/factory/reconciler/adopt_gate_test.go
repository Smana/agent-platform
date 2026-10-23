// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/rooms"
)

const (
	adoptTask = "4adoptzz"
	depFile   = "security/base/kyverno/helmrelease-controller.yaml"
	ageCheck  = "renovate/stability-days"
	bot       = "renovate[bot]"
	adoptHead = "cccccccccccccccccccccccccccccccccccccccc"
	adoptBase = "dddddddddddddddddddddddddddddddddddddddd"
)

// depsPatchClass is Task 12's deps-patch fixture, in shadow.
func depsPatchClass() config.Class {
	return config.Class{Shadow: true, DiffShape: "semver-patch", ModifyOnly: true, MaxPatchFiles: 3, MaxModifiedLines: 10,
		AllowedFiles:    []string{`^(apps|flux|infrastructure|observability|security|tooling)/.+\.ya?ml$`},
		ProtectedFiles:  []string{`^security/base/zitadel/`},
		ReleaseAgeCheck: ageCheck, MaxPerDay: 2,
		Author: &config.Author{Adopt: &config.Adopt{Logins: []string{bot}, Branches: []string{`^renovate/`}}}}
}

// adoptRig: an adopted Task for Renovate's PR 30 on renovate/kyverno-3.x, a patch bump of one
// allowed file, CI and policy green, the release age passed, every commit and push Renovate's.
func adoptRig(t *testing.T) *rig {
	t.Helper()
	tk := awaiting()
	tk.Name = adoptTask
	tk.Spec.Template, tk.Spec.PredictedClass, tk.Spec.Issue = "", "deps-patch", 0
	tk.Spec.Source = v1alpha1.Source{Kind: "adopt", Ref: "Smana/cloud-native-ref#30", Key: "adopt:Smana/cloud-native-ref#30",
		RequestedBy: "github:" + bot, Trust: "untrusted", ContentSHA256: strings.Repeat("a", 64)}
	tk.Status.Phase, tk.Status.RoomRef, tk.Status.Runs = v1alpha1.PhaseAwaitingCI, "", nil
	tk.Status.PullRequest = &v1alpha1.PullRequestRef{Number: 30, NodeID: "PR_30", HeadSHA: adoptHead}
	g := newRig(t, tk)
	g.r.Merger = g.f
	g.r.Cfg.AgentsLogin, g.r.Cfg.FactoryLogin = "ogenki-agents[bot]", "ogenki-agent-factory[bot]"
	g.r.Cfg.Classes = map[string]config.Class{"deps-patch": depsPatchClass()}
	g.r.Cfg.Merge = config.Merge{RequiredChecks: required, VerifyChecks: required, PolicyBotLogin: "ogenki-merge-gate[bot]",
		MergerLogin: "ogenki-agent-merger[bot]", AutoMergesPerDay: 10, FixRuns: 2, CheckReruns: 1,
		VerifyFor: config.Duration{Duration: 30 * time.Minute}, RevertWindow: config.Duration{Duration: 168 * time.Hour},
		Breaker: config.Breaker{Window: 10, MaxReverts: 1}}
	g.f.SetPR(forge.PR{Number: 30, NodeID: "PR_30", State: "OPEN", Title: "chore(deps): update kyverno to v3.8.4", Author: bot,
		HeadRef: "renovate/kyverno-3.x", HeadSHA: adoptHead})
	g.f.SetCommits(30, forge.Commit{SHA: "c1", Author: bot}, forge.Commit{SHA: adoptHead, Author: bot})
	g.f.SetActivity("renovate/kyverno-3.x", forge.Push{Type: "push", Actor: bot, Before: "c1", After: adoptHead},
		forge.Push{Type: "branch_creation", Actor: bot, After: "c1"})
	checks := green("SUCCESS")
	checks.Statuses = append(checks.Statuses, forge.Status{Context: ageCheck, State: "SUCCESS", Creator: bot})
	g.f.SetChecks(30, checks)
	g.f.SetFiles(map[string]string{depFile: "version: 3.8.3\n"}, map[string]string{depFile: "version: 3.8.4\n"})
	g.f.SetFileStats(forge.FileStat{Path: depFile, Status: "modified", Additions: 1, Deletions: 1})
	return g
}

func (g *rig) editPR(f func(p *forge.PR)) {
	p, _ := g.f.PullRequest(context.Background(), 30)
	f(&p)
	g.f.SetPR(p)
}

func (g *rig) setHeadRef(_ int, ref string) { g.editPR(func(p *forge.PR) { p.HeadRef = ref }) }
func (g *rig) setAuthor(_ int, who string)  { g.editPR(func(p *forge.PR) { p.Author = who }) }
func (g *rig) approve(_ int, who string) {
	g.editPR(func(p *forge.PR) {
		p.Reviews = append(p.Reviews, forge.Review{ID: 1, Author: who, State: "APPROVED", At: now})
	})
}

func (g *rig) setStatus(n int, name, state, creator string) {
	c, _ := g.f.PullRequestChecks(context.Background(), n)
	for i := range c.Statuses {
		if c.Statuses[i].Context == name {
			c.Statuses[i] = forge.Status{Context: name, State: state, Creator: creator}
		}
	}
	g.f.SetChecks(n, c)
}

func (g *rig) cancelOne(n int) {
	c, _ := g.f.PullRequestChecks(context.Background(), n)
	c.Runs[0].State, c.Runs[0].ID = "CANCELLED", 776
	g.f.SetChecks(n, c)
}

// noEmptyRoom is the broker: it has no log for the room named "".
type noEmptyRoom struct{ RoomLog }

var errNoRoom = errors.New("the broker refuses a room named \"\"")

func (n noEmptyRoom) EventsSince(ctx context.Context, room string, after int64, keep func(envelope.Event) bool) ([]envelope.Event, int64, error) {
	if room == "" {
		return nil, 0, errNoRoom
	}
	return n.RoomLog.EventsSince(ctx, room, after, keep)
}

func (n noEmptyRoom) LastSeq(ctx context.Context, room string) (int64, error) {
	if room == "" {
		return 0, errNoRoom
	}
	return n.RoomLog.LastSeq(ctx, room)
}

func (n noEmptyRoom) Queue(ctx context.Context, room string) ([]rooms.Queued, error) {
	if room == "" {
		return nil, errNoRoom
	}
	return n.RoomLog.Queue(ctx, room)
}

func (n noEmptyRoom) Enqueue(ctx context.Context, room, stream, text string, seq int64) error {
	if room == "" {
		return errNoRoom
	}
	return n.RoomLog.Enqueue(ctx, room, stream, text, seq)
}

// Offline proof 15: a second committer, a branch outside renovate/*, a stranger's push or a
// pending renovate/stability-days refuses or waits; a minor goes nowhere near deps-patch.
func TestAdoptedGate(t *testing.T) {
	for name, c := range map[string]struct {
		edit          func(g *rig)
		phase, reason string
	}{
		"clean, shadow": {func(*rig) {}, v1alpha1.PhaseAwaitingHuman, "shadow_would_arm"},
		"a second committer": {func(g *rig) {
			g.f.SetCommits(30, forge.Commit{SHA: "c1", Author: bot}, forge.Commit{SHA: adoptHead, Author: "Smana"})
		}, v1alpha1.PhaseAwaitingHuman, "human_commit_in_pr"},
		"the head is not among the commits": {func(g *rig) {
			g.f.SetCommits(30, forge.Commit{SHA: "c1", Author: bot})
		}, v1alpha1.PhaseAwaitingHuman, "human_commit_in_pr"},
		"commits past the page": {func(g *rig) { g.f.SetCommitsTruncated(30) }, v1alpha1.PhaseAwaitingHuman, "human_commit_in_pr"},
		"a stranger's push": {func(g *rig) {
			g.f.SetActivity("renovate/kyverno-3.x", forge.Push{Type: "push", Actor: "Smana", Before: "c1", After: adoptHead},
				forge.Push{Type: "branch_creation", Actor: bot, After: "c1"})
		}, v1alpha1.PhaseAwaitingHuman, "human_commit_in_pr"},
		"another branch": {func(g *rig) { g.setHeadRef(30, "feature/x") }, v1alpha1.PhaseAwaitingHuman, "branch_not_allowed"},
		"another author": {func(g *rig) { g.setAuthor(30, "dependabot[bot]") }, v1alpha1.PhaseAwaitingHuman, "not_adopted_author"},
		"release age pending": {func(g *rig) { g.setStatus(30, ageCheck, "PENDING", bot) },
			v1alpha1.PhaseAwaitingCI, "release_age_pending"},
		"release age absent": {func(g *rig) {
			c, _ := g.f.PullRequestChecks(t.Context(), 30)
			c.Statuses = c.Statuses[:1]
			g.f.SetChecks(30, c)
		}, v1alpha1.PhaseAwaitingCI, "release_age_pending"},
		"release age failed": {func(g *rig) { g.setStatus(30, ageCheck, "FAILURE", bot) },
			v1alpha1.PhaseAwaitingHuman, "release_age_red"},
		"release age by a stranger": {func(g *rig) { g.setStatus(30, ageCheck, "SUCCESS", "Smana") },
			v1alpha1.PhaseAwaitingHuman, "release_age_red"},
		"a minor": {func(g *rig) {
			g.f.SetFiles(map[string]string{depFile: "version: 3.8.4\n"}, map[string]string{depFile: "version: 3.9.0\n"})
		}, v1alpha1.PhaseAwaitingHuman, "class_mismatch"},
		"a maintainer approved": {func(g *rig) { g.approve(30, "Smana") }, v1alpha1.PhaseAwaitingHuman, "maintainer_approved"},
	} {
		t.Run(name, func(t *testing.T) {
			g := adoptRig(t)
			c.edit(g)
			tk := g.reconcile(t, adoptTask, 1)
			if tk.Status.Phase != c.phase || tk.Status.Reason != c.reason {
				t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
			}
			if len(g.f.Armed()) != 0 {
				t.Fatal("shadow never merges")
			}
		})
	}
}

// A live adopted class merges pinned to the head it decided on: a push after the decision is
// refused by GitHub and decided again, never merged.
func TestAnAdoptedLiveClassMergesThePinnedHead(t *testing.T) {
	g := adoptRig(t)
	cl := depsPatchClass()
	cl.Shadow, cl.Live = false, true
	g.r.Cfg.Classes["deps-patch"] = cl
	if tk := g.reconcile(t, adoptTask, 1); tk.Status.Phase != v1alpha1.PhaseAutoMerging || len(g.f.Armed()) != 1 {
		t.Fatalf("%s %s %v", tk.Status.Phase, tk.Status.Reason, g.f.Armed())
	}
	if got := g.f.Armed(); got[0] != "PR_30 "+adoptHead {
		t.Fatalf("merged %q, decided on %q", got[0], adoptHead)
	}
	g = adoptRig(t)
	g.r.Cfg.Classes["deps-patch"] = cl
	g.f.RefuseMerge("PR_30", forge.ErrHeadMoved)
	if tk := g.reconcile(t, adoptTask, 1); tk.Status.Phase != v1alpha1.PhaseAwaitingCI || tk.Status.Reason != "head_moved" ||
		len(g.f.Armed()) != 0 {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

// M12: an adopted Task has no room (status.roomRef is ""). On green CI the gate must read none:
// the broker refuses a room named "", and every adopted Task would retry forever.
func TestAnAdoptedTaskReadsNoRoom(t *testing.T) {
	g := adoptRig(t)
	g.r.Rooms = noEmptyRoom{g.r.Rooms}
	if tk := g.reconcile(t, adoptTask, 1); tk.Status.Reason != "shadow_would_arm" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

// L15: a cancelled check on Renovate's PR is handed off, never re-run by the factory.
func TestAnAdoptedCancelledCheckHandsOff(t *testing.T) {
	g := adoptRig(t)
	g.r.Rooms = noEmptyRoom{g.r.Rooms}
	g.cancelOne(30)
	if tk := g.reconcile(t, adoptTask, 1); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "ci_cancelled" ||
		len(g.f.Reruns()) != 0 {
		t.Fatalf("%s %s reruns %v", tk.Status.Phase, tk.Status.Reason, g.f.Reruns())
	}
}

// R45: a red check on Renovate's PR is handed off, never fixed by a run of the factory's.
func TestAnAdoptedRedCheckHandsOff(t *testing.T) {
	g := adoptRig(t)
	g.r.Rooms = noEmptyRoom{g.r.Rooms}
	c, _ := g.f.PullRequestChecks(t.Context(), 30)
	c.Runs[0].State = "FAILURE"
	g.f.SetChecks(30, c)
	tk := g.reconcile(t, adoptTask, 1)
	if tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "ci_red" || tk.Status.FixRuns != 0 {
		t.Fatalf("%s %s fixes %d", tk.Status.Phase, tk.Status.Reason, tk.Status.FixRuns)
	}
}

// The release age is named in what a wait still waits on.
func TestTheReleaseAgeIsAWaitedOnCheck(t *testing.T) {
	g := adoptRig(t)
	c, _ := g.f.PullRequestChecks(t.Context(), 30)
	in := ArmInputs{Checks: c, Task: &v1alpha1.Task{}, Cfg: g.r.Cfg}
	if got := unreported(in, depsPatchClass()); len(got) != 0 {
		t.Fatalf("passed: %v", got)
	}
	g.setStatus(30, ageCheck, "PENDING", bot)
	in.Checks, _ = g.f.PullRequestChecks(t.Context(), 30)
	if got := unreported(in, depsPatchClass()); len(got) != 1 || got[0] != ageCheck {
		t.Fatalf("pending: %v", got)
	}
}
