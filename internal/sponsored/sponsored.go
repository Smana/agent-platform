// SPDX-License-Identifier: Apache-2.0

// Package sponsored holds what the broker and the factory must agree on for a sponsored
// principal: the names neither may give one, and the shape of the git ref it may send.
package sponsored

import (
	"regexp"
	"slices"
	"strings"
)

// Reserved are names another component answers to: the fleet, the two system intakes, the broker
// and the policy engine. A sponsored principal never takes one (C22).
var Reserved = []string{"factory", "runlore", "scheduler", "room-broker", "policy"}

// IsReserved reports whether name is one of Reserved.
func IsReserved(name string) bool { return slices.Contains(Reserved, name) }

// refRE is a git ref: no leading dash, no whitespace, no control or hidden character, at most 256;
// an empty ref is none.
var refRE = regexp.MustCompile(`^([A-Za-z0-9._/][A-Za-z0-9._/-]{0,255})?$`)

// ValidRef reports whether ref is safe to write into a trusted brief: the pattern, no "..".
func ValidRef(ref string) bool { return refRE.MatchString(ref) && !strings.Contains(ref, "..") }
