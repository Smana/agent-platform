// SPDX-License-Identifier: Apache-2.0

package summary_test

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/summary"
)

var goldens = []string{"normal", "resumed", "expired_approval", "sealed", "no_facts"}

func TestFoldGolden(t *testing.T) {
	for _, name := range goldens {
		t.Run(name, func(t *testing.T) {
			var tc struct {
				Events []envelope.Event `json:"events"`
				Viewer struct {
					Role     string `json:"role"`
					Approver bool   `json:"approver"`
					Driver   bool   `json:"driver"`
					WebUI    bool   `json:"webUI"`
				} `json:"viewer"`
				Now  time.Time       `json:"now"`
				Want json.RawMessage `json:"want"`
			}
			readJSON(t, "testdata/"+name+".json", &tc)
			v := policy.Subject{Kind: envelope.ActorHuman, Role: policy.ParseRole(tc.Viewer.Role),
				Approver: tc.Viewer.Approver, Driver: tc.Viewer.Driver, WebUI: tc.Viewer.WebUI}
			got := summary.View(summary.Fold(tc.Events), "26zfnuxm", "https://rooms.example/r/26zfnuxm", v, 0, tc.Now)
			assertJSONEqual(t, tc.Want, got)
		})
	}
}

// The summary route folds the log a page at a time and drops each page, so its memory is one page:
// every split of a log folds to the state of the whole log.
func TestAddingPagesIsFoldingTheLog(t *testing.T) {
	logs := map[string][]envelope.Event{}
	for _, name := range goldens {
		var tc struct {
			Events []envelope.Event `json:"events"`
		}
		readJSON(t, "testdata/"+name+".json", &tc)
		logs[name] = tc.Events
	}
	for i := int64(1); i <= 50; i++ { // past the notes' window, so it slides across pages
		logs["notes"] = append(logs["notes"], note(i, fmt.Sprint("n", i)))
	}
	for name, evs := range logs {
		want := summary.Fold(evs)
		for size := 1; size <= len(evs); size++ {
			var st summary.State
			for page := range slices.Chunk(evs, size) {
				st.Add(page)
			}
			if !reflect.DeepEqual(st, want) {
				t.Fatalf("%s in pages of %d:\ngot  %+v\nwant %+v", name, size, st, want)
			}
		}
	}
}

// A room sealed by MaxEvents ends with state_changed{limit}, not room_phase: both seal.
func TestTheLimitSealAlsoClosesTheRoom(t *testing.T) {
	broker := envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"}
	evs := []envelope.Event{
		ev(2, envelope.StateChanged, envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"},
			envelope.TaskStatePayload(envelope.TaskFacts{Phase: "Implementing",
				Issue: &envelope.IssueFact{Number: 42, URL: "https://github.com/Smana/cloud-native-ref/issues/42"}})),
		approvalRequested(5, "01M4A", "git push", time.Now().Add(time.Hour)),
		ev(6, envelope.StateChanged, broker, envelope.StatePayload("limit", map[string]any{"events": 10000, "bytes": 1})),
	}
	s := summary.View(summary.Fold(evs), "r", "u", owner(), 0, time.Now())
	if len(s.NeedsYou) != 0 || len(s.Actions) != 0 {
		t.Fatalf("a sealed room offers needs %+v / actions %+v", s.NeedsYou, s.Actions)
	}
	if s.Status.Phase != "Sealed" || s.Status.Issue == nil || s.Status.Issue.Number != 42 {
		t.Fatalf("status %+v: wants phase Sealed and the task's issue kept", s.Status)
	}
}

// A concurrent-run refusal is also state_changed{limit}, but it seals nothing.
func TestAConcurrentRunLimitDoesNotSeal(t *testing.T) {
	evs := []envelope.Event{ev(6, envelope.StateChanged, envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"},
		envelope.StatePayload("limit", map[string]any{"reason": "concurrent_run", "running": "x"}))}
	if s := summary.View(summary.Fold(evs), "r", "u", owner(), 0, time.Now()); len(s.Actions) == 0 {
		t.Fatal("a concurrent-run refusal sealed the room")
	}
}

func TestNotesAfterTheCursor(t *testing.T) {
	evs := []envelope.Event{note(10, "planning"), note(20, "edited line 12"), note(30, "checks pass")}
	s := summary.View(summary.Fold(evs), "r", "u", watcher(), 20, time.Now())
	if len(s.Notes.Items) != 1 || s.Notes.Items[0].Text != "checks pass" || s.Cursor != "seq:30" || !s.Notes.Untrusted {
		t.Fatalf("%+v", s.Notes)
	}
}

