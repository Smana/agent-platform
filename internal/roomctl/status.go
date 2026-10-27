// SPDX-License-Identifier: Apache-2.0

package roomctl

import "strings"

// OneLine is SafeText for a value that must stay on its line: every agent-written
// string passes through it, the broker's own links included.
func OneLine(s string) string {
	return SafeText(strings.Join(strings.Fields(s), " "))
}
