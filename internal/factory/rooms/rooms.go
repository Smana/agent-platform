// SPDX-License-Identifier: Apache-2.0

// Package rooms is the factory's side of SP2: one Room per task, named after it (R2), and the
// broker's system API on :8443 (TLS, GP-18), where the factory is system:factory with a token of
// audience rooms-system (SP2 ruling P3). The wire contract is internal/bridgeapi's roomEvents and
// roomMessage.
package rooms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// brokerActor is who writes a run's lifecycle into its room (runwatch.Events).
const brokerActor = "system:room-broker"

// ErrForeignRoom is a room of the task's name that the factory did not make for this task.
var ErrForeignRoom = errors.New("rooms: a room of that name exists and is not this task's")

// Ensure creates the task's room when absent: owned and driven by system:factory, with the
// unattended approval profile (SP2 §6), the task's data class and repository. An existing room is
// adopted only when it is exactly that: its runs inherit its data class (C3), so a task never
// writes into a room another owner, class or repository made.
func Ensure(ctx context.Context, c client.Client, ns, id, dataClass, repo string) error {
	if !envelope.ValidID(id) {
		return fmt.Errorf("rooms: %q is not a C2 id", id)
	}
	want := v1alpha1.RoomSpec{Owner: runs.PrincipalFactory, Driver: runs.PrincipalFactory, DataClass: dataClass,
		Repository: repo, Approvals: v1alpha1.Approvals{Profile: "unattended"}}
	room := &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: ns, Labels: map[string]string{runs.LabelTask: id}},
		Spec: want}
	err := c.Create(ctx, room)
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("rooms: create room %s: %w", id, err)
	}
	var got v1alpha1.Room
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: id}, &got); err != nil {
		return fmt.Errorf("rooms: read room %s: %w", id, err)
	}
	s := got.Spec
	if s.Owner != want.Owner || s.DataClass != want.DataClass || s.Repository != want.Repository ||
		s.Approvals.Profile != want.Approvals.Profile {
		return fmt.Errorf("%w: %s", ErrForeignRoom, id)
	}
	return nil
}

// HumanDriver is whether a human holds the room's driver token: the factory never advances such a
// room (C4).
func HumanDriver(r *v1alpha1.Room) bool { return strings.HasPrefix(r.Status.Driver, "human:") }

// LastRunEnd finds the broker's end event for a run (SP2 P15): the reason the run really ended,
// where the AgentRun only says Failed. Only the broker's own run_phase counts.
func LastRunEnd(evs []envelope.Event, runID string) (phase, reason string, ok bool) {
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		// The broker stamps the actor from the credential, so its id alone names the writer.
		if e.Type != envelope.StateChanged || e.RunID != runID || e.Origin != envelope.OriginBroker || e.Actor.ID != brokerActor {
			continue
		}
		var p struct{ Kind, Phase, Reason string }
		if json.Unmarshal(e.Payload, &p) == nil && p.Kind == "run_phase" && p.Reason != "" {
			return p.Phase, p.Reason, true
		}
	}
	return "", "", false
}

// commitRE is room_verdict's own commit rule.
var commitRE = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// Verdict is a reviewer or tester run's verdict. Text is the agent's, untrusted: a brief quotes it
// sanitised and fenced.
type Verdict struct {
	Verdict, Text, Commit, RunID string
	Seq                          int64
}

// LastVerdict is what the factory acts on after a reviewer or tester run (§3): that run's own
// newest verdict, recorded by the agent with room_verdict. Humans steer through GitHub reviews,
// never through a verdict in the room (owner, 2026-09-27, R36). The run is bound by what the broker
// stamps from the run's credential, never by the payload (ruling TB). A malformed newest verdict
// is no verdict: it never falls back to the one it replaced.
func LastVerdict(evs []envelope.Event, runID string) (Verdict, bool) {
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		if e.Type != envelope.Message || e.RunID != runID || e.Origin != envelope.OriginClient ||
			e.Actor.Kind != envelope.ActorAgent || e.Actor.ID != "agent:"+runID {
			continue
		}
		var p envelope.MessagePayload
		if json.Unmarshal(e.Payload, &p) != nil || p.Kind != envelope.KindReviewVerdict {
			continue
		}
		if (p.Verdict != "approve" && p.Verdict != "changes") || !commitRE.MatchString(p.Commit) {
			return Verdict{}, false
		}
		return Verdict{Verdict: p.Verdict, Text: p.Text, Commit: p.Commit, RunID: e.RunID, Seq: e.Seq}, true
	}
	return Verdict{}, false
}
