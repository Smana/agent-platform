// SPDX-License-Identifier: Apache-2.0

package summary_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/summary"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/*.json want fields from the current fold")

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
			if *updateGolden { // a flag run rewrites instead of asserting; the next plain run checks
				if err := rewriteWant("testdata/"+name+".json", tc.Want, got); err != nil {
					t.Fatal(err)
				}
				return
			}
			assertJSONEqual(t, tc.Want, got)
		})
	}
}

// rewriteWant replaces the file's trailing "want" member with got, indented, leaving every byte
// before it (events, viewer, now) untouched. A want that already equals got is not rewritten, so
// hand-formatting survives until the fold really changes. It writes nothing and errors when the
// file has any member after want, which the splice would drop.
func rewriteWant(path string, want json.RawMessage, got any) error {
	var w, g any
	var enc bytes.Buffer
	e := json.NewEncoder(&enc)
	e.SetEscapeHTML(false) // keep "<text>" readable in the golden
	if err := e.Encode(got); err != nil {
		return err
	}
	b := bytes.TrimSpace(enc.Bytes())
	if err := json.Unmarshal(want, &w); err != nil {
		return err
	}
	if err := json.Unmarshal(b, &g); err != nil {
		return err
	}
	if reflect.DeepEqual(w, g) {
		return nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	i := bytes.LastIndex(src, []byte("\n  \"want\":"))
	if i < 0 {
		return fmt.Errorf("%s: no top-level \"want\" member", path)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, b, "  ", "  "); err != nil {
		return err
	}
	next := string(src[:i]) + "\n  \"want\": " + out.String() + "\n}\n"
	if err := sameOtherMembers(src, []byte(next)); err != nil {
		return fmt.Errorf("%s: refusing to rewrite (want must be the last member): %w", path, err)
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// sameOtherMembers reports an error unless a and b hold the same top-level members apart from "want".
func sameOtherMembers(a, b []byte) error {
	var ma, mb map[string]json.RawMessage
	if err := json.Unmarshal(a, &ma); err != nil {
		return err
	}
	if err := json.Unmarshal(b, &mb); err != nil {
		return err
	}
	delete(ma, "want")
	delete(mb, "want")
	if len(ma) != len(mb) {
		return fmt.Errorf("members differ: %d before, %d after", len(ma), len(mb))
	}
	for k, va := range ma {
		var x, y any
		if err := json.Unmarshal(va, &x); err != nil {
			return err
		}
		if vb, ok := mb[k]; !ok || json.Unmarshal(vb, &y) != nil || !reflect.DeepEqual(x, y) {
			return fmt.Errorf("member %q lost or changed", k)
		}
	}
	return nil
}

func TestRewriteWantRefusesAMemberAfterWant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "g.json")
	const src = "{\n  \"events\": [],\n  \"want\": {\"a\": 1},\n  \"extra\": 2\n}\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rewriteWant(path, json.RawMessage(`{"a":1}`), map[string]int{"a": 2}); err == nil {
		t.Fatal("rewriteWant accepted a golden with a member after want")
	}
	if b, _ := os.ReadFile(path); string(b) != src {
		t.Fatalf("file was modified:\n%s", b)
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

// A fork's log is its source's prefix, then state_changed{forked_from}. From there the summary is the
// fork's own: none of the source's task, approvals, verdict or notes, and a closed source's seal does
// not close the fork. The fork's own facts count again.
func TestAForkStartsItsSummaryAfresh(t *testing.T) {
	broker := envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"}
	factory := envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"}
	source := []envelope.Event{
		ev(1, envelope.StateChanged, broker, envelope.StatePayload("room_phase", map[string]any{"phase": "Open"})),
		ev(2, envelope.StateChanged, factory, envelope.TaskStatePayload(envelope.TaskFacts{Phase: "Implementing",
			Issue: &envelope.IssueFact{Number: 42, URL: "https://github.com/Smana/cloud-native-ref/issues/42"}})),
		approvalRequested(3, "01M4A", "git push", time.Now().Add(time.Hour)),
		ev(4, envelope.Message, envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:cf4ato2x", Role: "reviewer"},
			envelope.MessagePayload{Kind: envelope.KindReviewVerdict, Text: "ok", Delivery: envelope.DeliveryNone, Verdict: "approve"}),
		note(5, "fixing line 12"),
		ev(6, envelope.StateChanged, broker, envelope.StatePayload("room_phase", map[string]any{"phase": "Closed", "reason": "done"})),
	}
	forked := ev(7, envelope.StateChanged, envelope.Actor{Kind: envelope.ActorHuman, ID: "human:o"},
		envelope.StatePayload("forked_from", map[string]any{"room": "src", "seq": 6}))
	s := summary.View(summary.Fold(slices.Concat(source, []envelope.Event{forked})), "r", "u", owner(), 0, time.Now())
	if s.Status.Phase != "Open" || s.Status.Issue != nil || s.Status.LastVerdict != nil || len(s.NeedsYou) != 0 ||
		len(s.Notes.Items) != 0 || s.Cursor != "seq:7" {
		t.Fatalf("the fork shows its source's state: %+v", s)
	}
	if kinds := actionKinds(s); !slices.Equal(kinds, []string{"queue"}) {
		t.Fatalf("actions %v: want the open fork's, and no stop for the source's issue", kinds)
	}
	own := ev(8, envelope.StateChanged, factory, envelope.TaskStatePayload(envelope.TaskFacts{Phase: "Queued"}))
	s = summary.View(summary.Fold(slices.Concat(source, []envelope.Event{forked, own})), "r", "u", owner(), 0, time.Now())
	if s.Status.Phase != "Queued" {
		t.Fatalf("phase %q: the fork's own facts count", s.Status.Phase)
	}
}

func actionKinds(s summary.Summary) []string {
	var kinds []string
	for _, a := range s.Actions {
		kinds = append(kinds, a.Kind)
	}
	return kinds
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

// N3: a factory task held by a person in its room needs whoever can release the hold — the driver
// or the owner — until the factory's next facts say the task moved on.
func TestAHumanHoldNeedsWhoeverCanReleaseIt(t *testing.T) {
	factory := envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"}
	facts := func(seq int64, phase, reason string) envelope.Event {
		return ev(seq, envelope.StateChanged, factory, envelope.TaskStatePayload(envelope.TaskFacts{Phase: phase, Reason: reason}))
	}
	for _, reason := range []string{"waiting_human_driver", "waiting_room_busy"} {
		st := summary.Fold([]envelope.Event{facts(1, "Queued", reason)})
		for name, sub := range map[string]policy.Subject{"driver": driverWeb(), "owner": owner()} {
			got := summary.View(st, "r", "u", sub, 0, time.Now()).NeedsYou
			if len(got) != 1 || got[0].Kind != "hold" || got[0].ID != reason || got[0].URL != "u" || got[0].What == "" {
				t.Fatalf("%s, %s: %+v", reason, name, got)
			}
		}
		if got := summary.View(st, "r", "u", watcher(), 0, time.Now()).NeedsYou; len(got) != 0 {
			t.Fatalf("%s: a watcher cannot release a hold: %+v", reason, got)
		}
		st.Add([]envelope.Event{facts(2, "Implementing", "")})
		if got := summary.View(st, "r", "u", driverWeb(), 0, time.Now()).NeedsYou; len(got) != 0 {
			t.Fatalf("%s: the hold ended: %+v", reason, got)
		}
	}
	other := summary.Fold([]envelope.Event{facts(1, "Queued", "waiting_run_slot")})
	if got := summary.View(other, "r", "u", owner(), 0, time.Now()).NeedsYou; len(got) != 0 {
		t.Fatalf("the factory's own waits need no one: %+v", got)
	}
}

func TestSteeringHasNoCLIAndIsWebOnly(t *testing.T) {
	running := summary.Fold([]envelope.Event{ev(1, envelope.Participant, envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"},
		map[string]any{"principal": "agent:cf4ato2x", "change": "joined"})})
	web := summary.View(running, "r", "u", driverWeb(), 0, time.Now())
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
	for _, a := range summary.View(running, "r", "u", cli, 0, time.Now()).Actions {
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

// A sealed room refuses new facts, so its last wait reason is history, not a status.
func TestASealedRoomCarriesNoWaitingLine(t *testing.T) {
	factory := envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"}
	broker := envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"}
	evs := []envelope.Event{
		ev(1, envelope.StateChanged, factory, envelope.TaskStatePayload(envelope.TaskFacts{Phase: "AwaitingHuman", Reason: "policy_pending"})),
		ev(2, envelope.StateChanged, broker, envelope.StatePayload("room_phase", map[string]any{"phase": "Closed", "reason": "owner"})),
	}
	s := summary.View(summary.Fold(evs), "r", "u", owner(), 0, time.Now())
	if s.Status.Reason != "" || s.Status.Next != "" {
		t.Fatalf("a sealed room waits: reason %q next %q", s.Status.Reason, s.Status.Next)
	}
}

// Steering needs something to steer now: an agent that joined and has not left. Facts naming a run
// do not count: they outlive the run (Q2).
func TestSteerNeedsARunningRun(t *testing.T) {
	broker := envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"}
	join := ev(1, envelope.Participant, broker, map[string]any{"principal": "agent:cf4ato2x", "change": "joined", "role": "implementer"})
	leave := ev(2, envelope.Participant, broker, map[string]any{"principal": "agent:cf4ato2x", "change": "left"})
	join2 := ev(3, envelope.Participant, broker, map[string]any{"principal": "agent:b7reviewr", "change": "joined", "role": "reviewer"})
	leave2 := ev(4, envelope.Participant, broker, map[string]any{"principal": "agent:b7reviewr", "change": "left"})
	roleChanged := ev(5, envelope.Participant, broker, map[string]any{"principal": "agent:cf4ato2x", "change": "role_changed", "role": "reviewer"})
	cases := []struct {
		name string
		evs  []envelope.Event
		want bool
	}{
		{"empty room", nil, false},
		{"agent joined", []envelope.Event{join}, true},
		{"agent left", []envelope.Event{join, leave}, false},
		{"facts name a run but no agent present", []envelope.Event{ev(3, envelope.StateChanged, envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"},
			envelope.TaskStatePayload(envelope.TaskFacts{Phase: "Implementing", Run: &envelope.RunFact{ID: "cf4ato2x", Role: "implementer"}}))}, false},
		{"one of two agents left", []envelope.Event{join, join2, leave2}, true},
		{"both agents left", []envelope.Event{join, join2, leave, leave2}, false},
		{"a role change is no join", []envelope.Event{join, leave, roleChanged}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := slices.Contains(actionKinds(summary.View(summary.Fold(tc.evs), "r", "u", driverWeb(), 0, time.Now())), "steer")
			if got != tc.want {
				t.Fatalf("steer offered = %v, want %v", got, tc.want)
			}
		})
	}
}

// Scoper design SC8: a bounced task shows the room's owner an answer need naming the issue's author.
func TestABouncedTaskNeedsAnAnswer(t *testing.T) {
	f := &envelope.TaskFacts{Phase: "Escalated", Reason: "needs_author",
		Issue: &envelope.IssueFact{Number: 7, URL: "https://github.com/Smana/cloud-native-ref/issues/7", Author: "octocat"}}
	n, ok := summary.Answer(f, owner())
	if !ok || n.Kind != "answer" || n.ID != "7" || !strings.Contains(n.What, "@octocat") || n.URL != f.Issue.URL {
		t.Fatalf("%+v %v", n, ok)
	}
	if _, ok := summary.Answer(&envelope.TaskFacts{Phase: "Escalated", Reason: "stuck", Issue: f.Issue}, owner()); ok {
		t.Fatal("only needs_author asks for an answer")
	}
}
