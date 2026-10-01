// SPDX-License-Identifier: Apache-2.0

// Classification is best-effort oversight (T6), never a boundary. It reads a
// command line as far as a small shell lexer can, and anything it cannot fully
// read classes as forge.other (ruling SAN): a command substitution, an
// unterminated quote, a shell nested twice, a git or gh write mentioned where no
// command starts. What it cannot see at all, and leaves Plain: scripts and
// interpreters (a script file, python -c, a Makefile), git aliases and config
// (git -c alias.p=push p), HTTP clients other than gh (curl with $GH_TOKEN), gh
// writes missing from ghWrites, and which branch a bare `git push` or
// `git push origin HEAD` resolves to. Gateway and forge logs are the ground
// truth; octo-sts, the ruleset, the Gateway and CNP are the limits.

package bridge

import (
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/Smana/agent-platform/internal/wire"
)

// Class is the approval class of one pending agent action (§6).
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

func worse(a, b Class) Class {
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// The harness's own tools. Anything else is an MCP tool; OpenHands' browser_*
// tools, enabled when get_default_agent runs without cli_mode, would class as
// mcp.write.
var builtins = map[string]bool{"terminal": true, "execute_bash": true, "file_editor": true, "str_replace_editor": true,
	"task_tracker": true, "think": true, "finish": true, "browser": true}

// The MCP tools a run can reach, as the gateway names them (<backend>__<tool>):
// read-only by SP1's toolSelector, and the room tools only append to the room.
// Anything else is mcp.write. The harness event carries no readOnlyHint.
var readOnlyMCP = func() map[string]bool {
	m := map[string]bool{}
	for server, tools := range map[string][]string{
		"flux-operator-mcp": {"search_flux_docs", "get_flux_instance", "get_kubernetes_api_versions",
			"get_kubernetes_resources", "get_kubernetes_metrics", "get_kubernetes_logs"},
		"mcp-victoriametrics": {"documentation", "query", "query_range", "metrics", "metrics_metadata", "labels",
			"label_values", "series", "alerts", "rules", "explain_query", "prettify_query", "metric_statistics",
			"tsdb_status", "active_queries", "top_queries"},
		"mcp-victorialogs": {"documentation", "query", "hits", "facets", "field_names", "field_values", "stats_query",
			"stats_query_range", "streams", "stream_ids", "stream_field_names", "stream_field_values", "flags"},
		"room-broker": {"room_read", "room_post", "room_handoff", "room_verdict"},
	} {
		for _, t := range tools {
			m[server+"__"+t] = true
		}
	}
	return m
}()

// readOnly matches the last server__tool pair, so a prefix the harness adds
// cannot hide the server, and a bare tool name never matches.
func readOnly(tool string) bool {
	p := strings.Split(tool, "__")
	return len(p) >= 2 && readOnlyMCP[p[len(p)-2]+"__"+p[len(p)-1]]
}

var (
	installers = map[string]string{"pip": "pypi", "pip3": "pypi", "uv": "pypi", "npm": "npm", "yarn": "npm", "pnpm": "npm", "go": "golang", "cargo": "crates"}
	installs   = map[string][]string{"pip": {"install"}, "pip3": {"install"}, "uv": {"add", "pip"}, "npm": {"install", "i", "add", "ci"},
		"yarn": {"add", "install"}, "pnpm": {"add", "install"}, "go": {"get", "install"}, "cargo": {"add", "install", "fetch"}}
	goMod    = []string{"download", "tidy", "vendor"}
	prWrites = []string{"create", "edit", "ready"}
	ghWrites = map[string][]string{"pr": {"merge", "close", "comment", "review", "reopen", "lock", "unlock"},
		"issue":   {"create", "edit", "close", "comment", "delete", "reopen", "transfer", "pin", "unpin", "lock", "unlock", "develop"},
		"release": {"create", "delete", "upload", "edit"}, "repo": {"create", "delete", "edit", "fork", "rename", "sync", "archive", "unarchive"},
		"label": {"create", "delete", "edit"}, "workflow": {"run", "enable", "disable"}, "run": {"cancel", "rerun", "delete"},
		"secret": {"set", "delete"}, "variable": {"set", "delete"}}

	// Words that run the rest of the segment as a command. Their flags are skipped;
	// a flag's value is not, so `sudo -u x git push` falls to the forge scan.
	wrappers = map[string]bool{"env": true, "sudo": true, "command": true, "exec": true, "nohup": true, "time": true, "nice": true,
		"!": true, "{": true, "if": true, "then": true, "else": true, "elif": true, "do": true, "while": true, "until": true}
	shells     = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true}
	assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	redirect   = `([0-9]*(>>|>\||<<<|<>|>|<)|&>>|&>)`
	redirOp    = regexp.MustCompile(`^` + redirect)
	redirAlone = regexp.MustCompile(`^` + redirect + `$`)

	gitValued  = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true, "--config-env": true}
	pushWidens = map[string]bool{"--all": true, "--branches": true, "--mirror": true, "--tags": true, "--follow-tags": true,
		"--prune": true, "--delete": true, "-d": true}
	pushValued = map[string]bool{"-o": true, "--push-option": true, "--repo": true, "--receive-pack": true, "--exec": true}
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
		if readOnly(tool) {
			return Plain
		}
		return MCPWrite
	}
	worst := Plain
	if tool == "terminal" || tool == "execute_bash" {
		var a struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(action, &a); err != nil {
			return ForgeOther // an action the bridge cannot read is not one it can wave through (C4)
		}
		worst = c.line(a.Command, 1)
	}
	if worst == Plain && strings.EqualFold(risk, "HIGH") {
		return ShellHigh
	}
	return worst
}

