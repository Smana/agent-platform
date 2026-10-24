// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/filefence"
	"github.com/Smana/agent-platform/internal/factory/fmetrics"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/semverdiff"
	"github.com/Smana/agent-platform/internal/factory/taskid"
)

// maxOpenAdopted bounds the adopted Tasks a repository holds, per class, open at once: a Renovate backlog is
// adopted ten at a time, as each ends.
const maxOpenAdopted = 10

// openPRPage is OpenPullRequests' page: a list that fills it may have more behind it, and an
// unread pull request is never read as absent.
const openPRPage = 100

// adoptBase is the branch an adopted pull request's diff is read against.
const adoptBase = "main"

// Adopter is a leader-only manager.Runnable (MC10): it turns an open pull request of an adopted
// class's author (Renovate) into a Task with no run, in AwaitingCI once the reconciler receives it.
// It decides membership only: the merge gate decides everything again on the head it reads.
// The Task's name is its key's, `adopt:<repository>#<number>`, so a pull request is adopted once,
// whatever happens to its Task and across a restart. Reads come from the manager's informer cache, so
// a stale one can briefly undercount the open Tasks; a duplicate is impossible all the same, because
// the name is deterministic and a second Create conflicts. Only pull requests into main, from the
// repository itself, are adopted (a fork is refused here and again by the gate).
type Adopter struct {
	// Clients is a repository's factory App and merger connections (v2 D8), as the Scheduler's.
	Clients   func(repository string) (ScheduleForge, ScheduleMerger, bool)
	Client    client.Client
	Namespace string
	Cfg       *config.Config
	// Owned is v2 D7's fence: a repository not owned by this factory adopts nothing.
	Owned   func(repository string) bool
	Stopped func(context.Context) bool
	Now     func() time.Time
	Errors  errorCounter
	Log     *slog.Logger

	// judged is an optimisation only (a restart re-reads): the heads whose diff named no class, so
	// a pull request that is not adopted is not diffed again every minute (the rate limit). A new
	// head is a new read. It never stands for "adopted": that is the Task's name.
	mu     sync.Mutex
	judged map[string]bool
}

// NeedLeaderElection is true: two adopters would race every pull request.
func (a *Adopter) NeedLeaderElection() bool { return true }

func (a *Adopter) log() *slog.Logger {
	if a.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return a.Log
}

