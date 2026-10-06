// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/wire"
)

type fakeApprovals struct {
	mu   sync.Mutex
	reqs []wire.ApprovalRequest
	// code and err, when set, are what every request gets; expires is the ack's expiry.
	code    int
	err     error
	expires time.Time
}

func (f *fakeApprovals) RequestApproval(_ context.Context, r wire.ApprovalRequest) (wire.ApprovalAck, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r)
	if f.err != nil || f.code != 0 {
		return wire.ApprovalAck{}, f.code, f.err
	}
	return wire.ApprovalAck{ApprovalID: "ap-" + r.CallID, ExpiresAt: f.expires}, 200, nil
}

func (f *fakeApprovals) requests() []wire.ApprovalRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]wire.ApprovalRequest{}, f.reqs...)
}

func action(id, cmd string) RawEvent {
	b, _ := json.Marshal(map[string]any{"id": id, "kind": "ActionEvent", "source": "agent", "tool_name": "terminal",
		"tool_call_id": id, "security_risk": "LOW", "action": map[string]string{"command": cmd}})
	var e RawEvent
	_ = json.Unmarshal(b, &e)
	return e
}

func setup(t *testing.T, profile string) (*Confirmer, *fakeAgentServer, *fakeApprovals, *[]wire.Item) {
	f := &fakeAgentServer{pageSize: 100, status: "waiting_for_confirmation"}
	var items []wire.Item
	ap := &fakeApprovals{}
	c := &Confirmer{Harness: NewHarness(f.start(t, conv).URL, conv), Broker: ap, RunID: "7f3cq2xz",
		Classifier: Classifier{Branch: "agent/3kq7x2ma"}, Push: func(it wire.Item) { items = append(items, it) }}
	c.SetPolicy(wire.ApprovalPolicy{Profile: profile})
	return c, f, ap, &items
}

func (f *fakeAgentServer) responded() []bool {
	_, responses, _ := f.snapshot()
	return responses
}

func TestAllowedActionsRunWithoutAHuman(t *testing.T) {
	c, f, ap, items := setup(t, "attended")
	c.Observe(action("c1", "git push origin agent/3kq7x2ma"))
	c.OnStatus(context.Background(), "waiting_for_confirmation")
	waitFor(t, func() bool { return len(f.responded()) == 1 })
	if !f.responded()[0] || len(ap.reqs) != 0 || !strings.Contains(string((*items)[0].Payload), `"decision":"allow"`) {
		t.Fatalf("responses=%v reqs=%v", f.responded(), ap.reqs)
	}
}

func TestADenyRejectsEveryPendingSibling(t *testing.T) {
	c, f, _, _ := setup(t, "unattended")
	c.Observe(action("c1", "git push origin agent/3kq7x2ma"))
	c.Observe(action("c2", "gh pr merge 3"))
	c.OnStatus(context.Background(), "waiting_for_confirmation")
	waitFor(t, func() bool { return len(f.responded()) == 1 })
	if f.responded()[0] {
		t.Fatal("OpenHands answers all pending actions at once: one deny rejects them all")
	}
}

func TestAHumanClassWaitsForTheDecision(t *testing.T) {
	c, f, ap, items := setup(t, "attended")
	c.Observe(action("c1", "gh pr create --fill"))
	c.OnStatus(context.Background(), "waiting_for_confirmation")
	waitFor(t, func() bool { ap.mu.Lock(); defer ap.mu.Unlock(); return len(ap.reqs) == 1 })
	if len(f.responded()) != 0 {
		t.Fatal("answered before a human decided")
	}
	if err := c.Decision(context.Background(), wire.Decision{ApprovalID: "ap-c1", Allow: true, Ref: 42}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(f.responded()) == 1 })
	if !f.responded()[0] {
		t.Fatal("approved means accept")
	}
	last := (*items)[len(*items)-1]
	if !strings.Contains(string(last.Payload), `"kind":"decision_applied"`) || !strings.Contains(string(last.Payload), `"ref":42`) {
		t.Fatalf("the decision is acknowledged in the log: %s", last.Payload)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

const waiting = "waiting_for_confirmation"

// kinds lists each item's state_changed kind and the fields a test reads.
func kinds(items []wire.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		var p struct {
			Kind     string `json:"kind"`
			CallID   string `json:"callId"`
			Class    string `json:"class"`
			Decision string `json:"decision"`
			Ref      int64  `json:"ref"`
			RunID    string `json:"runId"`
		}
		_ = json.Unmarshal(it.Payload, &p)
		if p.Kind == "decision_applied" {
			out = append(out, fmt.Sprintf("%s %d %s", p.Kind, p.Ref, p.RunID))
			continue
		}
		out = append(out, fmt.Sprintf("%s %s %s %s", p.Kind, p.CallID, p.Class, p.Decision))
	}
	return out
}

