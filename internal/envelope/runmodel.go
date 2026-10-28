// SPDX-License-Identifier: Apache-2.0

package envelope

import (
	"regexp"
	"slices"
)

// HarnessStatus is the state_changed kind a bridge records for a change of its harness's
// execution_status: {"kind":"harness_status","status","previous"}, plus effort once the harness
// has told it.
const HarnessStatus = "harness_status"

// Efforts are the reasoning efforts a run may report: the gateway's providers' vocabularies,
// merged. A closed set of seven, so a value the sandbox chose is still a bounded metric label.
var Efforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// ValidEffort reports whether s is one of Efforts.
func ValidEffort(s string) bool { return slices.Contains(Efforts, s) }

// runModel is a model name as an AgentRun's spec.model holds it: a C5 logical name, or a
// provider's path. Bounded, and plain enough to be a metric label and a token in every view.
var runModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,63}$`)

// ValidModel reports whether s is 1 to 64 letters, digits and ._:/@+- characters, starting with
// a letter or digit. spec.model is the claim's, never the sandbox's; this is defence in depth.
func ValidModel(s string) bool { return runModel.MatchString(s) }