func TestAWatcherSeesNoActions(t *testing.T) {
	s := summary.View(summary.Fold(nil), "r", "u", watcher(), 0, time.Now())
	if len(s.Actions) != 0 {
		t.Fatalf("watcher actions %+v", s.Actions)
	}
}

func TestApprovalsNeedAnApproverAndCarryNoCommand(t *testing.T) {
	evs := []envelope.Event{approvalRequested(5, "01M4A", "git push", time.Now().Add(time.Hour))}
	st := summary.Fold(evs)
	if got := summary.View(st, "r", "u", watcher(), 0, time.Now()).NeedsYou; len(got) != 0 {
		t.Fatalf("a watcher is asked to approve: %+v", got)
	}
	got := summary.View(st, "r", "u", approverCLI(), 0, time.Now()).NeedsYou
	if len(got) != 1 || got[0].URL != "u#01M4A" || got[0].What != "git push" {
		t.Fatalf("approval need %+v: wants a link to the room (D4)", got)
	}
	b, _ := json.Marshal(got[0])
	if strings.Contains(string(b), "cli") {
		t.Fatalf("an approval need carries a command: %s", b)
	}
}

func TestSteeringHasNoCLIAndIsWebOnly(t *testing.T) {
	web := summary.View(summary.Fold(nil), "r", "u", driverWeb(), 0, time.Now())
	steer := false
	for _, a := range web.Actions {
		if a.Kind == "steer" {
			steer = true
			if a.CLI != "" {
				t.Fatalf("steer carries a command: %+v", a)
			}
		}
	}
	if !steer {
		t.Fatal("the web driver is not offered steering")
	}
	cli := driverWeb()
	cli.WebUI = false
	for _, a := range summary.View(summary.Fold(nil), "r", "u", cli, 0, time.Now()).Actions {
		if a.Kind == "steer" {
			t.Fatal("a CLI caller is offered steering")
		}
	}
}

func TestNotesAreBounded(t *testing.T) {
	var evs []envelope.Event
	for i := int64(1); i <= 50; i++ {
		evs = append(evs, note(i, "n"))
	}
	if n := len(summary.View(summary.Fold(evs), "r", "u", watcher(), 0, time.Now()).Notes.Items); n != 20 {
		t.Fatalf("%d notes", n)
	}
}

func ev(seq int64, typ envelope.Type, actor envelope.Actor, payload any) envelope.Event {
	p, ok := payload.(json.RawMessage)
	if !ok {
		p = envelope.Must(payload)
	}
	return envelope.Event{RoomID: "r", Seq: seq, Type: typ, Actor: actor, RunID: "cf4ato2x",
		TS: time.Unix(1_760_000_000+seq, 0).UTC(), Payload: p}
}

func note(seq int64, text string) envelope.Event {
	return ev(seq, envelope.Message, envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:cf4ato2x", Role: "implementer"},
		envelope.MessagePayload{Kind: envelope.KindProgress, Text: text, Delivery: envelope.DeliveryNone})
}

func approvalRequested(seq int64, id, command string, expires time.Time) envelope.Event {
	return ev(seq, envelope.ApprovalRequested, envelope.Actor{Kind: envelope.ActorSystem, ID: "system:policy"},
		envelope.ApprovalRequestedPayload{ApprovalID: id, CallID: "c1", Class: "forge.pr",
			Action: envelope.Must(map[string]string{"command": command}), ExpiresAt: expires})
}

func watcher() policy.Subject {
	return policy.Subject{Kind: envelope.ActorHuman, ID: "human:w", Role: policy.Watcher}
}

func owner() policy.Subject {
	return policy.Subject{Kind: envelope.ActorHuman, ID: "human:o", Role: policy.Owner, WebUI: true}
}

func driverWeb() policy.Subject {
	return policy.Subject{Kind: envelope.ActorHuman, ID: "human:d", Role: policy.Collaborator, Driver: true, WebUI: true}
}

// approverCLI is an approver on a CLI token: it may not decide there, but must learn it is needed.
func approverCLI() policy.Subject {
	return policy.Subject{Kind: envelope.ActorHuman, ID: "human:a", Role: policy.Collaborator, Approver: true}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func assertJSONEqual(t *testing.T, want json.RawMessage, got any) {
	t.Helper()
	var w, g any
	b, _ := json.Marshal(got)
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(b, &g)
	if !reflect.DeepEqual(w, g) {
		t.Fatalf("summary mismatch\nwant %s\ngot  %s", want, b)
	}
}