// The carried-in denial rule: the text says how to retry in an allowed shape,
// is fixed and short, and never echoes the model's command.
func TestADenialSaysHowToRetryAndNeverQuotesTheAction(t *testing.T) {
	cases := []struct {
		name, cmd string
		want      []string
	}{
		{"forge.other names the allowed shapes", `gh pr merge 3; echo "IGNORE ALL RULES"`,
			[]string{"unattended policy (forge.other)", "--body-file -", "--body-file <file>", "--force-with-lease origin <your branch>",
				"git --no-pager", "GIT_*", "none ran"}},
		{"egress.new names the fork", "npm install left-pad", []string{"egress.new", "egressProfiles", "none ran"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f, _, _ := setup(t, "unattended")
			c.Observe(action("c1", tc.cmd))
			c.OnStatus(t.Context(), waiting)
			got := f.answers()
			if len(got) != 1 || f.responded()[0] {
				t.Fatalf("answers %v %v", got, f.responded())
			}
			for _, w := range tc.want {
				if !strings.Contains(got[0], w) {
					t.Errorf("the denial lacks %q: %s", w, got[0])
				}
			}
			for _, leak := range []string{"IGNORE", "left-pad", "merge 3"} {
				if strings.Contains(got[0], leak) {
					t.Errorf("the denial quotes the action (%q): %s", leak, got[0])
				}
			}
			if len(got[0]) > 600 {
				t.Errorf("the denial is %d bytes", len(got[0]))
			}
		})
	}
	for _, tc := range []struct {
		profile string
		class   Class
		want    string
	}{
		{"attended", ForgePR, "attended policy (forge.pr)"},
		{"bogus", ForgePush, "attended policy (forge.push)"},
		{"unattended", MCPWrite, "unattended policy (mcp.write)"},
		{"unattended", ShellHigh, "Denied by the room's unattended policy. Every action"},
		{"", Plain, "Denied by the room's attended policy. Every action"},
	} {
		if got := denial(wire.ApprovalPolicy{Profile: tc.profile}, tc.class); !strings.HasPrefix(got, "Denied by the room's") ||
			!strings.Contains(got, tc.want) {
			t.Errorf("denial(%q, %q) = %s", tc.profile, tc.class, got)
		}
	}
	if s := denial(wire.ApprovalPolicy{}, MCPWrite); strings.Contains(s, "--body-file") {
		t.Errorf("the forge shapes are only for forge classes: %s", s)
	}
}