// line classes a whole command line; depth is how many sh -c layers it may still open.
func (c Classifier) line(cmd string, depth int) Class {
	segs, opaque := lex(cmd)
	worst := Plain
	if opaque {
		worst = ForgeOther
	}
	for _, s := range segs {
		worst = worse(worst, c.segment(s, depth))
	}
	return worst
}

func (c Classifier) segment(s segment, depth int) Class {
	f := unwrap(dropRedirects(s.words))
	if len(f) == 0 {
		return Plain
	}
	var cl Class
	if shells[f[0]] {
		cl = c.shell(f[1:], s.bodies, depth)
	} else {
		cl = c.command(f)
	}
	if cl == Plain && f[0] != "git" && f[0] != "gh" && mentionsForge(s.words) {
		return ForgeOther // a git or gh write the parser could not reach: xargs, ssh, timeout, echo
	}
	return cl
}

// shell recurses into `sh -c <script>` and into heredocs fed to a shell.
func (c Classifier) shell(args, bodies []string, depth int) Class {
	var scripts []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-o" || a == "+o" {
			i++
			continue
		}
		if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a, 'c') {
			if i+1 < len(args) {
				scripts = append(scripts, args[i+1])
			}
			break
		}
		if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+") {
			break // a script file: unseen (file header)
		}
	}
	worst := Plain
	for _, s := range append(scripts, bodies...) {
		if depth == 0 {
			return ForgeOther
		}
		worst = worse(worst, c.line(s, depth-1))
	}
	return worst
}

func (c Classifier) command(f []string) Class {
	if len(f) < 2 {
		return Plain
	}
	switch f[0] {
	case "git":
		if g := gitArgs(f[1:]); len(g) > 0 && g[0] == "push" {
			return c.push(g[1:])
		}
	case "gh":
		switch {
		case f[1] == "pr" && len(f) > 2 && slices.Contains(prWrites, f[2]):
			return ForgePR
		case len(f) > 2 && slices.Contains(ghWrites[f[1]], f[2]):
			return ForgeOther
		case f[1] == "api" && ghAPIWrites(f[2:]):
			return ForgeOther
		}
	default:
		if installers[f[0]] != "" && fetches(f) && !c.Egress[installers[f[0]]] {
			return EgressNew
		}
	}
	return Plain
}

// push is forge.push only for the run's own branch on origin; anything wider,
// another branch, a deletion or tags is forge.other.
func (c Classifier) push(args []string) Class {
	if c.Branch == "" {
		return ForgeOther // nothing to compare a push against
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case pushWidens[a] || shortFlag(a, 'd'):
			return ForgeOther // other rooms' branches and tags, outside the ruleset's refs/heads/agent/** (S9)
		case pushValued[a]:
			i++
		case strings.HasPrefix(a, "-"), a == "origin", a == "HEAD":
		case strings.HasPrefix(a, ":"):
			return ForgeOther // a deletion refspec
		case c.own(strings.TrimPrefix(a, "+")):
		default:
			return ForgeOther // a push to anything but the run's branch
		}
	}
	return ForgePush
}

func (c Classifier) own(ref string) bool {
	return ref == c.Branch || ref == "refs/heads/"+c.Branch ||
		strings.HasSuffix(ref, ":"+c.Branch) || strings.HasSuffix(ref, ":refs/heads/"+c.Branch)
}

