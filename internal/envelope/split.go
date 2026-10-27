// SPDX-License-Identifier: Apache-2.0

package envelope

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

// A split's bounds (splitter design SL2, "The split"): 2 to 5 parts, 21 KiB in all, well under the
// broker's 64 KiB event beside the scope and the summary.
const (
	MinChildren   = 2
	MaxChildren   = 5
	MaxSplitBytes = 21 << 10
)

// Split is the scoper's second answer (SL1, SL2): the task as two to five bounded tasks, in the
// order they merge, each a scope in state ready. It rides a triager's room_handoff beside the scope
// of the whole task, which /factory retry runs when a maintainer declines the split.
type Split struct {
	Why      string  `json:"why"`
	Children []Scope `json:"children"`
}

// SplitSchema is the split property of room_handoff's input schema, listed to the triager only:
// each part is the scope's schema in state ready.
var SplitSchema = `{"type":"object","additionalProperties":false,"required":["why","children"],"properties":{` +
	`"why":{"type":"string","minLength":1,"maxLength":500},` +
	`"children":{"type":"array","minItems":2,"maxItems":5,"items":` +
	strings.Replace(ScopeSchema, `"enum":["ready","question"]`, `"enum":["ready"]`, 1) + `}}}`

// ErrSplit is every refusal of a split: Validate's, and a split handed off without the whole task.
var ErrSplit = errors.New("split: why 1-500 bytes; children 2-5, in the order they merge, each a scope in state ready " +
	"with no question; at most 21504 bytes as JSON; and beside it scope, in state ready: the whole task")

// Validate checks the split's shape and bounds (SL2). Each part is a scope, held to Scope.Validate.
func (s Split) Validate() error {
	good := strings.TrimSpace(s.Why) != "" && len(s.Why) <= 500 && utf8.ValidString(s.Why) &&
		!strings.ContainsFunc(s.Why, Control) && len(s.Children) >= MinChildren && len(s.Children) <= MaxChildren
	for _, c := range s.Children {
		good = good && c.State == ScopeReady && c.Validate() == nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // as Scope counts: markup is one byte each
	if err := enc.Encode(s); !good || err != nil || buf.Len()-1 > MaxSplitBytes {
		return ErrSplit
	}
	return nil
}