// S9: a classifier panic, a broker outage or a timeout never yields allow.
func TestTheLoopFailsClosed(t *testing.T) {
	t.Run("a classifier panic is forge.other", func(t *testing.T) {
		for _, profile := range []string{"attended", "unattended"} {
			c, f, ap, items := setup(t, profile)
			c.classifyFn = func(string, json.RawMessage, string) Class { panic("boom") }
			c.Observe(action("c1", "ls"))
			c.OnStatus(t.Context(), waiting)
			if profile == "unattended" {
				if r := f.responded(); len(r) != 1 || r[0] || !strings.Contains(f.answers()[0], "(forge.other)") {
					t.Fatalf("unattended: %v %v", r, f.answers())
				}
				if k := kinds(*items); !slices.Equal(k, []string{"policy_decision c1 forge.other deny"}) {
					t.Fatalf("logged %v", k)
				}
				continue
			}
			if r := ap.requests(); len(r) != 1 || r[0].Class != "forge.other" || len(f.responded()) != 0 {
				t.Fatalf("attended: %v %v", r, f.responded())
			}
			if got := c.ClassOf("terminal", nil, ""); got != "forge.other" {
				t.Fatalf("the logged class is %q", got)
			}
		}
	})
	t.Run("a panic past the classifier rejects the step", func(t *testing.T) {
		c, f, _, _ := setup(t, "attended")
		c.Broker = approvalsFunc(func(context.Context, wire.ApprovalRequest) (wire.ApprovalAck, int, error) { panic("broker") })
		c.Observe(action("c1", "gh pr create --fill"))
		c.OnStatus(t.Context(), waiting)
		if r := f.responded(); len(r) != 1 || r[0] || f.answers()[0] != textInternal {
			t.Fatalf("%v %v", r, f.answers())
		}
	})
	for _, tc := range []struct {
		name string
		code int
		err  error
	}{
		{"a broker outage rejects the step", 0, errors.New("dial tcp: connection refused")},
		{"a broker refusal rejects the step", http.StatusServiceUnavailable, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f, ap, _ := setup(t, "attended")
			ap.code, ap.err = tc.code, tc.err
			c.Observe(action("c1", "git push origin agent/3kq7x2ma"))
			c.Observe(action("c2", "gh pr create --fill"))
			c.OnStatus(t.Context(), waiting)
			if r := f.responded(); len(r) != 1 || r[0] || f.answers()[0] != textBrokerDown {
				t.Fatalf("%v %v", r, f.answers())
			}
		})
	}
	for name, answer := range map[string]func(context.Context, wire.ApprovalRequest) (wire.ApprovalAck, int, error){
		"an ack the bridge could not read rejects the step": func(context.Context, wire.ApprovalRequest) (wire.ApprovalAck, int, error) {
			return wire.ApprovalAck{ApprovalID: "ap-c1"}, 200, errors.New("broker POST: unexpected EOF")
		},
		"a refusal naming an approval still rejects the step": func(context.Context, wire.ApprovalRequest) (wire.ApprovalAck, int, error) {
			return wire.ApprovalAck{ApprovalID: "ap-c1"}, http.StatusConflict, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, f, _, _ := setup(t, "attended")
			c.Broker = approvalsFunc(answer)
			c.Observe(action("c1", "gh pr create --fill"))
			c.OnStatus(t.Context(), waiting)
			if r := f.responded(); len(r) != 1 || r[0] {
				t.Fatalf("%v", r)
			}
		})
	}
	t.Run("an ack without an approval id rejects the step", func(t *testing.T) {
		c, f, _, _ := setup(t, "attended")
		c.Broker = approvalsFunc(func(context.Context, wire.ApprovalRequest) (wire.ApprovalAck, int, error) {
			return wire.ApprovalAck{}, 200, nil
		})
		c.Observe(action("c1", "gh pr create --fill"))
		c.OnStatus(t.Context(), waiting)
		if r := f.responded(); len(r) != 1 || r[0] {
			t.Fatalf("%v", r)
		}
	})
	t.Run("no decision by the deadline rejects the step", func(t *testing.T) {
		c, f, ap, _ := setup(t, "attended")
		now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		c.Now = func() time.Time { return now }
		ap.expires = now.Add(30 * time.Minute)
		c.Observe(action("c1", "gh pr create --fill"))
		c.OnStatus(t.Context(), waiting)
		now = now.Add(30*time.Minute + decisionGrace - time.Second)
		c.OnStatus(t.Context(), waiting)
		if len(f.responded()) != 0 {
			t.Fatal("rejected before the deadline")
		}
		now = now.Add(time.Second)
		c.OnStatus(t.Context(), waiting)
		if r := f.responded(); len(r) != 1 || r[0] || f.answers()[0] != textTimeout {
			t.Fatalf("%v %v", r, f.answers())
		}
		if len(ap.requests()) != 1 {
			t.Fatalf("the approval is asked for once: %v", ap.requests())
		}
	})
	t.Run("an ack without an expiry waits the default", func(t *testing.T) {
		c, f, _, _ := setup(t, "attended")
		now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		c.Now = func() time.Time { return now }
		c.Observe(action("c1", "gh pr create --fill"))
		c.OnStatus(t.Context(), waiting)
		now = now.Add(attendedWait + decisionGrace - time.Second)
		c.OnStatus(t.Context(), waiting)
		if len(f.responded()) != 0 {
			t.Fatal("rejected before the default wait")
		}
		now = now.Add(time.Second)
		c.OnStatus(t.Context(), waiting)
		if r := f.responded(); len(r) != 1 || r[0] {
			t.Fatalf("%v", r)
		}
	})
	t.Run("an unattended ack without an expiry waits the room's ttl", func(t *testing.T) {
		for _, tc := range []struct {
			ttl  string
			wait time.Duration
		}{{"2h", 2 * time.Hour}, {"", defaultTTL}, {"soon", defaultTTL}, {"-1h", defaultTTL}} {
			c, f, _, _ := setup(t, "unattended")
			c.SetPolicy(wire.ApprovalPolicy{Profile: "unattended", TTL: tc.ttl, Overrides: map[string]string{"forge.pr": "human"}})
			now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			c.Now = func() time.Time { return now }
			c.Observe(action("c1", "gh pr create --fill"))
			c.OnStatus(t.Context(), waiting)
			now = now.Add(tc.wait + decisionGrace - time.Second)
			c.OnStatus(t.Context(), waiting)
			if len(f.responded()) != 0 {
				t.Fatalf("ttl %q: rejected before %s", tc.ttl, tc.wait)
			}
			now = now.Add(time.Second)
			c.OnStatus(t.Context(), waiting)
			if r := f.responded(); len(r) != 1 || r[0] {
				t.Fatalf("ttl %q: %v", tc.ttl, r)
			}
		}
	})
	t.Run("an unknown override asks a human", func(t *testing.T) {
		c, f, ap, _ := setup(t, "unattended")
		c.SetPolicy(wire.ApprovalPolicy{Profile: "unattended", Overrides: map[string]string{"forge.push": "maybe"}})
		c.Observe(action("c1", "git push origin agent/3kq7x2ma"))
		c.OnStatus(t.Context(), waiting)
		if r := ap.requests(); len(r) != 1 || r[0].Class != "forge.push" || len(f.responded()) != 0 {
			t.Fatalf("%v %v", r, f.responded())
		}
	})
	t.Run("an action it cannot read is denied", func(t *testing.T) {
		c, f, _, items := setup(t, "unattended")
		var e RawEvent
		_ = json.Unmarshal([]byte(`{"id":"e9","kind":"ActionEvent","source":"agent","tool_call_id":7,"tool_name":"terminal"}`), &e)
		c.Observe(e)
		c.OnStatus(t.Context(), waiting)
		if r := f.responded(); len(r) != 1 || r[0] || f.answers()[0] != textUnreadable {
			t.Fatalf("%v %v", r, f.answers())
		}
		if k := kinds(*items); !slices.Equal(k, []string{"policy_decision event:e9 forge.other deny"}) {
			t.Fatalf("logged %v", k)
		}
	})
	t.Run("an unreadable action is denied locally, never escalated blind (M2)", func(t *testing.T) {
		c, f, ap, _ := setup(t, "attended")
		c.Observe(action("c1", "gh pr create --fill"))
		c.Observe(RawEvent{ID: "e3", Kind: "ActionEvent", Raw: json.RawMessage(`{"id":"e3","tool_call_id":[]}`)})
		c.OnStatus(t.Context(), waiting)
		if r := f.responded(); len(r) != 1 || r[0] || f.answers()[0] != textUnreadable || len(ap.requests()) != 0 {
			t.Fatalf("%v %v %v", r, f.answers(), ap.requests())
		}
	})
	t.Run("an action with no call id is still answered", func(t *testing.T) {
		c, f, _, items := setup(t, "unattended")
		var e RawEvent
		_ = json.Unmarshal([]byte(`{"id":"e4","kind":"ActionEvent","source":"agent","tool_name":"terminal",`+
			`"action":{"command":"gh pr merge 1"}}`), &e)
		c.Observe(e)
		c.OnStatus(t.Context(), waiting)
		if r := f.responded(); len(r) != 1 || r[0] {
			t.Fatalf("%v", r)
		}
		if k := kinds(*items); !slices.Equal(k, []string{"policy_decision event:e4 forge.other deny"}) {
			t.Fatalf("read, and keyed by its event: %v", k)
		}
	})
	t.Run("a malformed action is still answered", func(t *testing.T) {
		c, f, _, _ := setup(t, "unattended")
		c.Observe(RawEvent{Kind: "ActionEvent", Malformed: true, Raw: json.RawMessage(`{"kind":"ActionEvent"}`)})
		c.Observe(RawEvent{Kind: "ActionEvent", Malformed: true, Raw: json.RawMessage(`{"kind":"ActionEvent"}`)})
		c.OnStatus(t.Context(), waiting)
		if r := f.responded(); len(r) != 1 || r[0] || len(c.pending) != 2 {
			t.Fatalf("%v %d", r, len(c.pending))
		}
	})
}

