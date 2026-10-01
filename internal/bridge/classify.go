// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	"github.com/Smana/agent-platform/internal/wire"
)

// Class is the approval class of one pending agent action (§6). Approvals are
// oversight, not a boundary: octo-sts, the ruleset, the Gateway and CNP still
// hold whatever a misclassified action attempts.
type Class string

// The §6 classes. Plain is everything the profile table never gates.
const (
	Plain      Class = ""
	ForgePush  Class = "forge.push"
	ForgePR    Class = "forge.pr"
	ForgeOther Class = "forge.other"
	MCPWrite   Class = "mcp.write"
	ShellHigh  Class = "shell.high"
	EgressNew  Class = "egress.new"
)

// Verdict is what the bridge does with a classified action.
type Verdict string

// The three verdicts: run it, refuse it, or ask the room's approvers.
const (
	Allow Verdict = "allow"
	Deny  Verdict = "deny"
	Human Verdict = "human"
)

// Worst class wins when one command line holds several.
var rank = map[Class]int{Plain: 0, ShellHigh: 1, ForgePush: 2, ForgePR: 3, MCPWrite: 4, ForgeOther: 5, EgressNew: 6}

var builtins = map[string]bool{"terminal": true, "execute_bash": true, "file_editor": true, "str_replace_editor": true,
	"task_tracker": true, "think": true, "finish": true, "browser": true}

// The MCP tools a run can reach are read-only by SP1's toolSelector; the room tools
// only append to the room. Anything else is mcp.write.
var readOnlyMCP = map[string]bool{"search_flux_docs": true, "get_flux_instance": true, "get_kubernetes_api_versions": true,
	"get_kubernetes_resources": true, "get_kubernetes_metrics": true, "get_kubernetes_logs": true, "documentation": true,
	"query": true, "query_range": true, "metrics": true, "metrics_metadata": true, "labels": true, "label_values": true,
	"series": true, "alerts": true, "rules": true, "explain_query": true, "prettify_query": true, "metric_statistics": true,
	"tsdb_status": true, "active_queries": true, "top_queries": true, "hits": true, "facets": true, "field_names": true,
	"field_values": true, "stats_query": true, "stats_query_range": true, "streams": true, "stream_ids": true,
	"stream_field_names": true, "stream_field_values": true, "flags": true,
	"room_read": true, "room_post": true, "room_handoff": true, "room_verdict": true}

var (
	separators = regexp.MustCompile(`&&|\|\||;|\||\n`)
	installers = map[string]string{"pip": "pypi", "pip3": "pypi", "uv": "pypi", "npm": "npm", "yarn": "npm", "pnpm": "npm", "go": "golang", "cargo": "crates"}
	installs   = map[string][]string{"pip": {"install"}, "pip3": {"install"}, "uv": {"add", "pip"}, "npm": {"install", "i", "add", "ci"},
		"yarn": {"add", "install"}, "pnpm": {"add", "install"}, "go": {"get", "install", "mod"}, "cargo": {"add", "install", "fetch", "build"}}
	ghWrites = map[string][]string{"pr": {"merge", "close", "comment", "review", "reopen"}, "issue": {"create", "edit", "close", "comment", "delete", "reopen"},
		"release": {"create", "delete", "upload", "edit"}, "repo": {"create", "delete", "edit", "fork", "rename"},
		"label": {"create", "delete", "edit"}, "workflow": {"run", "enable", "disable"}, "secret": {"set", "delete"}, "variable": {"set", "delete"}}
)

// Classifier knows the run's own branch and the egress profiles it was given.
type Classifier struct {
	Branch string
	Egress map[string]bool
}

// Classify maps one harness action to its class, deterministically: the
// harness's own risk label only matters when nothing else matched.
func (c Classifier) Classify(tool string, action json.RawMessage, risk string) Class {
	if !builtins[tool] {
		name := tool
		if i := strings.LastIndex(tool, "__"); i >= 0 {
			name = tool[i+2:]
		}
		if readOnlyMCP[name] {
			return Plain
		}
		return MCPWrite
	}
	worst := Plain
	if tool == "terminal" || tool == "execute_bash" {
		var a struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(action, &a)
		for _, seg := range separators.Split(a.Command, -1) {
			if cl := c.command(strings.Fields(seg)); rank[cl] > rank[worst] {
				worst = cl
			}
		}
	}
	if worst == Plain && strings.EqualFold(risk, "HIGH") {
		return ShellHigh
	}
	return worst
}

func (c Classifier) command(f []string) Class {
	if len(f) < 2 {
		return Plain
	}
	switch {
	case f[0] == "git" && f[1] == "push":
		for _, arg := range f[2:] {
			if strings.HasPrefix(arg, "-") || arg == "origin" || arg == "HEAD" {
				continue
			}
			if arg != c.Branch && !strings.HasSuffix(arg, ":"+c.Branch) && !strings.HasSuffix(arg, ":refs/heads/"+c.Branch) {
				return ForgeOther // a push to anything but the run's branch
			}
		}
		return ForgePush
	case f[0] == "gh" && f[1] == "pr" && len(f) > 2 && slices.Contains([]string{"create", "edit", "ready"}, f[2]):
		return ForgePR
	case f[0] == "gh" && len(f) > 2 && slices.Contains(ghWrites[f[1]], f[2]):
		return ForgeOther
	case f[0] == "gh" && f[1] == "api" && ghAPIWrites(f[2:]):
		return ForgeOther
	case installers[f[0]] != "" && slices.Contains(installs[f[0]], f[1]) && !c.Egress[installers[f[0]]]:
		return EgressNew
	}
	return Plain
}

func ghAPIWrites(args []string) bool {
	for i, a := range args {
		if (a == "-X" || a == "--method") && i+1 < len(args) && !strings.EqualFold(args[i+1], "GET") {
			return true
		}
		if a == "-f" || a == "-F" || a == "--field" || a == "--raw-field" || a == "--input" {
			return true // gh api turns a body into a POST
		}
	}
	return false
}

// The §6 table. egress.new is never approvable: the path is a fork with extra egressProfiles (S9).
var profiles = map[string]map[Class]Verdict{
	"attended":   {Plain: Allow, ForgePush: Allow, ForgePR: Human, ForgeOther: Human, MCPWrite: Human, ShellHigh: Allow, EgressNew: Deny},
	"unattended": {Plain: Allow, ForgePush: Allow, ForgePR: Allow, ForgeOther: Deny, MCPWrite: Deny, ShellHigh: Allow, EgressNew: Deny},
}

// Decide applies the room's policy to a class: an override first, then the
// profile's row. An unknown profile falls back to attended, the stricter one.
func Decide(p wire.ApprovalPolicy, c Class) Verdict {
	if c == EgressNew {
		return Deny
	}
	if v, ok := p.Overrides[string(c)]; ok && c != Plain {
		return Verdict(v)
	}
	table, ok := profiles[p.Profile]
	if !ok {
		table = profiles["attended"]
	}
	return table[c]
}
