// SPDX-License-Identifier: Apache-2.0

package envelope

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The scope's states (scoper design SC6, SC7): ready, the implementer can start; question, the
// task waits for its issue's author.
const (
	ScopeReady    = "ready"
	ScopeQuestion = "question"
)

// MaxScopeBytes bounds a scope as canonical JSON (SC6).
const MaxScopeBytes = 4096

// Scope is the scoper's bounded task (SC6), carried by a triager's room_handoff. The broker
// validates it; the factory re-injects it into every implementer brief (SC9).
type Scope struct {
	State       string       `json:"state"`
	Task        string       `json:"task"`
	Touches     []string     `json:"touches"`
	Leave       []string     `json:"leave,omitempty"`
	Plan        []string     `json:"plan"`
	Assumptions []string     `json:"assumptions,omitempty"`
	Acceptance  []Acceptance `json:"acceptance"`
	Question    string       `json:"question,omitempty"`
}

// Acceptance is one check the judge reads: a command or an observation, and what it proves.
type Acceptance struct {
	Check  string `json:"check"`
	Proves string `json:"proves"`
}

// ScopeSchema is the scope property of room_handoff's input schema, listed to the triager only.
const ScopeSchema = `{"type":"object","additionalProperties":false,"required":["state","task","touches","plan","acceptance"],"properties":{` +
	`"state":{"enum":["ready","question"]},` +
	`"task":{"type":"string","minLength":1,"maxLength":1000},` +
	`"touches":{"type":"array","minItems":1,"maxItems":10,"items":{"type":"string","minLength":1,"maxLength":200}},` +
	`"leave":{"type":"array","maxItems":10,"items":{"type":"string","minLength":1,"maxLength":200}},` +
	`"plan":{"type":"array","minItems":1,"maxItems":7,"items":{"type":"string","minLength":1,"maxLength":300}},` +
	`"assumptions":{"type":"array","maxItems":5,"items":{"type":"string","minLength":1,"maxLength":300}},` +
	`"acceptance":{"type":"array","minItems":1,"maxItems":5,"items":{"type":"object","additionalProperties":false,"required":["check","proves"],` +
	`"properties":{"check":{"type":"string","minLength":1,"maxLength":300},"proves":{"type":"string","minLength":1,"maxLength":200}}}},` +
	`"question":{"type":"string","maxLength":500}}}`

// ErrScope is every refusal of Validate: the broker returns it to the model as an argument error.
var ErrScope = errors.New("scope: state ready or question; task 1-1000 bytes; touches 1-10 and leave 0-10, each 1-200; " +
	"plan 1-7 and assumptions 0-5, each 1-300; acceptance 1-5 of {check 1-300, proves 1-200}; question 1-500 exactly when " +
	"state is question, with at least one assumption; at most 4096 bytes as JSON")

// Validate checks the scope's shape and bounds (SC6). Bytes, not characters: the bound is the
// brief's, which is bytes.
func (s Scope) Validate() error {
	ok := func(v string, limit int) bool {
		return strings.TrimSpace(v) != "" && len(v) <= limit && utf8.ValidString(v) && !strings.ContainsFunc(v, control)
	}
	list := func(vs []string, lo, hi, each int) bool {
		if len(vs) < lo || len(vs) > hi {
			return false
		}
		for _, v := range vs {
			if !ok(v, each) {
				return false
			}
		}
		return true
	}
	good := (s.State == ScopeReady || s.State == ScopeQuestion) && ok(s.Task, 1000) &&
		list(s.Touches, 1, 10, 200) && list(s.Leave, 0, 10, 200) && list(s.Plan, 1, 7, 300) &&
		list(s.Assumptions, 0, 5, 300) && len(s.Acceptance) >= 1 && len(s.Acceptance) <= 5
	for _, a := range s.Acceptance {
		good = good && ok(a.Check, 300) && ok(a.Proves, 200)
	}
	if s.State == ScopeQuestion {
		good = good && ok(s.Question, 500) && len(s.Assumptions) > 0
	} else {
		good = good && s.Question == ""
	}
	// Unescaped: HTML escaping would count <, > and & as six bytes each.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); !good || err != nil || buf.Len()-1 > MaxScopeBytes {
		return ErrScope
	}
	return nil
}

// control is a control character other than tab and newline, or a Unicode format character
// (zero-width, bidi): the same runes steering.Validate refuses, since a scope is agent-influencing
// text in later implementer briefs.
func control(r rune) bool {
	return (unicode.IsControl(r) && r != '\n' && r != '\t') || unicode.Is(unicode.Cf, r)
}
