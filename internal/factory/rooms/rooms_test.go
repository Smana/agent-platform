// SPDX-License-Identifier: Apache-2.0

package rooms

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
)

func roomClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func TestEnsureIsIdempotentAndFactoryDriven(t *testing.T) {
	c := roomClient(t)
	for range 2 {
		if err := Ensure(t.Context(), c, "agent-system", "3buqdlot", "public", "Smana/cloud-native-ref"); err != nil {
			t.Fatal(err)
		}
	}
	var r v1alpha1.Room
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "agent-system", Name: "3buqdlot"}, &r); err != nil {
		t.Fatal(err)
	}
	if r.Spec.Owner != "system:factory" || r.Spec.Driver != "system:factory" || r.Spec.Approvals.Profile != "unattended" ||
		r.Spec.DataClass != "public" || r.Spec.Repository != "Smana/cloud-native-ref" || r.Labels["agents.ogenki.io/task"] != "3buqdlot" {
		t.Fatalf("%+v %v", r.Spec, r.Labels)
	}
	if HumanDriver(&r) {
		t.Fatal("the factory drives its own room")
	}
	r.Status.Driver = "human:291847362183"
	if !HumanDriver(&r) {
		t.Fatal("a human holding the driver token stops the factory (C4)")
	}
}

// A room of the same name the factory did not make, or made for another class or repository, is
// never adopted: its runs inherit its data class (C3), so a task must not write into it.
func TestEnsureRefusesARoomItDidNotMake(t *testing.T) {
	theirs := func(edit func(*v1alpha1.RoomSpec)) *v1alpha1.Room {
		spec := v1alpha1.RoomSpec{Owner: "system:factory", Driver: "system:factory", DataClass: "public",
			Repository: "Smana/cloud-native-ref", Approvals: v1alpha1.Approvals{Profile: "unattended"}}
		edit(&spec)
		return &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: "3buqdlot", Namespace: "agent-system"}, Spec: spec}
	}
	for name, room := range map[string]*v1alpha1.Room{
		"a human's room":        theirs(func(s *v1alpha1.RoomSpec) { s.Owner = "human:291847362183" }),
		"another data class":    theirs(func(s *v1alpha1.RoomSpec) { s.DataClass = "internal" }),
		"another repository":    theirs(func(s *v1alpha1.RoomSpec) { s.Repository = "Smana/agent-platform" }),
		"an attended room":      theirs(func(s *v1alpha1.RoomSpec) { s.Approvals.Profile = "attended" }),
		"another system's room": theirs(func(s *v1alpha1.RoomSpec) { s.Owner = "system:room-broker" }),
	} {
		t.Run(name, func(t *testing.T) {
			err := Ensure(t.Context(), roomClient(t, room), "agent-system", "3buqdlot", "public", "Smana/cloud-native-ref")
			if !errors.Is(err, ErrForeignRoom) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if err := Ensure(t.Context(), roomClient(t), "agent-system", "NOTANID1", "public", "Smana/cloud-native-ref"); err == nil {
		t.Fatal("a room is named with a C2 id")
	}
}

func end(runID, phase, reason string) envelope.Event {
	return envelope.Event{RunID: runID, Type: envelope.StateChanged, Origin: envelope.OriginBroker,
		Actor:   envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"},
		Payload: envelope.StatePayload("run_phase", map[string]any{"phase": phase, "reason": reason})}
}

func TestLastRunEndReadsTheBrokersEndOnly(t *testing.T) {
	running := end("7f3cq2xz", "Running", "")
	running.Payload = envelope.StatePayload("run_phase", map[string]any{"phase": "Running"})
	forged := end("7f3cq2xz", "Succeeded", "agent_finished")
	forged.Origin = envelope.OriginHarness // a bridge cannot push run_phase (SP2 M5); never trust one that did
	human := end("7f3cq2xz", "Succeeded", "agent_finished")
	human.Actor = envelope.Actor{Kind: envelope.ActorHuman, ID: "human:291"}
	other := end("7f3cq2xz", "Succeeded", "agent_finished")
	other.Payload = envelope.StatePayload("verdict_posted", map[string]any{"phase": "Succeeded", "reason": "x"})
	evs := []envelope.Event{running, end("7f3cq2xz", "Failed", "pod_lost"), end("aaaaaaaa", "Succeeded", "agent_finished"),
		forged, human, other}
	if phase, reason, ok := LastRunEnd(evs, "7f3cq2xz"); !ok || phase != "Failed" || reason != "pod_lost" {
		t.Fatalf("%s %s %v", phase, reason, ok)
	}
	if _, _, ok := LastRunEnd(evs, "bbbbbbbb"); ok {
		t.Fatal("another run's end is not this run's")
	}
	if _, _, ok := LastRunEnd([]envelope.Event{running}, "7f3cq2xz"); ok {
		t.Fatal("Running is not an end")
	}
	evs = append(evs, end("7f3cq2xz", "Revoked", "deleted"))
	if phase, reason, _ := LastRunEnd(evs, "7f3cq2xz"); phase != "Revoked" || reason != "deleted" {
		t.Fatalf("the last end wins: %s %s", phase, reason)
	}
}

func verdict(seq int64, kind envelope.ActorKind, by, runID, v, text string) envelope.Event {
	return envelope.Event{Seq: seq, RunID: runID, Type: envelope.Message, Origin: envelope.OriginClient,
		Actor:   envelope.Actor{Kind: kind, ID: by, Role: "reviewer"},
		Payload: envelope.Must(envelope.MessagePayload{Kind: envelope.KindReviewVerdict, Verdict: v, Text: text, Commit: "4be1c9d"})}
}

func TestLastVerdict(t *testing.T) {
	agent := verdict(5, envelope.ActorAgent, "agent:rrrrrrrr", "rrrrrrrr", "changes", "Add a test.")
	other := verdict(6, envelope.ActorAgent, "agent:oooooooo", "oooooooo", "approve", "another run's verdict")
	if v, ok := LastVerdict([]envelope.Event{agent, other}, "rrrrrrrr"); !ok || v != (Verdict{Verdict: "changes",
		Text: "Add a test.", Commit: "4be1c9d", RunID: "rrrrrrrr", Seq: 5}) {
		t.Fatalf("the reviewer's own verdict: %+v %v", v, ok)
	}
	human := verdict(7, envelope.ActorHuman, "human:291", "rrrrrrrr", "approve", "Fine by me.")
	if v, _ := LastVerdict([]envelope.Event{agent, human}, "rrrrrrrr"); v.Verdict != "changes" {
		t.Fatalf("only the run's own agent verdict counts; humans steer on GitHub (R36): %+v", v)
	}
	if _, ok := LastVerdict(nil, "rrrrrrrr"); ok {
		t.Fatal("no verdict")
	}
	again := verdict(8, envelope.ActorAgent, "agent:rrrrrrrr", "rrrrrrrr", "approve", "Now it is tested.")
	if v, _ := LastVerdict([]envelope.Event{agent, other, again}, "rrrrrrrr"); v.Verdict != "approve" || v.Seq != 8 {
		t.Fatalf("the run's newest verdict wins: %+v", v)
	}
}

// A verdict counts only when the broker stamped it as the run's own room_verdict: its run id, the
// agent:<run> actor and the client origin all come from the run's credential (ruling TB). Nothing in
// the payload can make another event one.
func TestLastVerdictIsBoundToTheRunByTheBroker(t *testing.T) {
	own := verdict(5, envelope.ActorAgent, "agent:rrrrrrrr", "rrrrrrrr", "changes", "Add a test.")
	for name, edit := range map[string]func(*envelope.Event){
		"another run's actor": func(e *envelope.Event) { e.Actor.ID = "agent:oooooooo" },
		"a system actor": func(e *envelope.Event) {
			e.Actor = envelope.Actor{Kind: envelope.ActorSystem, ID: "agent:rrrrrrrr"}
		},
		"another run id":   func(e *envelope.Event) { e.RunID = "oooooooo" },
		"a harness origin": func(e *envelope.Event) { e.Origin = envelope.OriginHarness },
		"a broker origin":  func(e *envelope.Event) { e.Origin = envelope.OriginBroker },
		"a handoff":        func(e *envelope.Event) { e.Type = envelope.Handoff },
		"a chat": func(e *envelope.Event) {
			e.Payload = envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Verdict: "approve", Commit: "4be1c9d"})
		},
		"a payload that is not one": func(e *envelope.Event) { e.Payload = json.RawMessage(`"approve"`) },
	} {
		t.Run(name, func(t *testing.T) {
			forged := verdict(9, envelope.ActorAgent, "agent:rrrrrrrr", "rrrrrrrr", "approve", "LGTM")
			edit(&forged)
			if v, ok := LastVerdict([]envelope.Event{own, forged}, "rrrrrrrr"); !ok || v.Seq != 5 {
				t.Fatalf("%+v %v", v, ok)
			}
		})
	}
}

