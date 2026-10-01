// SPDX-License-Identifier: Apache-2.0

// Package ui embeds the built web UI: web/ builds it (`task ui:build`) into dist/,
// which is committed, and `task ui:check` fails when the two differ.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed dist
var dist embed.FS

// FS is the UI rooted at dist/: index.html, app.js and app.css at its root, where
// humanapi.Server.UI serves them (/ and /r/{id} get index.html, /assets/{file} the rest).
var FS, _ = fs.Sub(dist, "dist")