// Start ticks at once, then every minute, until ctx ends; a failed tick is counted and retried.
func (a *Adopter) Start(ctx context.Context) error {
	t := time.NewTicker(scheduleEvery)
	defer t.Stop()
	for {
		if err := a.Tick(ctx); err != nil {
			if a.Errors != nil {
				a.Errors.IntakeError(ctx, "adopt")
			}
			a.log().Warn("adopt tick failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// adoptShape is a diff shape the poller tries, in order, against the classes declaring it.
type adoptShape struct {
	name string
	ok   func(base, head map[string]string) (bool, string)
}

var adoptShapes = []adoptShape{{"semver-patch", semverdiff.Patch}, {"semver-minor", semverdiff.Minor}}

// Tick adopts the open pull requests of every owned repository that has an adopted class live or
// in shadow. A list that fails or fills its page, or a diff that cannot be read, adopts nothing
// for that read: it is an error to retry, never a pull request taken as absent.
func (a *Adopter) Tick(ctx context.Context) error {
	if a.Stopped(ctx) {
		return nil
	}
	var errs []error
	for _, rc := range a.Cfg.Repos() {
		if a.Owned == nil || !a.Owned(rc.Name) || len(adoptedClasses(&rc)) == 0 {
			continue
		}
		if err := a.tick(fmetrics.WithRepository(ctx, rc.Name), &rc); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", rc.Name, err))
		}
	}
	return errors.Join(errs...)
}

// adoptedClasses are the repository's classes that adopt, live or in shadow (MC14: a class off
// adopts nothing), by name.
func adoptedClasses(rc *config.Repository) []string {
	var out []string
	for name, cl := range rc.Classes {
		if cl.Adopted() && (cl.Live || cl.Shadow) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func (a *Adopter) tick(ctx context.Context, rc *config.Repository) error {
	var l v1alpha1.TaskList
	if err := a.Client.List(ctx, &l, client.InNamespace(a.Namespace)); err != nil {
		return err
	}
	known, open := map[string]bool{}, map[string]int{} // open: per class
	for i := range l.Items {
		t := &l.Items[i]
		if t.Spec.Source.Kind != "adopt" || !strings.EqualFold(t.Spec.Repository, rc.Name) {
			continue
		}
		known[t.Spec.Source.Key] = true
		if !v1alpha1.TerminalPhase(t.Status.Phase) {
			open[t.Spec.PredictedClass]++
		}
	}
	full := func(class string) bool { return open[class] >= maxOpenAdopted }
	if !slices.ContainsFunc(adoptedClasses(rc), func(c string) bool { return !full(c) }) {
		return nil
	}
	gh, merger, ok := a.Clients(rc.Name)
	if !ok {
		return fmt.Errorf("intake: no GitHub connection for %s (v2 D8)", rc.Name)
	}
	prs, err := gh.OpenPullRequests(ctx)
	if err != nil {
		return err
	}
	if len(prs) >= openPRPage {
		return fmt.Errorf("intake: %d open pull requests fill the page read: adopting none", len(prs))
	}
	slices.SortFunc(prs, func(x, y forge.PRSummary) int { return x.Number - y.Number })
	var errs []error
	for _, p := range prs {
		key := fmt.Sprintf("adopt:%s#%d", rc.Name, p.Number)
		if known[key] {
			continue
		}
		class, ok, err := a.classOf(ctx, rc, merger, p, full)
		if err != nil {
			errs = append(errs, fmt.Errorf("#%d: %w", p.Number, err))
			continue
		}
		if !ok {
			continue
		}
		created, err := a.create(ctx, rc, p, key, class)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if created {
			open[class]++
		}
	}
	return errors.Join(errs...)
}

// candidates are the classes whose author and branch rules the pull request meets, by shape.
func candidates(rc *config.Repository, p forge.PRSummary) map[string]string {
	out := map[string]string{} // shape -> class
	for _, name := range adoptedClasses(rc) {
		cl := rc.Classes[name]
		ad := cl.Author.Adopt
		if p.CrossRepository || p.BaseRef != adoptBase { // a fork's names are its own; the diff is read against main
			continue
		}
		if !slices.ContainsFunc(ad.Logins, func(l string) bool { return forge.SameLogin(p.Author, l) }) || !branchAdopted(ad, p.HeadRef) {
			continue
		}
		if _, dup := out[cl.DiffShape]; !dup {
			out[cl.DiffShape] = name
		}
	}
	return out
}

// branchAdopted: the head branch starts with one of the literal prefixes (Validate: ^ and a
// literal): the same result as the gate's QuoteMeta'd pattern, with no pattern to compile.
func branchAdopted(ad *config.Adopt, ref string) bool {
	return slices.ContainsFunc(ad.Branches, func(b string) bool { return strings.HasPrefix(ref, strings.TrimPrefix(b, "^")) })
}

// classOf is the class the pull request's diff names: the first shape that holds, among the
// classes it is eligible for. A diff too large to read names none.
func (a *Adopter) classOf(ctx context.Context, rc *config.Repository, m ScheduleMerger, p forge.PRSummary, full func(string) bool) (string, bool, error) {
	cands := candidates(rc, p)
	if len(cands) == 0 || p.HeadSHA == "" {
		return "", false, nil
	}
	if !slices.ContainsFunc(slices.Collect(maps.Values(cands)), func(c string) bool { return !full(c) }) {
		return "", false, nil // every class it could join is full: it waits, and no verdict is cached
	}
	seen := fmt.Sprintf("%s#%d@%s", rc.Name, p.Number, p.HeadSHA)
	a.mu.Lock()
	skip := a.judged[seen]
	a.mu.Unlock()
	if skip {
		return "", false, nil
	}
	d, err := m.Diff(ctx, adoptBase, p.HeadSHA)
	if errors.Is(err, forge.ErrDiffTooLarge) {
		a.judge(seen)
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	for _, s := range adoptShapes {
		class, ok := cands[s.name]
		if !ok {
			continue
		}
		// The shape and the class's file conditions, which are the gate's own functions: a pull
		// request outside the fence is not this class's, whatever its shape.
		if held, _ := s.ok(d.Base, d.Head); held && filefence.Refusal(rc.Classes[class], d.Files) == "" {
			// The class is the one the gate would pick among all eligible ones. When it is full the
			// pull request waits for a place: a full class never pushes it into another.
			return class, !full(class), nil
		}
	}
	a.judge(seen)
	return "", false, nil
}

// judge remembers a head that named no class, bounded so a long-lived leader holds a few hundred.
func (a *Adopter) judge(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.judged == nil || len(a.judged) >= 512 {
		a.judged = map[string]bool{}
	}
	a.judged[key] = true
}

func (a *Adopter) create(ctx context.Context, rc *config.Repository, p forge.PRSummary, key, class string) (bool, error) {
	text := fmt.Sprintf("Adopted pull request #%d from %s (%s).", p.Number, p.Author, p.HeadRef)
	sum := sha256.Sum256([]byte(text))
	dataClass := a.Cfg.Defaults.DataClass
	t := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: taskid.Name(key), Namespace: a.Namespace},
		Spec: v1alpha1.TaskSpec{
			Source: v1alpha1.Source{Kind: "adopt", Ref: fmt.Sprintf("%s#%d", rc.Name, p.Number), Key: key,
				RequestedBy: "github:" + p.Author, Trust: "untrusted", ContentSHA256: hex.EncodeToString(sum[:])},
			Repository: rc.Name, Text: text, DataClass: dataClass, PredictedClass: class,
		},
	}
	err := a.Client.Create(ctx, t)
	if apierrors.IsAlreadyExists(err) {
		return false, nil // the name is the dedup (S2)
	}
	return err == nil, err
}
