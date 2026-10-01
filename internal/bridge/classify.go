// SPDX-License-Identifier: Apache-2.0

// Classification is best-effort oversight (T6), never a boundary. It recognises
// the safe shapes positively (ruling SAP): forge.push only for the exact push of
// the run's own branch, Plain only for a command it read and found no forge
// write in. Everything it cannot read is forge.other (ruling SAN): a command
// substitution, an unterminated quote, an expansion in a command position, a
// shell nested twice, a git or gh write mentioned where no command starts.
//
// What it cannot see, and leaves Plain: script files (bash x.sh, source x.sh,
// . x.sh, a Makefile, an interpreter given a file), git aliases already in a
// config file, HTTP clients other than gh (curl with $GH_TOKEN), and which
// branch a bare `git push` or `git push origin HEAD` resolves to. `git remote
// set-url` followed by a push is bounded by the repo-scoped octo-sts token.
// Gateway and forge logs are the ground truth; octo-sts, the ruleset, the
// Gateway and CNP are the limits.

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

func set(s ...string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, k := range s {
		m[k] = true
	}
	return m
}

var (
	installers = map[string]string{"pip": "pypi", "pip3": "pypi", "uv": "pypi", "npm": "npm", "yarn": "npm", "pnpm": "npm", "go": "golang", "cargo": "crates"}
	installs   = map[string][]string{"pip": {"install"}, "pip3": {"install"}, "uv": {"add", "pip"}, "npm": {"install", "i", "add", "ci"},
		"yarn": {"add", "install"}, "pnpm": {"add", "install"}, "go": {"get", "install"}, "cargo": {"add", "install", "fetch"}}
	goMod = []string{"download", "tidy", "vendor"}

	// The safe push shape (ruling SAP): only these flags. Any other, a unique
	// prefix git would accept (--mirro) included, is forge.other.
	pushFlags  = set("-u", "--set-upstream", "--force-with-lease", "-q", "-v", "--quiet", "--verbose")
	pushValued = set("-o", "--push-option")

	// git subcommands that run no user command and write nothing remote: read, never scanned.
	// Any other (rebase -x, submodule foreach, bisect run, config, an extension) is scanned.
	gitSafe = set("status", "diff", "log", "add", "commit", "checkout", "switch", "branch", "fetch", "pull", "merge",
		"reset", "restore", "stash", "show", "rev-parse", "remote", "tag", "clone", "init", "cherry-pick", "revert",
		"blame", "grep", "ls-files", "ls-remote", "describe", "clean", "mv", "rm", "apply", "am", "format-patch",
		"reflog", "shortlog", "notes", "archive", "cat-file", "rev-list", "show-ref", "symbolic-ref", "for-each-ref",
		"merge-base", "name-rev", "diff-tree", "diff-index", "hash-object", "count-objects", "version", "help",
		"range-diff", "check-ignore", "worktree", "sparse-checkout", "gc", "fsck")
	gitValued = set("-C", "-c", "--git-dir", "--work-tree", "--namespace", "--config-env")

	// gh's command groups. Any other word is an extension or an alias, which gh runs as a command.
	ghGroups = set("agent-task", "alias", "api", "attestation", "auth", "browse", "cache", "co", "codespace", "completion",
		"config", "copilot", "extension", "gist", "gpg-key", "help", "issue", "label", "org", "pr", "preview", "project",
		"release", "repo", "ruleset", "run", "search", "secret", "ssh-key", "status", "variable", "version", "workflow")
	ghNoVerb = set("api", "browse", "completion", "help", "status", "version")
	prWrites = []string{"create", "edit", "ready"}
	ghWrites = map[string][]string{
		"pr":         {"merge", "close", "comment", "review", "reopen", "lock", "unlock", "update-branch", "revert"},
		"issue":      {"create", "edit", "close", "comment", "delete", "reopen", "transfer", "pin", "unpin", "lock", "unlock", "develop"},
		"release":    {"create", "delete", "upload", "edit", "delete-asset"},
		"repo":       {"create", "delete", "edit", "fork", "rename", "sync", "archive", "unarchive", "deploy-key", "autolink"},
		"label":      {"create", "delete", "edit", "clone"},
		"workflow":   {"run", "enable", "disable"},
		"run":        {"cancel", "rerun", "delete"},
		"secret":     {"set", "delete"},
		"variable":   {"set", "delete"},
		"alias":      {"set", "import", "delete"},
		"extension":  {"install", "upgrade", "exec"},
		"gist":       {"create", "edit", "delete", "rename"},
		"cache":      {"delete"},
		"gpg-key":    {"add", "delete"},
		"ssh-key":    {"add", "delete"},
		"codespace":  {"create", "delete", "edit", "ssh", "cp", "rebuild"},
		"agent-task": {"create"},
		"project": {"create", "delete", "edit", "close", "copy", "link", "unlink", "mark-template", "item-add",
			"item-create", "item-edit", "item-delete", "item-archive", "field-create", "field-delete"},
	}

	// Words that run the rest of the segment as a command. Their flags are skipped;
	// a flag's value is not, so `sudo -u x git push` falls to the forge scan.
	wrappers = set("env", "sudo", "command", "exec", "nohup", "time", "nice",
		"!", "{", "if", "then", "else", "elif", "do", "while", "until")
	shells     = set("sh", "bash", "zsh", "dash")
	assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	redirect   = `([0-9]*(>>|>\||<<<|<>|>|<)|&>>|&>)`
	redirOp    = regexp.MustCompile(`^` + redirect)
	redirAlone = regexp.MustCompile(`^` + redirect + `$`)
	glob       = regexp.MustCompile(`[*?]|\[[^\]]*\]`)
	brace      = regexp.MustCompile(`\{[^{}\s]*(,|\.\.)[^{}\s]*\}`)
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

// line classes a whole command line; depth is how many sh -c or eval layers it may still open.
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
	w := unwrap(dropRedirects(s.words))
	cl, scan := Plain, true
	if len(w) > 0 {
		if w[0].expanded {
			return ForgeOther // $CMD: the command is whatever the variable holds (N1)
		}
		head, args := path.Base(w[0].text), w[1:]
		switch {
		case shells[head]:
			cl = c.shell(args, s.bodies, depth)
		case head == "eval":
			cl = c.eval(args, depth)
		case head == "git":
			cl, scan = c.git(args)
		case head == "gh":
			cl, scan = c.gh(args), false
		case interpreter(head) && mentionsForge(s.bodies):
			cl = ForgeOther // python3 <<'EOF' … os.system('git push …') (N6)
		default:
			cl = c.fetch(head, wordTexts(args))
		}
	}
	if cl == Plain && scan && mentionsForge(wordTexts(s.words)) {
		return ForgeOther // a git or gh write the parser could not reach: xargs, ssh, timeout, an alias
	}
	return cl
}

