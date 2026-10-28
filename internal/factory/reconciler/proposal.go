// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/envelope"
)

// ErrNoProposal is returned by Proposal when the triager made no proposal.
var ErrNoProposal = errors.New("reconciler: the triager made no proposal")

// Proposal is the text of the investigate task's proposal (RunLore agent-to-agent R7): the summary
// of the last handoff to the implementer the task's last triager run recorded itself, as the broker
// stamped it. Nothing else in the room counts, so no other writer can put text in a public issue.
// The caller sanitises it: the proposal is internal until it is.
func (r *Reconciler) Proposal(ctx context.Context, t *v1alpha1.Task) (string, error) {
	var run *v1alpha1.RunRecord
	for i := len(t.Status.Runs) - 1; i >= 0; i-- {
		if t.Status.Runs[i].Role == "triager" {
			run = &t.Status.Runs[i]
			break
		}
	}
	if run == nil || t.Status.RoomRef == "" {
		return "", ErrNoProposal
	}
	toImplementer := handoffTo("implementer")
	evs, complete, err := r.roomTail(ctx, t.Status.RoomRef, run.StartSeq, func(e envelope.Event) bool {
		return e.RunID == run.ID && e.Origin == envelope.OriginClient && e.Actor.Kind == envelope.ActorAgent &&
			e.Actor.ID == "agent:"+run.ID && toImplementer(e)
	})
	switch {
	case err != nil:
		return "", err
	case !complete:
		return "", fmt.Errorf("reconciler: the room of %s is longer than the factory reads", t.Name) // the newest handoff may be unread
	case len(evs) == 0:
		return "", ErrNoProposal
	}
	var p envelope.HandoffPayload
	if err := json.Unmarshal(evs[len(evs)-1].Payload, &p); err != nil {
		return "", fmt.Errorf("reconciler: the proposal's handoff: %w", err)
	}
	return p.Summary, nil
}
