// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Smana/agent-platform/internal/envelope"
)

// The loop policy's stall limit is read from the room log by scripts/stall-steps.sql (loops design
// L3). It runs here on the migrated schema, over events the store's own Append wrote, so a schema
// change that breaks the operator's query fails the suite.
func TestStallStepsQuery(t *testing.T) {
	s, _, _, super := open(t)
	ctx := context.Background()
	seq := map[string]int64{}
	put := func(run, role string, typ envelope.Type, payload any) {
		seq[run]++
		d := envelope.Draft{RoomID: room, RunID: run, Actor: envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:" + run, Role: role},
			Type: typ, Origin: envelope.OriginHarness, OriginClient: "run:" + run, OriginSeq: seq[run], Payload: envelope.Must(payload)}
		if _, _, err := s.Append(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	call := func(run, role, id, command, output string) {
		args := envelope.Must(map[string]string{"command": command, "kind": "TerminalAction"})
		put(run, role, envelope.ToolCall, envelope.ToolCallPayload{CallID: id, Tool: "terminal", Args: args})
		put(run, role, envelope.ToolResult, envelope.ToolResultPayload{CallID: id, Status: "ok", Output: output, Bytes: len(output)})
	}
	call("runaaaaa", "implementer", "a1", "grep -r x .", "a")
	call("runaaaaa", "implementer", "a2", "git commit -m fix", "On branch agent/x\nnothing to commit, working tree clean")
	call("runaaaaa", "implementer", "a3", `cd repo && git add -A && git commit -m "fix: x"`, "[agent/x 1a2b3c4] fix: x\n 1 file changed")
	call("runaaaaa", "implementer", "a4", "git push", "To github.com:o/r.git")
	call("runbbbbb", "implementer", "b1", "grep -r a .", "")
	call("runbbbbb", "implementer", "b2", "grep -r b .", "")
	call("runbbbbb", "implementer", "b3", `echo "git commit"`, "git commit")
	call("runccccc", "reviewer", "c1", "git commit -m r", "[main 1a2b3c4] r")
	call("runddddd", "implementer", "d1", "git commit -q -m x", "")
	call("runddddd", "implementer", "d2", "git -C repo commit -m y", "[main abcdef0] y")

	sql, err := os.ReadFile(filepath.Join("..", "..", "scripts", "stall-steps.sql"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, super)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	var out bytes.Buffer
	if _, err := conn.PgConn().CopyTo(ctx, &out, string(sql)); err != nil {
		t.Fatal(err)
	}
	// A failed or quiet commit is not one, a reviewer's run is not counted, and a run with no
	// commit has an empty first step.
	want := "run_id,steps,first_commit_step\nrunaaaaa,4,3\nrunbbbbb,3,\nrunddddd,2,2\n"
	if out.String() != want {
		t.Fatalf("stall-steps.sql =\n%s\nwant\n%s", out.String(), want)
	}
}
