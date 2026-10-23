// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/runwatch"
)

var triager = Caller{Run: runwatch.Run{ID: "7f3cq2xz", Room: "3kq7x2ma", Role: "triager", Branch: "agent/3kq7x2ma"}}

const sha = "4be1c9d0e8a7f6b5c4d3e2f1a0b9c8d7e6f5a4b3"

// goodScope is the scoper design's example, state ready.
func goodScope() map[string]any {
	return map[string]any{
		"state":       "ready",
		"task":        "Fix the user guide's link to the removed runbook 07 so it points at runbook 08, and change nothing else.",
		"touches":     []string{"website/content/docs/platform/ai-platform/agents/user-guide.md"},
		"leave":       []string{"docs/superpowers/", "website/content/docs/decisions/"},
		"plan":        []string{"Find the link to runbook 07 in the user guide.", "Point it at runbook 08's page with relref."},
		"assumptions": []string{"Runbook 08 replaces 07: its front matter says so."},
		"acceptance": []map[string]string{
			{"check": "task docs (CI: validate)", "proves": "the site builds and every relref resolves"},
			{"check": "the diff touches the user guide only", "proves": "nothing else changed"},
		},
	}
}

func handoff(scope map[string]any) json.RawMessage {
	return envelope.Must(map[string]any{"toRole": "implementer", "summary": "Scoped.", "commit": sha, "scope": scope})
}

// Scoper design SC6, offline proof 3: a triager's valid scope is stored on the handoff event.
func TestATriagersScopeIsStored(t *testing.T) {
	log := &memLog{}
	if _, err := tool(tools(log), "room_handoff").Call(t.Context(), triager, handoff(goodScope())); err != nil {
		t.Fatal(err)
	}
	var p envelope.HandoffPayload
	if err := json.Unmarshal(log.drafts[0].Payload, &p); err != nil || p.Scope == nil || p.Scope.State != "ready" ||
		len(p.Scope.Acceptance) != 2 || p.FromRole != "triager" {
		t.Fatalf("payload %s (%v)", log.drafts[0].Payload, err)
	}
}

// Each refusal is an argument error the model can fix within its run; nothing is stored.
func TestABadScopeIsAnArgumentError(t *testing.T) {
	with := func(k string, v any) map[string]any { s := goodScope(); s[k] = v; return s }
	question := func() map[string]any { return with("state", "question") }
	big := goodScope()
	big["assumptions"] = []string{strings.Repeat("a", 300), strings.Repeat("b", 300), strings.Repeat("c", 300), strings.Repeat("d", 300), strings.Repeat("e", 300)}
	big["plan"] = []string{strings.Repeat("p", 300), strings.Repeat("q", 300), strings.Repeat("r", 300), strings.Repeat("s", 300), strings.Repeat("t", 300), strings.Repeat("u", 300), strings.Repeat("v", 300)}
	for name, c := range map[string]struct {
		caller Caller
		args   json.RawMessage
	}{
		"from an implementer":          {impl, handoff(goodScope())},
		"over 4096 bytes":              {triager, handoff(big)},
		"question without a question":  {triager, handoff(question())},
		"ready with a question":        {triager, handoff(with("question", "Which dashboard?"))},
		"question without assumptions": {triager, handoff(func() map[string]any { s := question(); s["question"] = "Which?"; delete(s, "assumptions"); return s }())},
		"an unknown state":             {triager, handoff(with("state", "done"))},
		"no acceptance":                {triager, handoff(with("acceptance", []any{}))},
		"eleven touches":               {triager, handoff(with("touches", strings.Split(strings.Repeat("a,", 10)+"a", ",")))},
		"an unknown field":             {triager, handoff(with("owner", "me"))},
		"a control character":          {triager, handoff(with("task", "fix\u0000it"))},
		"a format character":           {triager, handoff(with("task", "fix\u202eit"))},
	} {
		t.Run(name, func(t *testing.T) {
			log := &memLog{}
			if _, err := tool(tools(log), "room_handoff").Call(t.Context(), c.caller, c.args); err == nil || len(log.drafts) != 0 {
				t.Fatalf("accepted (%v), %d appends", err, len(log.drafts))
			}
		})
	}
}

// scope is listed to the triager only: every other role's room_handoff schema has no such property.
func TestScopeIsListedToTheTriagerOnly(t *testing.T) {
	for _, role := range []string{"implementer", "tester", "triager"} {
		s := server(t, role)
		s.Tools = tools(&memLog{})
		b, _ := json.Marshal(rpc(t, s, "k", sub, "tools/list", nil).out["result"])
		var res struct {
			Tools []struct {
				Name        string
				InputSchema struct{ Properties map[string]json.RawMessage } `json:"inputSchema"`
			}
		}
		if err := json.Unmarshal(b, &res); err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, tl := range res.Tools {
			if tl.Name != "room_handoff" {
				continue
			}
			seen++
			if _, has := tl.InputSchema.Properties["scope"]; has != (role == "triager") {
				t.Errorf("%s: scope listed %v", role, has)
			}
		}
		if seen != 1 {
			t.Errorf("%s: room_handoff listed %d times", role, seen)
		}
	}
}
