// SPDX-License-Identifier: Apache-2.0

package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/Smana/agent-platform/internal/roomctl"
	"github.com/Smana/agent-platform/internal/roomview"
)

// viewFormat is the --format value as a view format: md, text, or "" for what w is (a terminal
// gets colour, a pipe plain text).
func viewFormat(w io.Writer, name string) (roomview.Format, error) {
	switch name {
	case "":
		return textFormat(w), nil
	case "md":
		return roomview.Markdown, nil
	case "text":
		return roomview.Text, nil
	}
	return 0, fmt.Errorf("--format %q: md or text", name)
}

const changesUsage = "changes <room> [--cursor seq:N] [--format md|text] [--json]"

// changes prints what changed in a room since a cursor: --cursor, else the one the last call
// stored in cursors.json, else the broker's last 24 hours. The cursor it returns is stored for the next call.
func (r Roomctl) changes(ctx context.Context, c roomctl.Client, cursorsPath string, args []string) error {
	fs := flag.NewFlagSet("changes", flag.ContinueOnError)
	cursor := fs.String("cursor", "", "start after this cursor, seq:N (default: where your last call ended)")
	format := fs.String("format", "", "md or text")
	asJSON := fs.Bool("json", false, "print the broker's structured content as is")
	pos, err := flags(fs, args)
	if err != nil || len(pos) != 1 || (*cursor != "" && !roomctl.IsCursor(*cursor)) {
		return fmt.Errorf("%s: %w", changesUsage, errors.Join(err, errors.New("one room, and a cursor of seq:N")))
	}
	f, err := viewFormat(r.Out, *format)
	if err != nil {
		return err
	}
	room, store := pos[0], roomctl.Cursors{Path: cursorsPath}
	args2 := map[string]string{"room": room}
	if cur := cmp.Or(*cursor, store.Get(room)); cur != "" {
		args2["cursor"] = cur
	}
	_, raw, err := c.Tool(ctx, "room_changes", args2)
	if errors.Is(err, roomctl.ErrUnknownTool) {
		return errors.New("roomctl changes needs a broker with room_changes (agent-platform v0.23.0 or later)")
	}
	if err != nil {
		return err
	}
	var set roomview.ChangeSet
	if err := json.Unmarshal(raw, &set); err != nil {
		return fmt.Errorf("changes: the broker's answer: %w", err)
	}
	if *asJSON {
		_, err = fmt.Fprintln(r.Out, string(raw))
	} else {
		err = roomview.Changes(r.Out, set, roomview.Options{Format: f, Now: time.Now(), Rooms: c.URL})
	}
	if err != nil {
		return err
	}
	if roomctl.IsCursor(set.Cursor) {
		return store.Put(room, set.Cursor)
	}
	return nil
}
