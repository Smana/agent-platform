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

// FS is the UI rooted at dist/: index.html alone at its root, and under assets/ the
// bundle (app.js, app.css) with the svg/, favicon/ and fonts/ copied from web/src/assets.
// humanapi.Server.UI serves it: / and /r/{id} get index.html, /assets/{file...} a file
// under assets/.
var FS, _ = fs.Sub(dist, "dist")