// shortFlag reports a cluster of git push's short flags (-fd) holding flag;
// -o carries its value attached (-oci.skip), so it is never a cluster.
func shortFlag(a string, flag rune) bool {
	return len(a) > 1 && a[0] == '-' && a[1] != '-' && a[1] != 'o' && strings.ContainsRune(a[1:], flag)
}

// gitArgs drops git's global options, leaving the subcommand first.
func gitArgs(a []string) []string {
	for len(a) > 0 && strings.HasPrefix(a[0], "-") {
		if gitValued[a[0]] && len(a) > 1 {
			a = a[2:]
		} else {
			a = a[1:]
		}
	}
	return a
}

// ghAPIWrites follows gh: the method is GET, or POST once a body flag is set,
// unless -X names one. pflag accepts -XPOST, --method=POST, -fk=v and --field=k=v.
func ghAPIWrites(args []string) bool {
	method, body := "", false
	for i, a := range args {
		switch {
		case a == "-X" || a == "--method":
			if i+1 < len(args) {
				method = args[i+1]
			}
		case strings.HasPrefix(a, "--method="):
			method = strings.TrimPrefix(a, "--method=")
		case strings.HasPrefix(a, "-X"):
			method = a[2:]
		case strings.HasPrefix(a, "-f"), strings.HasPrefix(a, "-F"), a == "--field", a == "--raw-field", a == "--input",
			strings.HasPrefix(a, "--field="), strings.HasPrefix(a, "--raw-field="), strings.HasPrefix(a, "--input="):
			body = true
		}
	}
	if method != "" {
		return !strings.EqualFold(method, "GET")
	}
	return body
}

// fetches reports a package fetch. A denied egress.new should point the room at
// the way out, a fork with egressProfiles: [<profile>] (S9): 5.2's denial names it.
func fetches(f []string) bool {
	if f[0] == "go" && f[1] == "mod" {
		return len(f) > 2 && slices.Contains(goMod, f[2])
	}
	return slices.Contains(installs[f[0]], f[1])
}

// unwrap strips env assignments and wrapper words, and reduces the command to
// its base name (/usr/bin/git is git).
func unwrap(f []string) []string {
	wrapped := false
	for len(f) > 0 {
		switch {
		case assignment.MatchString(f[0]):
			f = f[1:]
		case wrapped && strings.HasPrefix(f[0], "-"):
			f = f[1:]
		case wrappers[f[0]]:
			f, wrapped = f[1:], true
		default:
			return append([]string{path.Base(f[0])}, f[1:]...)
		}
	}
	return f
}

// dropRedirects removes redirections: 2>&1, >/dev/null, and a lone operator with its target.
func dropRedirects(w []string) []string {
	out := make([]string, 0, len(w))
	for i := 0; i < len(w); i++ {
		switch {
		case redirAlone.MatchString(w[i]):
			i++
		case redirOp.MatchString(w[i]):
		default:
			out = append(out, w[i])
		}
	}
	return out
}

// mentionsForge scans words the parser could not place for a git push or a gh write.
func mentionsForge(words []string) bool {
	t := strings.Fields(strings.NewReplacer("`", " ", "$(", " ", "(", " ", ")", " ", "'", " ", `"`, " ").Replace(strings.Join(words, " ")))
	for i, w := range t {
		switch path.Base(w) {
		case "git":
			if slices.Contains(t[i+1:], "push") {
				return true
			}
		case "gh":
			for j := i + 1; j+1 < len(t); j++ {
				if (t[j] == "pr" && slices.Contains(prWrites, t[j+1])) || slices.Contains(ghWrites[t[j]], t[j+1]) ||
					(t[j] == "api" && ghAPIWrites(t[j+1:])) {
					return true
				}
			}
		}
	}
	return false
}

// A segment is one simple command: its words, quotes removed, and the heredoc
// bodies fed to it.
type segment struct {
	words  []string
	bodies []string
}

type heredoc struct {
	seg    int
	delim  string
	strip  bool // <<-
	quoted bool // <<'EOF': the body is literal
}

