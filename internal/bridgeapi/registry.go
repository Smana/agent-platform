// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"context"
	"sync"
)

// registry holds this replica's SSE streams, one per run: the last stream per
// runId wins on a replica (§3), which also bounds a run to one connection here.
type registry struct {
	mu      sync.Mutex
	streams map[string]*context.CancelFunc
}

// attach registers a stream for runID, ending the one it replaces. done ends
// this stream and unregisters it, unless a newer one took its place.
func (r *registry) attach(ctx context.Context, runID string) (streamCtx context.Context, done func()) {
	ctx, cancel := context.WithCancel(ctx)
	entry := &cancel
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.streams == nil {
		r.streams = map[string]*context.CancelFunc{}
	}
	if old := r.streams[runID]; old != nil {
		(*old)()
	}
	r.streams[runID] = entry
	return ctx, func() {
		cancel()
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.streams[runID] == entry {
			delete(r.streams, runID)
		}
	}
}

// drop ends runID's stream on this replica.
func (r *registry) drop(runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.streams[runID]; c != nil {
		(*c)()
		delete(r.streams, runID)
	}
}

// dropAll ends every stream, so a shutting-down replica's drain is not held
// open by streams that would otherwise last until their tokens expire.
func (r *registry) dropAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, c := range r.streams {
		(*c)()
		delete(r.streams, id)
	}
}
