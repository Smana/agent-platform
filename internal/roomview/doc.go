// SPDX-License-Identifier: Apache-2.0

// Package roomview renders the factory's four room views, and is the one place that makes untrusted text inert.
package roomview

import "time"

// Format is how a view is written.
type Format int

// The formats a view comes in: Markdown for an agent's chat, Text for a pipe, TextColor for a terminal.
const (
	Markdown Format = iota
	Text
	TextColor
)

// Options are the choices every view takes.
type Options struct {
	Format Format
	Now    time.Time
	All    bool
	Rooms  string // the rooms base URL, https://rooms.<domain>
}