func interpreter(head string) bool {
	return strings.HasPrefix(head, "python") || head == "node" || head == "ruby" || head == "perl"
}

// shell recurses into `sh -c <script>` and into heredocs fed to a shell.
func (c Classifier) shell(args []word, bodies []string, depth int) Class {
	var scripts []string
	for i := 0; i < len(args); i++ {
		a := args[i].text
		if a == "-o" || a == "+o" {
			i++
			continue
		}
		if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a, 'c') {
			if i+1 < len(args) {
				if args[i+1].expanded {
					return ForgeOther // bash -c "$CMD" (N1)
				}
				scripts = append(scripts, args[i+1].text)
			}
			break
		}
		if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+") {
			break // a script file: unseen (file header)
		}
	}
	return c.nested(append(scripts, bodies...), depth)
}

// eval runs its words joined as a script; an expanded word is unreadable (N1).
func (c Classifier) eval(args []word, depth int) Class {
	for _, a := range args {
		if a.expanded {
			return ForgeOther
		}
	}
	if len(args) == 0 {
		return Plain
	}
	return c.nested([]string{strings.Join(wordTexts(args), " ")}, depth)
}

func (c Classifier) nested(scripts []string, depth int) Class {
	worst := Plain
	for _, s := range scripts {
		if depth == 0 {
			return ForgeOther // a shell within a shell within a shell
		}
		worst = worse(worst, c.line(s, depth-1))
	}
	return worst
}

// git classes a git command, and reports whether its words still need the forge scan.
func (c Classifier) git(args []word) (Class, bool) {
	g, aliased := gitArgs(args)
	if aliased {
		return ForgeOther, false // git -c alias.p=push p (N7)
	}
	if len(g) == 0 {
		return Plain, false
	}
	if g[0].expanded {
		return ForgeOther, false // git $P (N1)
	}
	rest := wordTexts(g[1:])
	switch sub := g[0].text; {
	case sub == "push":
		return c.push(g[1:]), false
	case sub == "send-pack" || sub == "http-push":
		return ForgeOther, false // other push paths (N8); `subtree push` is not safe-listed, so the scan finds it
	case sub == "config" && slices.ContainsFunc(rest, func(a string) bool { return strings.HasPrefix(a, "alias.") }):
		return ForgeOther, false // an alias definition (N7)
	case gitSafe[sub]:
		return Plain, false
	}
	return Plain, true
}

