// SPDX-License-Identifier: Apache-2.0

package runwatch

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var gvk = schema.GroupVersionKind{Group: "cloud.ogenki.io", Version: "v1alpha1", Kind: "AgentRun"}

// GVK is the AgentRun claim's kind (C3).
func GVK() schema.GroupVersionKind { return gvk }

// Watcher runs on every replica: every replica must cut its own connections of a
// run that ended (S4). Appending to the log is the leader's job (Events).
//
// Callbacks run synchronously, in registration order, on the caller of Upsert or
// Remove (the informer's handler goroutine): one at a time, so a slow callback
// delays the watch rather than spawning work without bound.
type Watcher struct {
	mu      sync.RWMutex
	runs    map[string]Run
	gone    []func(context.Context, Run)
	changed []func(context.Context, Run)
	removed []func(context.Context, Run)
	// synced reports that the AgentRun cache has delivered its first list; nil before Register.
	synced func() bool
	live   []func(context.Context, int64)
}

// New returns an empty Watcher.
func New() *Watcher { return &Watcher{runs: map[string]Run{}} }

// OnGone registers f for a run that stops being live (terminal, revoked or
// deleted). It fires once per run.
func (w *Watcher) OnGone(f func(context.Context, Run)) {
	w.mu.Lock()
	w.gone = append(w.gone, f)
	w.mu.Unlock()
}

// OnLive registers f for the count of live runs naming a room: +1 when a run
// enters that set, -1 when it leaves (terminal, revoked or deleted). A run is
// counted at most once, whatever the updates, resyncs and deletes that follow.
// Register f before the informer starts: a Remove of a run it never saw counted
// would otherwise drive the value negative.
func (w *Watcher) OnLive(f func(context.Context, int64)) {
	w.mu.Lock()
	w.live = append(w.live, f)
	w.mu.Unlock()
}

// counted is whether r is in the set OnLive counts.
func counted(r Run) bool { return r.Room != "" && r.Live() }

// notifyLive reports the move of one run in or out of the counted set.
func notifyLive(ctx context.Context, fs []func(context.Context, int64), was, is bool) {
	if was == is {
		return
	}
	d := int64(1)
	if was {
		d = -1
	}
	for _, f := range fs {
		f(ctx, d)
	}
}

// OnChange registers f for every observed state of a run, replays included.
func (w *Watcher) OnChange(f func(context.Context, Run)) {
	w.mu.Lock()
	w.changed = append(w.changed, f)
	w.mu.Unlock()
}

// OnRemove registers f for every removed claim that was watched (review M15),
// with its last known state.
func (w *Watcher) OnRemove(f func(context.Context, Run)) {
	w.mu.Lock()
	w.removed = append(w.removed, f)
	w.mu.Unlock()
}

// Upsert records the claim's current state. A claim that is not a run is ignored.
func (w *Watcher) Upsert(ctx context.Context, u *unstructured.Unstructured) {
	cur, ok := FromUnstructured(u)
	if !ok {
		return
	}
	w.mu.Lock()
	old, had := w.runs[cur.ID]
	w.runs[cur.ID] = cur
	gone, changed, live := slices.Clone(w.gone), slices.Clone(w.changed), slices.Clone(w.live)
	w.mu.Unlock()
	notifyLive(ctx, live, had && counted(old), counted(cur))
	// OnGone first: cutting a dead run's connections must never wait behind
	// OnChange's appends to the log.
	if !cur.Live() && (!had || old.Live()) {
		for _, f := range gone {
			f(ctx, cur)
		}
	}
	for _, f := range changed {
		f(ctx, cur)
	}
}

// Remove forgets a deleted claim, firing OnGone if it was still live, then OnRemove.
func (w *Watcher) Remove(ctx context.Context, u *unstructured.Unstructured) {
	r, ok := FromUnstructured(u)
	if !ok {
		return
	}
	w.mu.Lock()
	old, had := w.runs[r.ID]
	delete(w.runs, r.ID)
	gone, removed, live := slices.Clone(w.gone), slices.Clone(w.removed), slices.Clone(w.live)
	w.mu.Unlock()
	if !had {
		return
	}
	notifyLive(ctx, live, counted(old), false)
	if old.Live() {
		for _, f := range gone {
			f(ctx, old)
		}
	}
	for _, f := range removed {
		f(ctx, old)
	}
}

// SetSynced installs the report that this watcher holds every run (Register does it).
// Until it says true, a run the watcher lacks is unknown, not dead.
func (w *Watcher) SetSynced(f func() bool) {
	w.mu.Lock()
	w.synced = f
	w.mu.Unlock()
}

// Get returns the run's last known state, terminal or not.
func (w *Watcher) Get(id string) (Run, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	r, ok := w.runs[id]
	return r, ok
}

// Live returns the run only while it is live. known is false for a run the watcher
// lacks before its cache has synced (this replica may simply lag the API server). Once
// synced, an absent run is known and dead: it was deleted.
func (w *Watcher) Live(id string) (run Run, live, known bool) {
	w.mu.RLock()
	r, ok := w.runs[id]
	synced := w.synced
	w.mu.RUnlock()
	if ok {
		return r, r.Live(), true
	}
	return Run{}, false, synced != nil && synced()
}

// InRoom returns the watched runs naming room, terminal ones included.
func (w *Watcher) InRoom(room string) []Run {
	w.mu.RLock()
	defer w.mu.RUnlock()
	var out []Run
	for _, r := range w.runs {
		if r.Room == room {
			out = append(out, r)
		}
	}
	return out
}

// All returns every watched run.
func (w *Watcher) All() []Run {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]Run, 0, len(w.runs))
	for _, r := range w.runs {
		out = append(out, r)
	}
	return out
}

// informerSource is the one cache method Register calls; a manager's cache.Cache has it.
type informerSource interface {
	GetInformer(ctx context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error)
}

// Register attaches the watcher to the cache's informer for AgentRuns. ctx is
// handed to every callback, so it must live as long as the informer does.
//
// synced reports once this handler has been delivered the informer's first list,
// so the Watcher holds every run. The cache's own sync does not say that: the
// informer's store can be full while the handler's notifications are still
// queued (client-go: HasSynced "doesn't tell you if an individual handler is
// synced"). Anything that reads the Watcher as complete waits on synced.
func Register(ctx context.Context, c informerSource, w *Watcher) (synced func() bool, err error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	inf, err := c.GetInformer(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("runwatch: agentrun informer: %w", err)
	}
	reg, err := inf.AddEventHandler(handler(ctx, w))
	if err != nil {
		return nil, fmt.Errorf("runwatch: agentrun event handler: %w", err)
	}
	synced = inf.HasSynced
	if reg != nil { // without a registration the informer's own sync is all there is
		synced = reg.HasSynced
	}
	w.SetSynced(synced)
	return synced, nil
}

func handler(ctx context.Context, w *Watcher) toolscache.ResourceEventHandlerFuncs {
	return toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { upsert(ctx, w, o) },
		UpdateFunc: func(_, o any) { upsert(ctx, w, o) },
		DeleteFunc: func(o any) {
			// A delete the informer missed arrives as a tombstone around the last known object.
			if d, ok := o.(toolscache.DeletedFinalStateUnknown); ok {
				o = d.Obj
			}
			if u, ok := o.(*unstructured.Unstructured); ok {
				w.Remove(ctx, u)
			}
		},
	}
}

func upsert(ctx context.Context, w *Watcher, o any) {
	if u, ok := o.(*unstructured.Unstructured); ok {
		w.Upsert(ctx, u)
	}
}
