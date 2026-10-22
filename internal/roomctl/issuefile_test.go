// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// withFile files through f.
func withFile(f IssueFiler) func(*Relay) { return func(r *Relay) { r.File = f } }

// filing records what the relay asked gh to file.
type filing struct {
	mu    sync.Mutex
	calls []string
}

func (f *filing) file(_ context.Context, repo, title, body string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, repo+"|"+title+"|"+body)
	return "https://github.com/Smana/a/issues/7", nil
}

func TestRelayListsIssueFile(t *testing.T) {
	s := relayTo(t, newMCPBroker(t), tok)
	tools := s.request(t, 1, "tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 || tools[1].(map[string]any)["name"] != "issue_file" {
		t.Fatalf("%v", tools)
	}
}

// F3: issue_file files only what the developer accepted in the client's prompt, as the broker
// checked it, with no label; a client that cannot ask files nothing.
func TestIssueFile(t *testing.T) {
	args := map[string]any{"name": "issue_file", "arguments": map[string]any{"repo": "Smana/a", "title": "docs: a dead link", "body": "x"}}
	t.Run("a client that cannot ask", func(t *testing.T) {
		f := &filing{}
		s := relayTo(t, newMCPBroker(t), tok, withFile(f.file))
		s.request(t, 1, "initialize", map[string]any{"capabilities": map[string]any{}})
		res := s.request(t, 2, "tools/call", args)
		if r := res["result"].(map[string]any); r["isError"] != true || !strings.Contains(fmt.Sprint(r["content"]), "needs_confirmation") || len(f.calls) != 0 {
			t.Fatalf("%v %v", res, f.calls)
		}
	})
	for action, filed := range map[string]bool{"accept": true, "decline": false, "cancel": false} {
		t.Run(action, func(t *testing.T) {
			f := &filing{}
			b := newMCPBroker(t)
			s := relayTo(t, b, tok, withFile(f.file))
			s.request(t, 1, "initialize", map[string]any{"capabilities": map[string]any{"elicitation": map[string]any{}}})
			s.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": args})
			ask := s.next(t)
			if ask["method"] != "elicitation/create" || !strings.Contains(fmt.Sprint(ask["params"]), "## What is wrong\nstripped") {
				t.Fatalf("the developer sees the broker's draft: %v", ask)
			}
			s.send(t, map[string]any{"jsonrpc": "2.0", "id": ask["id"], "result": map[string]any{"action": action, "content": map[string]any{"file": true}}})
			res := s.next(t)
			if res["id"] != 2.0 || strings.Contains(fmt.Sprint(res["result"]), `"filed":true`) != filed || len(f.calls) != map[bool]int{true: 1, false: 0}[filed] {
				t.Fatalf("%v %v", res, f.calls)
			}
			if filed && f.calls[0] != "Smana/a|docs: a dead link|## What is wrong\nstripped" {
				t.Fatalf("filed %q", f.calls[0])
			}
		})
	}
	t.Run("a draft the broker refuses", func(t *testing.T) {
		f := &filing{}
		b := newMCPBroker(t)
		b.draftErr = "invalid_draft: the title"
		s := relayTo(t, b, tok, withFile(f.file))
		s.request(t, 1, "initialize", map[string]any{"capabilities": map[string]any{"elicitation": map[string]any{}}})
		res := s.request(t, 2, "tools/call", args)
		if !strings.Contains(fmt.Sprint(res["result"]), "invalid_draft: the title") || len(f.calls) != 0 {
			t.Fatalf("%v", res)
		}
	})
}

// GH files with the developer's gh: each value joined to its flag, the body on stdin, no label.
func TestGHFilesWithoutALabel(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + filepath.Join(dir, "args") + "\ncat > " + filepath.Join(dir, "stdin") +
		"\necho https://github.com/Smana/a/issues/7\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	url, err := GH(t.Context(), "Smana/a", "docs: a dead link", "## What is wrong\nx")
	if err != nil || url != "https://github.com/Smana/a/issues/7" {
		t.Fatalf("%q %v", url, err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if string(args) != "issue\ncreate\n--repo=Smana/a\n--title=docs: a dead link\n--body-file=-\n" || string(stdin) != "## What is wrong\nx" {
		t.Fatalf("args %q stdin %q", args, stdin)
	}
}