// gitArgs drops git's global options, leaving the subcommand first, and reports
// an alias defined by -c or --config-env.
func gitArgs(a []word) ([]word, bool) {
	for len(a) > 0 && strings.HasPrefix(a[0].text, "-") {
		opt := a[0].text
		if strings.HasPrefix(opt, "--config-env=alias.") {
			return nil, true
		}
		if gitValued[opt] && len(a) > 1 {
			if (opt == "-c" || opt == "--config-env") && strings.HasPrefix(a[1].text, "alias.") {
				return nil, true
			}
			a = a[2:]
		} else {
			a = a[1:]
		}
	}
	return a, false
}

// push is forge.push only for the exact safe shape (ruling SAP): `git push`, then
// optionally origin and the run's own branch, every word a literal, and only the
// pushFlags. Anything else is forge.other: other rooms' branches, tags, a mirror.
func (c Classifier) push(args []word) Class {
	if c.Branch == "" {
		return ForgeOther // nothing to compare a push against
	}
	var pos []string
	for i := 0; i < len(args); i++ {
		if !literal(args[i]) {
			return ForgeOther
		}
		switch t := args[i].text; {
		case pushFlags[t], strings.HasPrefix(t, "--force-with-lease="), strings.HasPrefix(t, "--push-option="),
			strings.HasPrefix(t, "-o") && len(t) > 2:
		case pushValued[t]:
			i++
			if i >= len(args) || !literal(args[i]) {
				return ForgeOther
			}
		default:
			pos = append(pos, t) // any other flag (--mirro, -f) lands here and fails the shape below
		}
	}
	switch {
	case len(pos) == 0:
		return ForgePush // the current branch (file header)
	case pos[0] != "origin" || len(pos) > 2:
		return ForgeOther
	case len(pos) == 1 || c.own(pos[1]):
		return ForgePush
	}
	return ForgeOther
}

// literal is a word bash passes as written: no expansion, no quoted redirect
// character that a reader could mistake for a redirect (N5).
func literal(w word) bool {
	return !w.expanded && (!w.quoted() || !strings.ContainsAny(w.text, "<>"))
}

func (c Classifier) own(ref string) bool {
	b := c.Branch
	return ref == b || ref == "refs/heads/"+b || ref == "HEAD" || ref == "HEAD:"+b || ref == "HEAD:refs/heads/"+b
}

// gh classes a gh command from its group and verb. Only -R/--repo may stand
// between them (N3): cobra resolves the verb past any flag.
func (c Classifier) gh(args []word) Class {
	if len(args) == 0 {
		return Plain
	}
	group := args[0].text
	switch {
	case strings.HasPrefix(group, "-"):
		if len(args) == 1 {
			return Plain // gh --version
		}
		return ForgeOther
	case !ghGroups[group]:
		return ForgeOther // an extension, an alias (N7), or an expanded word ($G)
	case group == "api":
		if ghAPIWrites(wordTexts(args[1:])) {
			return ForgeOther
		}
		return Plain
	case ghNoVerb[group]:
		return Plain
	}
	rest := args[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0].text, "-") {
		switch t := rest[0].text; {
		case t == "-R" || t == "--repo":
			rest = rest[min(2, len(rest)):]
		case strings.HasPrefix(t, "--repo="), strings.HasPrefix(t, "-R"):
			rest = rest[1:]
		default:
			return ForgeOther
		}
	}
	if len(rest) == 0 {
		return Plain
	}
	if rest[0].expanded {
		return ForgeOther
	}
	switch verb := rest[0].text; {
	case group == "pr" && slices.Contains(prWrites, verb):
		return ForgePR
	case slices.Contains(ghWrites[group], verb):
		return ForgeOther
	}
	return Plain
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

// fetch reports a package fetch outside the run's egress profiles. A denied
// egress.new should point the room at the way out, a fork with
// egressProfiles: [<profile>] (S9): 5.2's denial names it.
func (c Classifier) fetch(head string, args []string) Class {
	p := installers[head]
	if p == "" || len(args) == 0 || c.Egress[p] {
		return Plain
	}
	if head == "go" && args[0] == "mod" {
		if len(args) > 1 && slices.Contains(goMod, args[1]) {
			return EgressNew
		}
		return Plain
	}
	if slices.Contains(installs[head], args[0]) {
		return EgressNew
	}
	return Plain
}