// lex splits a command line as a POSIX shell would, as far as classification
// needs: quotes and backslashes group, ; | & ( ) newline and substitutions split
// outside quotes, a # comment ends the line, and heredoc bodies are set aside.
// opaque reports what it cannot read: a command substitution or an unterminated quote.
func lex(line string) (segs []segment, opaque bool) {
	var (
		cur     segment
		word    strings.Builder
		inWord  bool
		pending []heredoc
	)
	endWord := func() {
		if inWord {
			cur.words = append(cur.words, word.String())
			word.Reset()
			inWord = false
		}
	}
	endSeg := func() {
		endWord()
		segs = append(segs, cur)
		cur = segment{}
	}
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case ch == '\\':
			if i+1 < len(line) {
				i++
				if line[i] != '\n' {
					word.WriteByte(line[i])
					inWord = true
				}
			}
		case ch == '\'':
			j := strings.IndexByte(line[i+1:], '\'')
			if j < 0 {
				word.WriteString(line[i+1:])
				inWord, opaque, i = true, true, len(line)
				continue
			}
			word.WriteString(line[i+1 : i+1+j])
			inWord, i = true, i+1+j
		case ch == '"':
			inWord = true
			for i++; i < len(line) && line[i] != '"'; i++ {
				if line[i] == '`' || (line[i] == '$' && i+1 < len(line) && line[i+1] == '(') {
					opaque = true
				}
				if line[i] == '\\' && i+1 < len(line) {
					i++
				}
				word.WriteByte(line[i])
			}
			if i >= len(line) {
				opaque = true
			}
		case ch == '#' && !inWord:
			for i+1 < len(line) && line[i+1] != '\n' {
				i++
			}
		case ch == '`' || (ch == '$' && i+1 < len(line) && line[i+1] == '('):
			opaque = true
			endSeg() // the substituted command is still classified, as its own segment
		case ch == '&' && ((i > 0 && line[i-1] == '>') || (i+1 < len(line) && line[i+1] == '>')):
			word.WriteByte(ch) // >&2, 2>&1, &>file
			inWord = true
		case strings.HasPrefix(line[i:], "<<") && !strings.HasPrefix(line[i:], "<<<"):
			endWord()
			h, n := readDelim(line[i+2:])
			if h.delim == "" {
				opaque = true
			}
			h.seg = len(segs)
			pending = append(pending, h)
			i += 1 + n
		case ch == '\n':
			endSeg()
			var bad bool
			i, bad = bodies(line, i+1, pending, segs)
			opaque = opaque || bad
			pending = nil
		case strings.IndexByte(";|&()", ch) >= 0:
			endSeg()
		case ch == ' ' || ch == '\t':
			endWord()
		default:
			word.WriteByte(ch)
			inWord = true
		}
	}
	endSeg()
	return segs, opaque
}

// readDelim reads a heredoc operator's tail (after <<): -, blanks, then the
// delimiter word. It returns the heredoc and the bytes consumed.
func readDelim(s string) (heredoc, int) {
	var h heredoc
	i := 0
	if i < len(s) && s[i] == '-' {
		h.strip = true
		i++
	}
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	var d strings.Builder
	for ; i < len(s) && strings.IndexByte(" \t\n;|&()<>", s[i]) < 0; i++ {
		switch s[i] {
		case '\'', '"':
			h.quoted = true
			if j := strings.IndexByte(s[i+1:], s[i]); j >= 0 {
				d.WriteString(s[i+1 : i+1+j])
				i += 1 + j
			}
		case '\\':
			h.quoted = true
			if i+1 < len(s) {
				i++
				d.WriteByte(s[i])
			}
		default:
			d.WriteByte(s[i])
		}
	}
	h.delim = d.String()
	return h, i
}

// bodies consumes the pending heredocs' lines from pos and attaches each body to
// its segment. It returns the index of the last byte consumed, and whether an
// unquoted body holds a substitution.
func bodies(line string, pos int, pending []heredoc, segs []segment) (int, bool) {
	opaque := false
	for _, h := range pending {
		var lines []string
		for pos < len(line) {
			end := strings.IndexByte(line[pos:], '\n')
			if end < 0 {
				end = len(line) - pos
			}
			l := line[pos : pos+end]
			pos += end + 1
			check := l
			if h.strip {
				check = strings.TrimLeft(l, "\t")
			}
			if check == h.delim {
				break
			}
			lines = append(lines, l)
		}
		body := strings.Join(lines, "\n")
		if !h.quoted && (strings.Contains(body, "`") || strings.Contains(body, "$(")) {
			opaque = true
		}
		segs[h.seg].bodies = append(segs[h.seg].bodies, body)
	}
	return pos - 1, opaque
}

// The §6 table. egress.new is never approvable: the path is a fork with extra egressProfiles (S9).
var profiles = map[string]map[Class]Verdict{
	"attended":   {Plain: Allow, ForgePush: Allow, ForgePR: Human, ForgeOther: Human, MCPWrite: Human, ShellHigh: Allow, EgressNew: Deny},
	"unattended": {Plain: Allow, ForgePush: Allow, ForgePR: Allow, ForgeOther: Deny, MCPWrite: Deny, ShellHigh: Allow, EgressNew: Deny},
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