type approvalsFunc func(context.Context, wire.ApprovalRequest) (wire.ApprovalAck, int, error)

func (f approvalsFunc) RequestApproval(ctx context.Context, r wire.ApprovalRequest) (wire.ApprovalAck, int, error) {
	return f(ctx, r)
}

func TestHumanDecisions(t *testing.T) {
	t.Run("a decline rejects the step with the approver's reason", func(t *testing.T) {
		c, f, _, items := setup(t, "attended")
		c.Observe(action("c1", "gh pr create --fill"))
		c.Observe(action("c2", "gh issue close 4"))
		c.OnStatus(t.Context(), waiting)
		_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c2", Allow: false, Reason: "open it as a draft", Ref: 51})
		if r := f.responded(); len(r) != 1 || r[0] || f.answers()[0] != declined("open it as a draft") ||
			!strings.Contains(f.answers()[0], "open it as a draft") {
			t.Fatalf("one decline is enough, before the other approval: %v %v", r, f.answers())
		}
		if k := kinds(*items); !slices.Equal(k, []string{"decision_applied 51 7f3cq2xz"}) {
			t.Fatalf("logged %v", k)
		}
		if declined("") != textDeclined || len(declined(strings.Repeat("x", 5000))) != len(textDeclined)+len(" Their reason: ")+maxApproverReason {
			t.Fatal("the reason is optional and capped")
		}
	})
	t.Run("every approval of the step must allow it", func(t *testing.T) {
		c, f, ap, items := setup(t, "attended")
		c.Observe(action("c1", "git push origin agent/3kq7x2ma"))
		c.Observe(action("c2", "gh pr create --fill"))
		c.Observe(action("c3", "gh issue close 4"))
		c.OnStatus(t.Context(), waiting)
		if r := ap.requests(); len(r) != 2 || r[0].CallID != "c2" || r[1].CallID != "c3" ||
			!strings.Contains(string(r[0].Action), "gh pr create") {
			t.Fatalf("requests %v", r)
		}
		_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c2", Allow: true, Ref: 50})
		c.OnStatus(t.Context(), waiting)
		if len(f.responded()) != 0 {
			t.Fatal("one human class holds its siblings")
		}
		_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c3", Allow: true, Ref: 52})
		if r := f.responded(); len(r) != 1 || !r[0] || f.answers()[0] != "" {
			t.Fatalf("%v %v", r, f.answers())
		}
		want := []string{"decision_applied 50 7f3cq2xz", "decision_applied 52 7f3cq2xz", "policy_decision c1 forge.push allow"}
		if k := kinds(*items); !slices.Equal(k, want) {
			t.Fatalf("logged %v", k)
		}
		if len(ap.requests()) != 2 {
			t.Fatalf("each approval is asked for once: %v", ap.requests())
		}
	})
	t.Run("a decision replayed before its request is held", func(t *testing.T) {
		c, f, _, _ := setup(t, "attended")
		_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c1", Allow: true, Ref: 42})
		if len(f.responded()) != 0 {
			t.Fatal("a decision alone answers nothing")
		}
		c.Observe(action("c1", "gh pr create --fill"))
		c.OnStatus(t.Context(), waiting)
		if r := f.responded(); len(r) != 1 || !r[0] {
			t.Fatalf("%v", r)
		}
	})
	t.Run("held decisions are bounded", func(t *testing.T) {
		c, f, _, _ := setup(t, "attended")
		for i := range maxHeldDecisions + 5 {
			_ = c.Decision(t.Context(), wire.Decision{ApprovalID: fmt.Sprint("x", i), Allow: true})
		}
		if len(c.decided) != maxHeldDecisions {
			t.Fatalf("%d held", len(c.decided))
		}
		c.Observe(action("c1", "gh pr create --fill"))
		c.OnStatus(t.Context(), waiting)
		_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c1", Allow: true})
		if r := f.responded(); len(r) != 1 || !r[0] || len(c.decided) != maxHeldDecisions {
			t.Fatalf("a decision a pending action names is always taken, then applied: %v, %d held", r, len(c.decided))
		}
	})
	t.Run("a decision for an answered step is ignored", func(t *testing.T) {
		c, f, _, items := setup(t, "attended")
		c.Observe(action("c1", "gh pr create --fill"))
		c.OnStatus(t.Context(), waiting)
		_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c1", Allow: true, Ref: 42})
		_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c1", Allow: true, Ref: 42})
		c.OnStatus(t.Context(), waiting)
		c.OnStatus(t.Context(), waiting)
		if r := f.responded(); len(r) != 1 || len(*items) != 1 {
			t.Fatalf("answered %v, logged %v", r, kinds(*items))
		}
	})
}

