// SPDX-License-Identifier: Apache-2.0

package bridge

import "github.com/Smana/maku/internal/wire"

// The §6 table. egress.new is never approvable: the path is a fork with extra egressProfiles (S9).
var profiles = map[string]map[Class]Verdict{
	"attended":   {Plain: Allow, ForgePush: Allow, ForgePR: Human, ForgeOther: Human, MCPWrite: Human, ShellHigh: Allow, EgressNew: Deny},
	"unattended": {Plain: Allow, ForgePush: Allow, ForgePR: Allow, ForgeOther: Deny, MCPWrite: Deny, ShellHigh: Allow, EgressNew: Deny},
}

// overridable are the classes a Room's approvals.overrides may name (the CRD's rule).
var overridable = []Class{ForgePush, ForgePR, ForgeOther, MCPWrite, ShellHigh}

// strictness orders verdicts from the weakest gate to the strictest.
var strictness = map[Verdict]int{Allow: 0, Human: 1, Deny: 2}

// Stricter decides every class as the stricter of a and b, deny over human over
// allow, written as attended plus the overrides that differ from its row. A fork
// its source's owner did not make takes it against a new room's default, so a
// watcher never drives under a weaker gate than a room of their own (ruling M2).
func Stricter(a, b wire.ApprovalPolicy) wire.ApprovalPolicy {
	out := wire.ApprovalPolicy{Profile: "attended"}
	for _, c := range overridable {
		v := Decide(a, c)
		if w := Decide(b, c); strictness[w] > strictness[v] {
			v = w
		}
		if v != profiles["attended"][c] {
			if out.Overrides == nil {
				out.Overrides = map[string]string{}
			}
			out.Overrides[string(c)] = string(v)
		}
	}
	return out
}

// Decide applies the room's policy to a class: an override first, then the
// profile's row. It returns only allow, deny or human: an unknown profile
// falls back to attended, and an unknown override value or class asks a human.
func Decide(p wire.ApprovalPolicy, c Class) Verdict {
	if c == EgressNew {
		return Deny
	}
	if v, ok := p.Overrides[string(c)]; ok && c != Plain {
		if vv := Verdict(v); vv == Allow || vv == Deny || vv == Human {
			return vv
		}
		return Human
	}
	table, ok := profiles[p.Profile]
	if !ok {
		table = profiles["attended"]
	}
	if v, ok := table[c]; ok {
		return v
	}
	return Human
}
