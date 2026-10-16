// SPDX-License-Identifier: Apache-2.0

// Package suspend is the room-broker's suspend trigger (v2 warm-sandboxes design §6, owner
// decision 2026-10-16): a suspendable run in an attended room that waits on an approval while
// the room stays idle is suspended, and resumed once nothing waits. It acts only through the
// run's suspend annotations, under the claim's resourceVersion, and mints nothing: the
// composition deletes and recreates the pod, and the resumed bridge answers the confirmation.
package suspend

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
)

// The operating modes the composition reads.
const (
	ModeSuspended = "Suspended"
	ModeRunning   = "Running"
)

// minSeconds is the shortest deadline the composition takes: under it, remaining-seconds is
// ignored and the pod gets the run's whole budget.
const minSeconds = 60

// seconds is the composition's remaining-seconds shape.
var seconds = regexp.MustCompile(`^[0-9]{1,6}$`)

// Decide is the patch of run's annotations, if any. A Running run that waits on an approval
// in a room idle since idleSince for at least after is suspended, with the running time it has
// left; a suspended one whose approvals are all closed is resumed, and the resume time
// recorded. Anything else, a run that is not suspendable or no longer live included, is left
// alone.
func Decide(after time.Duration, run runwatch.Run, waiting bool, idleSince, now time.Time) (map[string]string, bool) {
	if !run.Suspendable || !run.Live() {
		return nil, false
	}
	switch {
	case run.OperatingMode == ModeSuspended && !waiting:
		return map[string]string{runwatch.OperatingModeAnnotation: ModeRunning,
			runwatch.ResumedAtAnnotation: now.UTC().Format(time.RFC3339)}, true
	case run.OperatingMode != ModeSuspended && waiting && run.Phase == "Running" && !run.StartedAt.IsZero() &&
		now.Sub(idleSince) >= after:
		return map[string]string{runwatch.OperatingModeAnnotation: ModeSuspended,
			runwatch.RemainingSecondsAnnotation: strconv.FormatInt(remaining(run, now), 10)}, true
	}
	return nil, false
}

// remaining is the running time run has left: what the current pod was given, as the
// composition read it, less what it ran since it started or was last resumed. It never
// drops under minSeconds, so a run suspended at its deadline still gets a pod to end in.
func remaining(run runwatch.Run, now time.Time) int64 {
	budget := run.MaxMinutes * 60
	if seconds.MatchString(run.RemainingSeconds) {
		if s, _ := strconv.ParseInt(run.RemainingSeconds, 10, 64); s >= minSeconds {
			budget = min(budget, s)
		}
	}
	since := run.StartedAt
	if run.ResumedAt.After(since) {
		since = run.ResumedAt
	}
	return max(budget-int64(now.Sub(since)/time.Second), minSeconds)
}

// approvals is the one store read the trigger needs; *store.Store has it.
type approvals interface {
	OpenApprovals(ctx context.Context, roomID string) ([]store.Approval, error)
}

// patcher writes a claim's annotations; a controller-runtime client.Client is one.
type patcher interface {
	Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error
}

// Suspender applies Decide to a room's runs.
type Suspender struct {
	Client    patcher
	Approvals approvals
	// After is how long a room stays idle, with a run waiting, before the run is suspended.
	After time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log records each suspend and resume; nil discards them.
	Log *slog.Logger
}

// Room suspends or resumes each of runs, the runs of room, whose log last moved at idleSince.
// An unattended room is never touched: nobody is expected there. A run whose claim moved
// since it was read is skipped, with the conflict returned, and decided again at the next
// resync.
func (s *Suspender) Room(ctx context.Context, room *v1alpha1.Room, idleSince time.Time, runs []runwatch.Run) error {
	if room.Spec.Approvals.Profile == "unattended" {
		return nil
	}
	open, err := s.Approvals.OpenApprovals(ctx, room.Name)
	if err != nil {
		return fmt.Errorf("suspend: room %s: %w", room.Name, err)
	}
	waiting := map[string]bool{}
	for _, a := range open {
		waiting[a.RunID] = true
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	for _, run := range runs {
		ann, ok := Decide(s.After, run, waiting[run.ID], idleSince, now)
		if !ok {
			continue
		}
		if err := s.patch(ctx, run, ann); err != nil {
			return fmt.Errorf("suspend: run %s of room %s: %w", run.ID, room.Name, err)
		}
		s.log().Info("run "+map[string]string{ModeSuspended: "suspended", ModeRunning: "resumed"}[ann[runwatch.OperatingModeAnnotation]],
			"room", room.Name, "run", run.ID, "remainingSeconds", ann[runwatch.RemainingSecondsAnnotation])
	}
	return nil
}

// patch merges ann into the run's claim, under the resourceVersion it was read at.
func (s *Suspender) patch(ctx context.Context, run runwatch.Run, ann map[string]string) error {
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": run.ResourceVersion,
		"annotations": ann}})
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(runwatch.GVK())
	u.SetNamespace(runwatch.Namespace)
	u.SetName(runwatch.ClaimPrefix + run.ID)
	return s.Client.Patch(ctx, u, client.RawPatch(types.MergePatchType, body))
}

func (s *Suspender) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}