func TestTheStepIsAnsweredOnce(t *testing.T) {
	t.Run("a refused answer is retried, its verdict logged once", func(t *testing.T) {
		c, f, _, items := setup(t, "unattended")
		f.refuse = http.StatusInternalServerError
		c.Observe(action("c1", "gh pr merge 1"))
		c.OnStatus(t.Context(), waiting)
		c.OnStatus(t.Context(), waiting)
		f.mu.Lock()
		f.refuse = 0
		f.mu.Unlock()
		c.OnStatus(t.Context(), waiting)
		c.OnStatus(t.Context(), waiting)
		if r := f.responded(); len(r) != 1 || r[0] {
			t.Fatalf("%v", r)
		}
		if k := kinds(*items); !slices.Equal(k, []string{"policy_decision c1 forge.other deny"}) {
			t.Fatalf("logged %v", k)
		}
	})
	t.Run("a refused answer to a decision is retried at the next status", func(t *testing.T) {
		c, f, _, items := setup(t, "attended")
		c.Observe(action("c1", "gh pr create --fill"))
		c.OnStatus(t.Context(), waiting)
		f.mu.Lock()
		f.refuse = http.StatusServiceUnavailable
		f.mu.Unlock()
		_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c1", Allow: true, Ref: 42})
		f.mu.Lock()
		f.refuse = 0
		f.mu.Unlock()
		c.OnStatus(t.Context(), waiting)
		if r := f.responded(); len(r) != 1 || !r[0] {
			t.Fatalf("not stale: the failed answer changed nothing: %v", r)
		}
		if k := kinds(*items); !slices.Equal(k, []string{"decision_applied 42 7f3cq2xz"}) {
			t.Fatalf("logged %v", k)
		}
	})
	t.Run("answered actions wait for their results, not a second answer", func(t *testing.T) {
		c, f, _, _ := setup(t, "attended")
		c.Observe(action("c1", "git push origin agent/3kq7x2ma"))
		c.OnStatus(t.Context(), waiting)
		c.OnStatus(t.Context(), waiting)
		c.Observe(action("c2", "ls"))
		c.OnStatus(t.Context(), "running")
		c.OnStatus(t.Context(), waiting)
		if r := f.responded(); len(r) != 2 {
			t.Fatalf("c1 once, then c2 alone: %v", r)
		}
	})
	t.Run("results end the wait", func(t *testing.T) {
		c, f, _, _ := setup(t, "unattended")
		c.Observe(action("c1", "gh pr merge 1"))
		c.Observe(action("c2", "gh pr merge 2"))
		c.Observe(action("c3", "gh pr merge 3"))
		c.Observe(action("c4", "gh pr merge 4"))
		for _, k := range []string{"ObservationEvent", "UserRejectObservation", "AgentErrorEvent"} {
			b, _ := json.Marshal(map[string]any{"id": "r" + k, "kind": k, "tool_call_id": map[string]string{
				"ObservationEvent": "c1", "UserRejectObservation": "c2", "AgentErrorEvent": "c3"}[k]})
			var e RawEvent
			_ = json.Unmarshal(b, &e)
			c.Observe(e)
		}
		c.Observe(RawEvent{Kind: "ObservationEvent", Malformed: true, Raw: json.RawMessage(`{"tool_call_id":"c4"}`)})
		var odd RawEvent // well formed, but a field of the wrong type: not read as c4's result
		_ = json.Unmarshal([]byte(`{"id":"r9","kind":"ObservationEvent","tool_call_id":"c4","tool_name":5}`), &odd)
		c.Observe(odd)
		if len(c.pending) != 1 || c.pending["c4"] == nil {
			t.Fatalf("pending %v", c.pending)
		}
		c.Observe(action("c4", "ls")) // mapped again after a restart: the first sighting stands
		c.OnStatus(t.Context(), waiting)
		if r := f.responded(); len(r) != 1 || r[0] {
			t.Fatalf("%v", r)
		}
	})
	t.Run("only a waiting status is answered", func(t *testing.T) {
		c, f, _, _ := setup(t, "attended")
		c.Observe(action("c1", "ls"))
		for _, s := range []string{"running", "idle", "paused", ""} {
			c.OnStatus(t.Context(), s)
		}
		if r := f.responded(); len(r) != 0 {
			t.Fatalf("answered a status that waits on nothing: %v", r)
		}
		c.OnStatus(t.Context(), waiting)
		if r := f.responded(); len(r) != 1 || !r[0] {
			t.Fatalf("%v", r)
		}
	})
	t.Run("the status after a decision's answer is stale", func(t *testing.T) {
		for _, next := range []string{waiting, "running"} {
			c, f, _, _ := setup(t, "attended")
			c.Observe(action("c1", "gh pr create --fill"))
			c.OnStatus(t.Context(), waiting)
			_ = c.Decision(t.Context(), wire.Decision{ApprovalID: "ap-c1", Allow: true, Ref: 42})
			c.Observe(action("c2", "ls"))
			c.OnStatus(t.Context(), next) // read before the decision's answer: skipped, whatever it says
			if r := f.responded(); len(r) != 1 {
				t.Fatalf("after %q: answered a stale status: %v", next, r)
			}
			c.OnStatus(t.Context(), waiting)
			if r := f.responded(); len(r) != 2 || !r[1] {
				t.Fatalf("after %q: the next status is fresh: %v", next, r)
			}
		}
	})
	t.Run("nothing pending, nothing answered", func(t *testing.T) {
		c, f, _, _ := setup(t, "attended")
		c.OnStatus(t.Context(), waiting)
		if len(f.responded()) != 0 {
			t.Fatal("answered an empty step")
		}
	})
	t.Run("a deny is found before any approver is asked", func(t *testing.T) {
		c, f, ap, _ := setup(t, "attended")
		c.SetPolicy(wire.ApprovalPolicy{Profile: "attended", Overrides: map[string]string{"forge.other": "deny"}})
		c.Observe(action("c1", "gh pr create --fill"))
		c.Observe(action("c2", "gh issue close 4"))
		c.OnStatus(t.Context(), waiting)
		if r := f.responded(); len(r) != 1 || r[0] || len(ap.requests()) != 0 {
			t.Fatalf("%v %v", r, ap.requests())
		}
	})
}

