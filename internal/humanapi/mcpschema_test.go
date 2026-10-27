// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"encoding/json"
	"fmt"
	"testing"
)

// untyped lists every schema node that constrains a value (enum, items, properties, pattern) without
// naming its type. JSON Schema allows it; stricter model providers do not: Moonshot refused a whole
// session's tool list over room_fork's egress items (gcp-0, 2026-10-27).
func untyped(node any, path string) []string {
	m, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	_, typed := m["type"]
	for _, k := range []string{"enum", "items", "properties", "pattern"} {
		if _, has := m[k]; has && !typed {
			out = append(out, fmt.Sprintf("%s: %s without type", path, k))
			break
		}
	}
	if props, ok := m["properties"].(map[string]any); ok {
		for name, child := range props {
			out = append(out, untyped(child, path+".properties."+name)...)
		}
	}
	out = append(out, untyped(m["items"], path+".items")...)
	return out
}

func TestEveryToolSchemaNodeNamesItsType(t *testing.T) {
	e := setup(t, mcpOn)
	_, out := rpc(t, e, viaRoomctl("dev"), "tools/list", nil)
	b, _ := json.Marshal(out["result"])
	var r struct {
		Tools []struct {
			Name        string
			InputSchema any
		}
	}
	if json.Unmarshal(b, &r) != nil || len(r.Tools) == 0 {
		t.Fatal(string(b))
	}
	for _, tool := range r.Tools {
		for _, bad := range untyped(tool.InputSchema, tool.Name) {
			t.Error(bad)
		}
	}
}