// unwrap strips env assignments, wrapper words and `timeout <duration>`.
func unwrap(w []word) []word {
	wrapped := false
	for len(w) > 0 {
		t := path.Base(w[0].text)
		switch {
		case assignment.MatchString(w[0].bare):
			w = w[1:]
		case wrapped && strings.HasPrefix(t, "-"):
			w = w[1:]
		case wrappers[t]:
			w, wrapped = w[1:], true
		case t == "timeout":
			w = w[1:]
			for len(w) > 0 && strings.HasPrefix(w[0].text, "-") {
				w = w[1:]
			}
			if len(w) > 0 {
				w = w[1:]
			}
		default:
			return w
		}
	}
	return w
}

// dropRedirects removes unquoted redirections: 2>&1, >/dev/null, and a lone
// operator with its target. A quoted ">" is a word (N5).
func dropRedirects(w []word) []word {
	out := make([]word, 0, len(w))
	for i := 0; i < len(w); i++ {
		switch {
		case redirAlone.MatchString(w[i].bare):
			i++
		case redirOp.MatchString(w[i].bare):
		default:
			out = append(out, w[i])
		}
	}
	return out
}

func wordTexts(w []word) []string {
	t := make([]string, len(w))
	for i := range w {
		t[i] = w[i].text
	}
	return t
}

// mentionsForge scans text the parser could not place for a git push or a gh
// write. Splitting on = reaches `alias p='git push …'` (N2).
func mentionsForge(text []string) bool {
	t := strings.Fields(strings.NewReplacer("`", " ", "$(", " ", "(", " ", ")", " ", "'", " ", `"`, " ",
		"=", " ").Replace(strings.Join(text, " ")))
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

// A word as bash passes it: text without quotes, bare with every quoted or
// escaped byte as \x00, and whether it holds an expansion bash resolves at run time.
type word struct {
	text     string
	bare     string
	expanded bool
}

func (w word) quoted() bool { return strings.IndexByte(w.bare, 0) >= 0 }

// A segment is one simple command: its words and the heredoc bodies fed to it.
type segment struct {
	words  []word
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
// A word holding $ outside single quotes ($VAR, $'…', $"…"), an unquoted brace
// expansion or an unquoted glob is expanded (N1). opaque reports what it cannot
// read at all: a command substitution or an unterminated quote.
func lex(line string) (segs []segment, opaque bool) {
	var (
		cur            segment
		text, bare     strings.Builder
		inWord, expand bool
		pending        []heredoc
	)
	put := func(ch byte, quoted bool) {
		text.WriteByte(ch)
		if quoted {
			bare.WriteByte(0)
		} else {
			bare.WriteByte(ch)
		}
		inWord = true
	}
	// dollar reports a $ at i that bash expands: anything but a trailing or lone $.
	dollar := func(i int, end string) bool {
		return line[i] == '$' && i+1 < len(line) && strings.IndexByte(end, line[i+1]) < 0
	}
	endWord := func() {
		if inWord {
			b := bare.String()
			cur.words = append(cur.words, word{text: text.String(), bare: b,
				expanded: expand || glob.MatchString(b) || brace.MatchString(b)})
			text.Reset()
			bare.Reset()
			inWord, expand = false, false
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
					put(line[i], true)
				}
			}
		case ch == '\'':
			j := strings.IndexByte(line[i+1:], '\'')
			if j < 0 {
				j, opaque = len(line)-i-1, true
			}
			for k := i + 1; k < i+1+j; k++ {
				put(line[k], true)
			}
			inWord, i = true, i+1+j
		case ch == '"':
			inWord = true
			for i++; i < len(line) && line[i] != '"'; i++ {
				if line[i] == '`' || (line[i] == '$' && i+1 < len(line) && line[i+1] == '(') {
					opaque = true
				}
				if dollar(i, "\" \t\n") {
					expand = true
				}
				if line[i] == '\\' && i+1 < len(line) {
					i++
				}
				put(line[i], true)
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
			put(ch, false) // >&2, 2>&1, &>file
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
			if dollar(i, " \t\n;|&()<>") {
				expand = true // $VAR, ${VAR}, $'…', $"…"
			}
			put(ch, false)
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
