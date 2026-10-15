// SPDX-License-Identifier: Apache-2.0

package forge

import "strings"

// SameLogin compares GitHub logins, which GitHub treats as case-insensitive; every login
// comparison at the merge gate goes through it (review 2026-10-15, F7).
func SameLogin(a, b string) bool { return strings.EqualFold(a, b) }