func TestReadySetsAlwaysConfirm(t *testing.T) {
	c, f, _, _ := setup(t, "attended")
	if err := c.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, p := f.snapshot(); p != "AlwaysConfirm" {
		t.Fatalf("policy %q", p)
	}
	f.mu.Lock()
	f.refuse = http.StatusInternalServerError
	f.mu.Unlock()
	if c.Ready(t.Context()) == nil {
		t.Fatal("a refusal is reported, so the bridge tries again")
	}
}

func TestNextReportsTheLogsEnd(t *testing.T) {
	ctx := bounded(t)
	f := &fakeAgentServer{pageSize: 2, status: "running"}
	agentEvents(f, 3)
	h := NewHarness(f.start(t, conv).URL, conv)
	evs, cur, end, err := h.next(ctx, Cursor{})
	if err != nil || len(evs) != 2 || end {
		t.Fatalf("a page with a next one is not the end: %d %v %v", len(evs), end, err)
	}
	evs, cur, end, err = h.next(ctx, cur)
	if err != nil || len(evs) != 1 || !end {
		t.Fatalf("the last page is: %d %v %v", len(evs), end, err)
	}
	if _, _, end, err = h.next(ctx, cur); err != nil || !end {
		t.Fatalf("an idle poll is: %v %v", end, err)
	}
	if _, _, end, err = h.next(ctx, Cursor{LastID: "nope", Count: 3}); err == nil || end {
		t.Fatalf("a failed walk is not: %v %v", end, err)
	}
	if _, _, end, err = NewHarness("http://127.0.0.1:1", conv).next(ctx, Cursor{}); err == nil || end {
		t.Fatalf("an unreachable harness is not: %v %v", end, err)
	}
	odd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"id":"e1","kind":"MessageEvent"}],"next_page_id":5}`))
	}))
	t.Cleanup(odd.Close)
	if evs, _, end, err := NewHarness(odd.URL, conv).next(ctx, Cursor{}); !errors.Is(err, ErrNextPageUnreadable) || len(evs) != 1 || end {
		t.Fatalf("a page naming no readable next page is not: %d %v %v", len(evs), end, err)
	}
	var seen []string
	if _, err := h.skip(ctx, 2, func(e RawEvent) { seen = append(seen, e.ID) }); err != nil || !slices.Equal(seen, []string{"e1", "e2"}) {
		t.Fatalf("skip hands over what it passes: %v %v", seen, err)
	}
}

// confirming wires a Confirmer into the rig's bridge as app.RunBridge does.
func confirming(r *rig) *Confirmer {
	c := &Confirmer{Harness: r.b.Harness, Broker: r.b.Broker, RunID: runID, Push: r.b.Push,
		Classifier: Classifier{Branch: "agent/3kq7x2ma"}}
	r.b.OnResume = func(_ context.Context, res wire.Resume) { c.SetPolicy(res.Approvals) }
	r.b.OnReady, r.b.OnRaw, r.b.OnStatus, r.b.OnDecision = c.Ready, c.Observe, c.OnStatus, c.Decision
	r.b.Classify = c.ClassOf
	return c
}

func actionEvent(id, cmd string) map[string]any {
	return map[string]any{"kind": "ActionEvent", "source": "agent", "tool_name": "terminal", "tool_call_id": id,
		"security_risk": "LOW", "action": map[string]string{"command": cmd}}
}

func statusKinds(fb *fakeBroker) []string { return kinds(fb.stored(wire.StreamStatus)) }

func TestTheBridgeConfirmsThroughItsHooks(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: waiting}
	f.add(actionEvent("c1", "gh pr merge 3"))
	fb := &fakeBroker{resume: wire.Resume{Approvals: wire.ApprovalPolicy{Profile: "unattended"}}}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	confirming(r)
	ctx, stop := r.run(t)
	eventually(ctx, t, "the step is answered and logged", func() bool {
		return len(f.responded()) == 1 && slices.Contains(statusKinds(fb), "policy_decision c1 forge.other deny")
	})
	time.Sleep(50 * time.Millisecond) // ten more polls, each still waiting
	stop()
	if r := f.responded(); len(r) != 1 || r[0] {
		t.Fatalf("answered %v", r)
	}
	if w := f.written(); !slices.Equal(w, []string{"respond false", "run"}) {
		t.Fatalf("the rejected conversation is resumed once: %v", w)
	}
	if p, n := f.policySet(); p != "AlwaysConfirm" || n != 1 {
		t.Fatalf("policy %q set %d times", p, n)
	}
	calls := fb.stored(wire.StreamEvents)
	if len(calls) != 1 || !payloadHas(calls[0], `"class":"forge.other"`) || !payloadHas(calls[0], `"callId":"c1"`) {
		t.Fatalf("the tool_call carries its class: %v", calls)
	}
}

func TestTheBridgeEscalatesAndAppliesTheDecision(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: waiting}
	f.add(actionEvent("c1", "gh pr create --fill"))
	fb := &fakeBroker{resume: wire.Resume{Approvals: wire.ApprovalPolicy{Profile: "attended"}},
		sse: []string{"event: decision\ndata: {\"approvalId\":\"ap-c1\",\"allow\":true,\"ref\":42}\n\n"}}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	confirming(r)
	ctx, stop := r.run(t)
	eventually(ctx, t, "the decision is applied", func() bool {
		return slices.Contains(statusKinds(fb), "decision_applied 42 "+runID)
	})
	stop()
	if r := f.responded(); len(r) != 1 || !r[0] {
		t.Fatalf("answered %v", r)
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.approvals) != 1 || fb.approvals[0].CallID != "c1" || fb.approvals[0].Class != "forge.pr" {
		t.Fatalf("approvals %v", fb.approvals)
	}
}

// OpenHands answers every pending action at once: answering before every
// action of the step was read would accept the unread ones unclassified.
func TestTheBridgeAnswersOnlyOnceTheLogIsRead(t *testing.T) {
	f := &fakeAgentServer{pageSize: 1, status: waiting}
	f.add(chatEvent("on it"))
	f.add(actionEvent("c1", "git push origin agent/3kq7x2ma"))
	f.add(actionEvent("c2", "gh pr merge 3"))
	fb := &fakeBroker{resume: wire.Resume{Approvals: wire.ApprovalPolicy{Profile: "unattended"}}}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	confirming(r)
	ctx, stop := r.run(t)
	eventually(ctx, t, "the step is answered", func() bool { return len(f.responded()) == 1 })
	time.Sleep(50 * time.Millisecond)
	stop()
	if r := f.responded(); len(r) != 1 || r[0] {
		t.Fatalf("answered %v: the push alone was read and accepted", r)
	}
}

// A restarted bridge skips what the log holds, but still learns which actions
// wait: the harness is waiting on one the log already mirrored.
func TestARestartedBridgeStillAnswersThePendingStep(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: waiting}
	f.add(chatEvent("on it"))
	f.add(actionEvent("c1", "gh pr merge 1"))
	f.add(chatEvent("waiting"))
	fb := &fakeBroker{resume: wire.Resume{AfterHarnessSeq: SeqFor(3, 0),
		Approvals: wire.ApprovalPolicy{Profile: "unattended"}}}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	confirming(r)
	ctx, stop := r.run(t)
	eventually(ctx, t, "the step is answered", func() bool { return len(f.responded()) == 1 })
	stop()
	if r := f.responded(); r[0] {
		t.Fatalf("answered %v", r)
	}
}

func TestAlwaysConfirmIsRetriedUntilTaken(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: "running", refuse: http.StatusInternalServerError}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), &fakeBroker{})
	confirming(r)
	ctx, stop := r.run(t)
	eventually(ctx, t, "the refusal is logged", func() bool { return strings.Contains(r.logs.String(), "AlwaysConfirm; retrying") })
	f.mu.Lock()
	f.refuse = 0
	f.mu.Unlock()
	eventually(ctx, t, "the policy is taken", func() bool { _, _, p := f.snapshot(); return p == "AlwaysConfirm" })
	time.Sleep(50 * time.Millisecond)
	stop()
	if _, n := f.policySet(); n != 1 {
		t.Fatalf("set %d times once taken", n)
	}
}

func TestOnlyToolCallsAreClassified(t *testing.T) {
	b := &Bridge{Classify: func(string, json.RawMessage, string) string { return "forge.pr" }}
	call := Map(RawEvent{Kind: "ActionEvent", Raw: json.RawMessage(`{"tool_name":"terminal","tool_call_id":"c1","action":{}}`)}, runID)[0]
	if got := b.classified(call); !strings.Contains(string(got.Payload), `"class":"forge.pr"`) {
		t.Fatalf("%s", got.Payload)
	}
	stub := Mapped{envelope.ToolCall, envelope.Oversize(envelope.ToolCall, 70000)}
	chat := Mapped{envelope.Message, json.RawMessage(`{"kind":"chat","text":"hi"}`)}
	for _, m := range []Mapped{stub, chat} {
		if got := b.classified(m); string(got.Payload) != string(m.Payload) {
			t.Fatalf("changed %s into %s", m.Payload, got.Payload)
		}
	}
	if got := (&Bridge{}).classified(call); string(got.Payload) != string(call.Payload) {
		t.Fatal("no hook, no class")
	}
	// A call at the cap is pushed over it by its class: it becomes the stub, never an oversize payload.
	big := envelope.Must(envelope.ToolCallPayload{CallID: "c1", Tool: "terminal",
		Args: json.RawMessage(`{"command":"` + strings.Repeat("a", envelope.MaxPayload-80) + `"}`)})
	if len(big) > envelope.MaxPayload {
		t.Fatalf("fixture is %d bytes", len(big))
	}
	if got := b.classified(Mapped{envelope.ToolCall, big}); !strings.Contains(string(got.Payload), `"oversize":true`) {
		t.Fatalf("%d bytes: %.80s", len(got.Payload), got.Payload)
	}
}

func TestRequestApprovalPostsToTheBroker(t *testing.T) {
	fb := &fakeBroker{}
	srv, ca := fb.start(t)
	br, err := NewBroker(srv.URL, writeToken(t, t.TempDir(), "v1"), ca)
	if err != nil {
		t.Fatal(err)
	}
	ack, code, err := br.RequestApproval(t.Context(), wire.ApprovalRequest{CallID: "c9", Class: "forge.pr", Action: json.RawMessage(`{"command":"gh pr create"}`)})
	if err != nil || code != http.StatusOK || ack.ApprovalID != "ap-c9" || ack.ExpiresAt.IsZero() {
		t.Fatalf("%+v %d %v", ack, code, err)
	}
	if _, code, err = br.RequestApproval(t.Context(), wire.ApprovalRequest{Class: "forge.pr", Action: json.RawMessage(`{}`)}); err != nil || code != http.StatusBadRequest {
		t.Fatalf("a refusal is a code, not an error: %d %v", code, err)
	}
	if _, _, err = br.RequestApproval(t.Context(), wire.ApprovalRequest{CallID: "c9", Action: json.RawMessage(`{`)}); err == nil {
		t.Fatal("an action that is not JSON is never sent")
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.approvals) != 1 || fb.approvals[0].Class != "forge.pr" || fb.tokens[0] != "Bearer v1" {
		t.Fatalf("%v %v", fb.approvals, fb.tokens)
	}
}

// Finding A (run ikely2yk): the broker deployed with that run predated phase 5.
// Its hello handed over no policy (so attended) and it had no approvals route,
// so the push, forge.other without a branch, asked a human and got ServeMux's
// 404. The step stays rejected, but the agent is no longer told to retry what a
// retry cannot change: it retried for 25 minutes.
func TestABrokerWithoutApprovalsRefusesForGood(t *testing.T) {
	fb := &fakeBroker{noApprovals: true}
	srv, ca := fb.start(t)
	br, err := NewBroker(srv.URL, writeToken(t, t.TempDir(), "v1"), ca)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAgentServer{pageSize: 100, status: waiting}
	c := &Confirmer{Harness: NewHarness(f.start(t, conv).URL, conv), Broker: br, RunID: "ikely2yk", Push: func(wire.Item) {}}
	c.SetPolicy(wire.ApprovalPolicy{})
	c.Observe(action("c1", "cd /workspace/cloud-native-ref && git push origin agent/ibbay5ud"))
	c.OnStatus(t.Context(), waiting)
	if r := f.responded(); len(r) != 1 || r[0] {
		t.Fatalf("the step is rejected: %v", r)
	}
	if got := f.answers()[0]; got == textBrokerDown || strings.Contains(got, "Retry it later") || !strings.Contains(got, "404") {
		t.Fatalf("the agent is told %q", got)
	}
}

// A refusal a retry gets again is not an outage; a timeout, a 408, a 429, a 401
// (a token mid-rotation) or a 5xx may clear, so the agent is told to retry later.
func TestARefusalIsToldApartFromAnOutage(t *testing.T) {
	for _, tc := range []struct {
		code  int
		err   error
		retry bool
	}{
		{0, errors.New("context deadline exceeded"), true},
		{http.StatusServiceUnavailable, nil, true},
		{http.StatusInternalServerError, nil, true},
		// A 4xx whose body could not be read: Broker.call returns its code and the error.
		{http.StatusNotFound, errors.New("broker POST /v1/bridge/approvals: unexpected EOF"), true},
		{http.StatusTooManyRequests, nil, true},
		{http.StatusUnauthorized, nil, true},
		{http.StatusRequestTimeout, nil, true},
		{http.StatusNotFound, nil, false},
		{http.StatusBadRequest, nil, false},
		{http.StatusForbidden, nil, false},
		{http.StatusConflict, nil, false},
		{http.StatusGone, nil, false},
	} {
		t.Run(fmt.Sprint(tc.code, tc.err), func(t *testing.T) {
			c, f, ap, _ := setup(t, "attended")
			ap.code, ap.err = tc.code, tc.err
			c.Observe(action("c1", "gh pr create --fill"))
			c.OnStatus(t.Context(), waiting)
			r, got := f.responded(), f.answers()
			if len(r) != 1 || r[0] {
				t.Fatalf("the step is rejected: %v", r)
			}
			want := textBrokerDown
			if !tc.retry {
				want = textRefused(tc.code)
			}
			if got[0] != want {
				t.Fatalf("told %q, want %q", got[0], want)
			}
		})
	}
}
