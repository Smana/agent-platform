// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Smana/maku/internal/envelope"
)

// goodSplit is two parts, each the scoper design's example shape, state ready.
func goodSplit() map[string]any {
	second := goodScope()
	second["task"] = "Point the harness reference's link to runbook 07 at runbook 08, and change nothing else."
	second["touches"] = []string{"website/content/docs/platform/ai-platform/agents/harness-reference.md"}
	return map[string]any{"why": "Two pages link the removed runbook; each fix is verified and merged alone.",
		"children": []any{goodScope(), second}}
}

func splitHandoff(scope, split map[string]any) json.RawMessage {
	args := map[string]any{"toRole": "implementer", "summary": "Scoped and split.", "commit": sha, "split": split}
	if scope != nil {
		args["scope"] = scope
	}
	return envelope.Must(args)
}

// canonical is v's length as Validate counts it: JSON, HTML unescaped.
func canonical(t *testing.T, v any) int {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return b.Len() - 1
}

// sized is a ready scope of exactly n bytes as canonical JSON.
func sized(t *testing.T, n int) map[string]any {
	t.Helper()
	s := goodScope()
	s["plan"] = slices.Repeat([]string{strings.Repeat("p", 300)}, 7)
	s["assumptions"] = slices.Repeat([]string{strings.Repeat("a", 300)}, 5)
	s["task"] = "x"
	s["task"] = strings.Repeat("x", 1+n-canonical(t, s))
	if got := canonical(t, s); got != n {
		t.Fatalf("%d bytes, want %d", got, n)
	}
	return s
}

// Splitter design SL2, offline proof 2: a triager's scope with a valid two-part split is stored,
// a part at the 4096-byte bound included.
func TestATriagersSplitIsStored(t *testing.T) {
	atBound := goodSplit()
	atBound["children"] = []any{goodScope(), sized(t, envelope.MaxScopeBytes)}
	for name, split := range map[string]map[string]any{"two parts": goodSplit(), "a part at the bound": atBound} {
		t.Run(name, func(t *testing.T) {
			log := &memLog{}
			if _, err := tool(tools(log), "room_handoff").Call(t.Context(), triager, splitHandoff(goodScope(), split)); err != nil {
				t.Fatal(err)
			}
			var p envelope.HandoffPayload
			if err := json.Unmarshal(log.drafts[0].Payload, &p); err != nil || p.Split == nil || len(p.Split.Children) != 2 ||
				p.Scope == nil || p.FromRole != "triager" {
				t.Fatalf("payload %s (%v)", log.drafts[0].Payload, err)
			}
		})
	}
}

// Each refusal is an argument error the model can fix within its run; nothing is stored.
func TestABadSplitIsAnArgumentError(t *testing.T) {
	parts := func(n int) map[string]any {
		s := goodSplit()
		s["children"] = slices.Repeat([]any{goodScope()}, n)
		return s
	}
	asked := goodScope()
	asked["state"], asked["question"] = "question", "Which runbook replaces 07?"
	withQuestion := goodSplit()
	withQuestion["children"] = []any{goodScope(), asked}
	oversized := goodSplit()
	oversized["children"] = []any{goodScope(), sized(t, envelope.MaxScopeBytes+1)}
	for name, c := range map[string]struct {
		caller Caller
		args   json.RawMessage
	}{
		"without a scope":              {triager, splitHandoff(nil, goodSplit())},
		"beside a question":            {triager, splitHandoff(asked, goodSplit())},
		"one part":                     {triager, splitHandoff(goodScope(), parts(1))},
		"six parts":                    {triager, splitHandoff(goodScope(), parts(6))},
		"a part in state question":     {triager, splitHandoff(goodScope(), withQuestion)},
		"a part of 4097 bytes":         {triager, splitHandoff(goodScope(), oversized)},
		"no why":                       {triager, splitHandoff(goodScope(), map[string]any{"children": goodSplit()["children"]})},
		"from an implementer":          {impl, splitHandoff(nil, goodSplit())},
		"markup past the stored limit": {triager, splitHandoff(markup(), markupSplit())},
	} {
		t.Run(name, func(t *testing.T) {
			log := &memLog{}
			if _, err := tool(tools(log), "room_handoff").Call(t.Context(), c.caller, c.args); err == nil || len(log.drafts) != 0 {
				t.Fatalf("accepted (%v), %d appends", err, len(log.drafts))
			}
		})
	}
}

// split is listed to the triager only, beside scope.
func TestSplitIsListedToTheTriagerOnly(t *testing.T) {
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
		for _, tl := range res.Tools {
			if tl.Name != "room_handoff" {
				continue
			}
			if _, has := tl.InputSchema.Properties["split"]; has != (role == "triager") {
				t.Errorf("%s: split listed %v", role, has)
			}
			if _, has := tl.InputSchema.Properties["scope"]; has != (role == "triager") {
				t.Errorf("%s: scope listed %v", role, has)
			}
		}
	}
}

// markup is a valid scope of '<' runes: small unescaped, six times that as stored.
func markup() map[string]any {
	s := goodScope()
	s["task"] = strings.Repeat("<", 1000)
	s["plan"] = slices.Repeat([]string{strings.Repeat("<", 300)}, 7)
	return s
}

func markupSplit() map[string]any {
	s := goodSplit()
	s["children"] = slices.Repeat([]any{markup()}, 5)
	return s
}

func TestFiveParts(t *testing.T) {
	s := goodSplit()
	s["children"] = slices.Repeat([]any{goodScope()}, envelope.MaxChildren)
	log := &memLog{}
	if _, err := tool(tools(log), "room_handoff").Call(t.Context(), triager, splitHandoff(goodScope(), s)); err != nil {
		t.Fatal(err)
	}
}

func TestSplitSchemaNarrowsTheStateToReady(t *testing.T) {
	if !strings.Contains(envelope.SplitSchema, `"enum":["ready"]`) || strings.Contains(envelope.SplitSchema, `"enum":["ready","question"]`) {
		t.Fatal("the split schema's parts must allow state ready only")
	}
}
