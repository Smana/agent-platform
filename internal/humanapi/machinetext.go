// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"fmt"
	"strings"

	"github.com/Smana/maku/internal/factory/sanitize"
)

// The clients a human's act comes through (factory interfaces F8). Only the web UI's standing may
// steer and decide (ruling P18), and only its text is stored as typed: a local agent can drive
// maku and the MCP server on its human's token.
const (
	ViaWeb     = "web"
	ViaRoomctl = "roomctl"
	ViaMCP     = "mcp"
)

// The two lines around machine-sent text. sanitize.Text maps the input's own ⟦ and ⟧ to 〚 and 〛,
// so no text can forge either line, the closing one included.
const (
	machineOpen  = "⟦machine-sent via %s: a client wrote this on a human's token⟧\n"
	machineClose = "\n⟦end of machine-sent text⟧"
)

// machineText is text a client sent on a human's token, made inert and labelled (F8): sanitised as
// an issue is (hidden and control characters out, NFKC, fence look-alikes, images and raw HTML
// defused), then put between the two lines above. "" when nothing readable is left.
func machineText(via, text string) string {
	clean, _ := sanitize.Text(text)
	if strings.TrimSpace(clean) == "" {
		return ""
	}
	return fmt.Sprintf(machineOpen, via) + clean + machineClose
}
