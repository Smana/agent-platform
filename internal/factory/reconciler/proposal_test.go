// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"errors"
	"testing"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/envelope"
)

func handoffEvent(seq int64, run, actor string, origin envelope.Origin, summary string) envelope.Event {
	return envelope.Event{Seq: seq, RunID: run, Type: envelope.Handoff, Origin: origin,
		Actor:   envelope.Actor{Kind: envelope.ActorAgent, ID: actor},
		Payload: envelope.Must(envelope.HandoffPayload{FromRole: "triager", ToRole: "implementer", Summary: summary})}
}

// R7: the proposal is the last handoff to the implementer the triager's own run recorded; an older
// run's, another run's, and one anyone else wrote are not.
func TestProposal(t *testing.T) {
	tk := issueTask("3buqdlot", 7, "# x")
	tk.Status.RoomRef = "3buqdlot"
	tk.Status.Runs = []v1alpha1.RunRecord{{ID: "aaaaaaaa", Role: "triager", Trigger: "initial", StartSeq: 3},
		{ID: "bbbbbbbb", Role: "triager", Trigger: "retry", StartSeq: 10}}
	g := newRig(t, tk)
	g.log.evs = []envelope.Event{
		handoffEvent(5, "aaaaaaaa", "agent:aaaaaaaa", envelope.OriginClient, "older run"),
		handoffEvent(11, "bbbbbbbb", "agent:bbbbbbbb", envelope.OriginClient, "first"),
		handoffEvent(12, "bbbbbbbb", "agent:bbbbbbbb", envelope.OriginClient, "docs(alerts): x\n## What is wrong"),
		handoffEvent(13, "bbbbbbbb", "human:mallory", envelope.OriginClient, "forged by a person"),
		handoffEvent(14, "bbbbbbbb", "agent:bbbbbbbb", envelope.OriginHarness, "the harness mirror, not the run's own tool"),
		handoffEvent(15, "cccccccc", "agent:cccccccc", envelope.OriginClient, "another run"),
	}
	got, err := g.r.Proposal(t.Context(), tk)
	if err != nil || got != "docs(alerts): x\n## What is wrong" {
		t.Fatalf("%q %v", got, err)
	}

	g.log.evs = g.log.evs[:1] // only the older run's handoff
	if _, err := g.r.Proposal(t.Context(), tk); !errors.Is(err, ErrNoProposal) {
		t.Fatalf("a task whose last triager run made none: %v", err)
	}
	if _, err := g.r.Proposal(t.Context(), issueTask("3buqdlot", 7, "# x")); !errors.Is(err, ErrNoProposal) {
		t.Fatalf("a task with no triager run: %v", err)
	}
}