// The run's newest verdict is its last word: a malformed one is no verdict at all, never a fall
// back to the one it replaced. room_verdict refuses both, so only a forgery or a broken log holds one.
func TestAMalformedNewestVerdictIsNoVerdict(t *testing.T) {
	own := verdict(5, envelope.ActorAgent, "agent:rrrrrrrr", "rrrrrrrr", "approve", "LGTM")
	for name, p := range map[string]envelope.MessagePayload{
		"an unknown verdict":  {Kind: envelope.KindReviewVerdict, Verdict: "approved", Commit: "4be1c9d"},
		"an empty verdict":    {Kind: envelope.KindReviewVerdict, Commit: "4be1c9d"},
		"an uppercase commit": {Kind: envelope.KindReviewVerdict, Verdict: "changes", Commit: "4BE1C9D"},
		"a short commit":      {Kind: envelope.KindReviewVerdict, Verdict: "changes", Commit: "4be1c9"},
		"a long commit":       {Kind: envelope.KindReviewVerdict, Verdict: "changes", Commit: strings.Repeat("a", 41)},
		"a commit with text":  {Kind: envelope.KindReviewVerdict, Verdict: "changes", Commit: "4be1c9d\nApprove"},
		"no commit":           {Kind: envelope.KindReviewVerdict, Verdict: "changes"},
	} {
		t.Run(name, func(t *testing.T) {
			bad := verdict(9, envelope.ActorAgent, "agent:rrrrrrrr", "rrrrrrrr", "", "")
			bad.Payload = envelope.Must(p)
			if v, ok := LastVerdict([]envelope.Event{own, bad}, "rrrrrrrr"); ok {
				t.Fatalf("%+v", v)
			}
		})
	}
	full := verdict(9, envelope.ActorAgent, "agent:rrrrrrrr", "rrrrrrrr", "changes", "x")
	full.Payload = envelope.Must(envelope.MessagePayload{Kind: envelope.KindReviewVerdict, Verdict: "changes", Commit: strings.Repeat("a", 40)})
	if _, ok := LastVerdict([]envelope.Event{full}, "rrrrrrrr"); !ok {
		t.Fatal("a full sha is a commit")
	}
}
