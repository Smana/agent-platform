// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"regexp"
	"slices"
)

// maxCursors is how many rooms' cursors cursors.json keeps (R18).
const maxCursors = 200

// Cursors keeps, per room, the cursor `roomctl changes` last printed, so the next run starts
// there. The file holds the maxCursors most recently stored rooms, oldest first. It is a
// convenience, never a source of truth: a missing or unreadable file reads as empty.
type Cursors struct{ Path string }

type cursorEntry struct {
	Room   string `json:"room"`
	Cursor string `json:"cursor"`
}

type cursorFile struct {
	Cursors []cursorEntry `json:"cursors"`
}

func (c Cursors) load() cursorFile {
	var f cursorFile
	if err := LoadJSON(c.Path, &f); err != nil {
		return cursorFile{}
	}
	return f
}

// Get is the room's stored cursor, or "".
func (c Cursors) Get(room string) string {
	for _, e := range c.load().Cursors {
		if e.Room == room {
			return e.Cursor
		}
	}
	return ""
}

// Put stores the room's cursor as the most recent, and forgets the oldest rooms past 200.
func (c Cursors) Put(room, cursor string) error {
	f := c.load()
	f.Cursors = slices.DeleteFunc(f.Cursors, func(e cursorEntry) bool { return e.Room == room })
	f.Cursors = append(f.Cursors, cursorEntry{Room: room, Cursor: cursor})
	if over := len(f.Cursors) - maxCursors; over > 0 {
		f.Cursors = f.Cursors[over:]
	}
	return SaveJSON(c.Path, f)
}

var cursorForm = regexp.MustCompile(`^seq:[0-9]{1,18}$`)

// IsCursor reports whether s is a cursor as the broker prints it, seq:N.
func IsCursor(s string) bool { return cursorForm.MatchString(s) }
