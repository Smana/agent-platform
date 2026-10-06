// SPDX-License-Identifier: Apache-2.0

// Classification is best-effort oversight (T6), never a boundary. It recognises
// the safe shapes positively (rulings SAP, SAR): forge.push only for the exact
// push of the run's own branch, Plain only for a command it read and found no
// forge write in. Every list it decides by is an allowlist: git global options
// and -c keys, git config writes, gh verbs per group, the environment of git and
// gh. Everything it cannot read is forge.other (ruling SAN): a command
// substitution, an unterminated quote, an expansion in a command position (a
// runner's included), a shell nested twice, a git or gh write mentioned where no
// command starts.
//
// What it cannot see, and leaves Plain (S9): script files (bash x.sh, source
// x.sh, . x.sh, a Makefile, an interpreter given a file, a git hook), config
// written by file edit (.git/config or ~/.gitconfig through file_editor), git
// aliases already in such a file, HTTP clients other than gh (curl with
// $GH_TOKEN), a runner option missing from its table (it shifts the command
// position), and which branch a bare `git push` or `git push origin HEAD`
// resolves to. `git remote set-url` followed by a push is bounded by the
// repo-scoped octo-sts token. A push of the run's branch name after `git tag
// <that name>` writes refs/tags/<name> (T1): the tag ruleset bounds it, not
// this class. Gateway and forge logs are the ground truth;
// octo-sts, the ruleset, the Gateway and CNP are the limits.

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

	// git's global options (SAR R1). Any other (--exec-path, an unknown one) is forge.other.
	gitGlobal = set("--no-pager", "-P", "-p", "--paginate", "--bare", "--no-replace-objects", "--literal-pathspecs",
		"--glob-pathspecs", "--noglob-pathspecs", "--icase-pathspecs", "--no-optional-locks", "--no-advice", "--no-lazy-fetch")
	gitGlobalValued = set("-C", "--git-dir", "--work-tree", "--namespace")

	// Config keys that hold no command, URL, refspec or path git would act on
	// (SAR R1, R2): the only ones -c, --config-env and `git config` may set.
	safeConfig       = set("user.name", "user.email", "core.quotepath", "init.defaultbranch", "safe.directory", "commit.gpgsign", "pull.rebase", "pull.ff")
	safeConfigPrefix = []string{"color.", "advice."}
	configReads      = set("--get", "--get-all", "--get-regexp", "--get-urlmatch", "--get-color", "--get-colorbool", "--list", "-l")
	configValued     = set("-f", "--file", "--blob", "--type", "--default", "--comment", "--value")

	// git subcommands that run no user command and write nothing remote: read,
	// never scanned, once commandFlags, -c and the environment are clear. config,
	// remote, push and the runners have their own readers; any other subcommand
	// (an extension) is scanned.
	gitSafe = set("status", "diff", "log", "add", "commit", "checkout", "switch", "branch", "fetch", "pull", "merge",
		"reset", "restore", "stash", "show", "rev-parse", "tag", "clone", "init", "cherry-pick", "revert",
		"blame", "grep", "ls-files", "ls-remote", "describe", "clean", "mv", "rm", "apply", "am", "format-patch",
		"reflog", "shortlog", "notes", "archive", "cat-file", "rev-list", "show-ref", "symbolic-ref", "for-each-ref",
		"merge-base", "name-rev", "diff-tree", "diff-index", "hash-object", "count-objects", "version", "help",
		"range-diff", "check-ignore", "worktree", "sparse-checkout", "gc", "fsck")
	// Options whose value git runs as a command (SAR R1), for any subcommand ("") or one.
	commandFlags = map[string][]string{
		"":        {"--upload-pack", "--receive-pack", "--ext-diff"},
		"archive": {"--exec"},
		"clone":   {"-u", "-c", "--config", "--template"},
		"init":    {"--template"},
		"grep":    {"-O", "--open-files-in-pager"},
	}
	// The git runners and the options whose value is a shell command (SAR R4).
	gitRunnerOpts = map[string]map[string]bool{
		"rebase":        set("-x", "--exec"),
		"difftool":      set("-x", "--extcmd"),
		"filter-branch": set("--setup", "--env-filter", "--tree-filter", "--index-filter", "--parent-filter", "--msg-filter", "--commit-filter", "--tag-name-filter"),
	}

	// gh's read verbs per group (SAR R3). Any other verb, an alias cobra resolves
	// (pr new, secret remove) included, is forge.other; pr's writes are forge.pr.
	ghReads = map[string][]string{
		"agent-task":  {"list", "view"},
		"alias":       {"list"},
		"attestation": {"verify", "download", "trusted-root"},
		"auth":        {"status", "token"},
		"cache":       {"list"},
		"codespace":   {"list", "view"},
		"config":      {"get", "list"},
		"extension":   {"list", "search", "browse"},
		"gist":        {"list", "view", "clone"},
		"gpg-key":     {"list"},
		"issue":       {"list", "view", "status"},
		"label":       {"list"},
		"org":         {"list"},
		"pr":          {"list", "view", "status", "diff", "checks", "checkout", "co"},
		"project":     {"list", "view", "field-list", "item-list"},
		"release":     {"list", "view", "download", "verify", "verify-asset"},
		"repo":        {"list", "view", "clone", "set-default"},
		"ruleset":     {"list", "view", "check"},
		"run":         {"list", "view", "watch", "download"},
		"search":      {"repos", "issues", "prs", "commits", "code"},
		"secret":      {"list"},
		"ssh-key":     {"list"},
		"variable":    {"list", "get"},
		"workflow":    {"list", "view"},
	}
	// gh groups without verbs. copilot is absent: it runs a command-running agent.
	ghNoVerb = set("api", "browse", "co", "completion", "help", "status", "version")
	prWrites = []string{"create", "new", "edit", "ready"}

	// The environment git and gh may be given (SAR R1); anything else is forge.other for them.
	safeEnv = set("LANG", "TZ", "CI", "NO_COLOR", "TERM", "CLICOLOR", "CLICOLOR_FORCE", "FORCE_COLOR", "COLUMNS", "LINES")
	// Exact assignments git and gh may take despite a command-valued name (ruling SAS): no pager, no prompt.
	safeAssign = set("GIT_PAGER=cat", "PAGER=cat", "GH_PAGER=cat", "GIT_TERMINAL_PROMPT=0")
	// Variables any program may run as a command or load code from: forge.other on every command.
	commandEnvs      = set("PAGER", "MANPAGER", "EDITOR", "VISUAL", "BROWSER", "LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT", "BASH_ENV", "ENV", "PROMPT_COMMAND", "LESSOPEN", "LESSCLOSE")
	commandEnvPrefix = []string{"GIT_", "GH_", "SSH_"}
	declares         = set("export", "declare", "typeset", "readonly", "local")

	shells     = set("sh", "bash", "zsh", "dash")
	assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	redirect   = `([0-9]*(>>|>\||<<<|<>|>|<)|&>>|&>)`
	redirOp    = regexp.MustCompile(`^` + redirect)
	redirAlone = regexp.MustCompile(`^` + redirect + `$`)
	glob       = regexp.MustCompile(`[*?]|\[[^\]]*\]`)
	brace      = regexp.MustCompile(`\{[^{}\s]*(,|\.\.)[^{}\s]*\}`)
)

// A runner runs a command it is given (SAR R4). Its table names the command
// position: after its options, after skip positional words, or the value of a
// script option.
type runner struct {
	valued  map[string]bool // options that take the next word as their value
	scripts map[string]bool // options whose value is a shell script: flock -c, env -S, script -c
	skip    int             // positional words before the command: timeout's duration, ssh's host, flock's lock
	shell   bool            // the command words are joined and run by a shell: watch, ssh
	appends bool            // the runner appends arguments the parser cannot see: xargs
	remote  bool            // the command runs elsewhere, with credentials the hard limits do not bound: ssh
}

var keyword = runner{}

var runners = map[string]runner{
	"!": keyword, "{": keyword, "if": keyword, "then": keyword, "else": keyword, "elif": keyword,
	"do": keyword, "while": keyword, "until": keyword,
	"command": {},
	"nohup":   {},
	"setsid":  {},
	"exec":    {valued: set("-a")},
	"env":     {valued: set("-u", "--unset", "-C", "--chdir"), scripts: set("-S", "--split-string")},
	"sudo": {valued: set("-u", "--user", "-g", "--group", "-C", "--close-from", "-D", "--chdir", "-h", "--host",
		"-p", "--prompt", "-r", "--role", "-t", "--type", "-U", "--other-user", "-T", "--command-timeout")},
	"time":   {valued: set("-f", "--format", "-o", "--output")},
	"nice":   {valued: set("-n", "--adjustment")},
	"stdbuf": {valued: set("-i", "-o", "-e", "--input", "--output", "--error")},
	// skip is the duration. An option missing from valued shifts the command
	// position by one word, which the file header lists as a residual.
	"timeout": {valued: set("-s", "--signal", "-k", "--kill-after"), skip: 1},
	"flock":   {valued: set("-w", "--timeout", "-E", "--conflict-exit-code"), scripts: set("-c", "--command"), skip: 1},
	"script": {valued: set("-E", "--echo", "-I", "--log-in", "-O", "--log-out", "-B", "--log-io", "-T", "--log-timing",
		"-m", "--logging-format"), scripts: set("-c", "--command"), skip: 1},
	"xargs": {valued: set("-a", "--arg-file", "-d", "--delimiter", "-E", "-I", "--replace", "-L", "--max-lines",
		"-n", "--max-args", "-P", "--max-procs", "-s", "--max-chars", "--process-slot-var"), appends: true},
	"watch": {valued: set("-n", "--interval", "-q", "--equexit"), shell: true},
	"ssh": {valued: set("-B", "-b", "-c", "-D", "-E", "-e", "-F", "-I", "-i", "-J", "-L", "-l", "-m", "-O", "-o", "-p",
		"-Q", "-R", "-S", "-W", "-w"), skip: 1, shell: true, remote: true},
}

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
	u := unwrap(dropRedirects(s.words))
	cl, scan := Plain, true
	for _, sc := range u.scripts {
		if sc.expanded {
			return ForgeOther // watch "$CMD", flock -c "$CMD" (R4)
		}
		cl = worse(cl, c.nested([]string{sc.text}, depth))
	}
	if len(u.cmd) == 0 {
		if slices.ContainsFunc(u.env, func(a string) bool { return commandEnv(assignName(a)) }) {
			return ForgeOther // PAGER=…, then any later command (R1)
		}
	} else {
		w := u.cmd
		if w[0].expanded {
			return ForgeOther // $CMD, xargs $CMD: the command is whatever the variable holds (N1, R4)
		}
		head, args := path.Base(w[0].text), w[1:]
		if envUnsafe(u.env, head) {
			return ForgeOther // GIT_PAGER=… git log (R1)
		}
		var hc Class
		switch {
		case shells[head]:
			hc = c.shell(args, s.bodies, depth)
		case head == "eval":
			hc = c.eval(args, depth)
		case head == "git":
			hc, scan = c.git(args, u.appended, depth)
		case head == "gh":
			hc, scan = c.gh(args, u.appended), false
		case head == "find":
			hc = c.find(args, depth)
		case head == "parallel":
			hc = parallel(args)
		case declares[head]:
			hc = exports(args)
		case interpreter(head) && mentionsForge(s.bodies):
			hc = ForgeOther // python3 <<'EOF' … os.system('git push …') (N6)
		default:
			hc = c.fetch(head, wordTexts(args))
		}
		cl = worse(cl, hc)
	}
	if u.remote && (cl == ForgePush || cl == ForgePR) {
		cl = ForgeOther
	}
	if cl == Plain && scan && mentionsForge(wordTexts(s.words)) {
		return ForgeOther // a git or gh write the parser could not reach: an alias, an unknown runner
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

// find classes each -exec, -execdir, -ok and -okdir command as its own segment (R4).
func (c Classifier) find(args []word, depth int) Class {
	worst := Plain
	for i := 0; i < len(args); i++ {
		switch args[i].text {
		case "-exec", "-execdir", "-ok", "-okdir":
			j := i + 1
			for j < len(args) && args[j].text != ";" && args[j].text != "+" {
				j++
			}
			worst = worse(worst, c.segment(segment{words: args[i+1 : j]}, depth))
			i = j
		}
	}
	return worst
}

// parallel runs the words before ::: through a shell; an expanded one is unreadable
// (R4). Literal ones are left to the forge scan.
func parallel(args []word) Class {
	for _, a := range args {
		if strings.HasPrefix(a.text, ":::") {
			break
		}
		if a.expanded {
			return ForgeOther
		}
	}
	return Plain
}

// exports reads `export NAME=…`: a command-valued name is forge.other for every
// later command (R1).
func exports(args []word) Class {
	for _, a := range args {
		name, _, _ := strings.Cut(a.text, "=")
		if commandEnv(name) {
			return ForgeOther
		}
	}
	return Plain
}

func commandEnv(name string) bool {
	return commandEnvs[name] || slices.ContainsFunc(commandEnvPrefix, func(p string) bool { return strings.HasPrefix(name, p) })
}

// envUnsafe reports an assignment a command could run: a command-valued name
// for any command, and for git and gh anything off safeEnv and safeAssign.
func envUnsafe(assigns []string, head string) bool {
	gitgh := head == "git" || head == "gh"
	for _, a := range assigns {
		if gitgh && safeAssign[a] {
			continue
		}
		if n := assignName(a); commandEnv(n) || (gitgh && !safeEnv[n] && !strings.HasPrefix(n, "LC_")) {
			return true
		}
	}
	return false
}

func assignName(a string) string {
	n, _, _ := strings.Cut(a, "=")
	return n
}

// git classes a git command, and reports whether its words still need the forge scan.
func (c Classifier) git(args []word, appended bool, depth int) (Class, bool) {
	g, unsafe := gitArgs(args)
	if unsafe {
		return ForgeOther, false // a global option or -c key off the allowlist (R1, R2, N7)
	}
	if len(g) == 0 {
		if appended {
			return ForgeOther, false // xargs git: the subcommand comes from stdin
		}
		return Plain, false
	}
	if g[0].expanded {
		return ForgeOther, false // git $P (N1)
	}
	sub, rest := g[0].text, g[1:]
	if commandFlag(sub, wordTexts(rest)) {
		return ForgeOther, false // git ls-remote --upload-pack=… (R1)
	}
	switch {
	case sub == "push":
		if appended {
			return ForgeOther, false // xargs git push: the refspecs come from stdin
		}
		return c.push(rest), false
	case sub == "send-pack" || sub == "http-push":
		return ForgeOther, false // other push paths (N8); `subtree push` is not safe-listed, so the scan finds it
	case sub == "config":
		return gitConfig(wordTexts(rest)), false
	case sub == "remote":
		return gitRemote(wordTexts(rest)), false
	case sub == "rebase" || sub == "difftool" || sub == "filter-branch" || sub == "submodule" || sub == "bisect":
		return c.gitRunner(sub, rest, depth), false
	case gitSafe[sub]:
		return Plain, false
	}
	return Plain, true
}

// gitArgs drops git's global options, leaving the subcommand first, and reports
// one off the allowlist: an unknown option, or a -c/--config-env key off safeConfig.
func gitArgs(a []word) ([]word, bool) {
	for len(a) > 0 && strings.HasPrefix(a[0].text, "-") {
		name, val, attached := strings.Cut(a[0].text, "=")
		switch {
		case name == "-c" || name == "--config-env":
			if name == "-c" || !attached {
				if len(a) < 2 {
					return nil, true
				}
				a, val = a[1:], a[1].text
			}
			if key, _, _ := strings.Cut(val, "="); !safeKey(key) {
				return nil, true
			}
		case gitGlobalValued[name]:
			if !attached && len(a) > 1 {
				a = a[1:]
			}
		case !gitGlobal[name] || attached:
			return nil, true // --exec-path and any option not listed
		}
		a = a[1:]
	}
	return a, false
}

func safeKey(key string) bool {
	k := strings.ToLower(key)
	return safeConfig[k] || slices.ContainsFunc(safeConfigPrefix, func(p string) bool { return strings.HasPrefix(k, p) })
}

// commandFlag reports an option whose value git runs as a command (R1).
func commandFlag(sub string, args []string) bool {
	flags := append(slices.Clone(commandFlags[""]), commandFlags[sub]...)
	for _, a := range args {
		for _, f := range flags {
			if a == f || strings.HasPrefix(a, f+"=") || (len(f) == 2 && strings.HasPrefix(a, f)) {
				return true
			}
		}
	}
	return false
}

// gitConfig is Plain for a read, or a write of a key on safeConfig; any other
// write, remote.*, push.*, branch.* and alias.* included, is forge.other (R2).
func gitConfig(args []string) Class {
	var pos []string
	read := false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case configReads[a]:
			read = true
		case a == "-e" || a == "--edit":
			return ForgeOther // an editor, and any key
		case configValued[a]:
			i++
		case strings.HasPrefix(a, "-"):
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) > 0 {
		switch pos[0] {
		case "get", "list":
			return Plain
		case "edit":
			return ForgeOther
		case "set", "unset", "rename-section", "remove-section":
			pos = pos[1:]
		}
	}
	if read || len(pos) <= 1 || safeKey(pos[0]) {
		return Plain
	}
	return ForgeOther
}

// gitRemote: a mirror remote, or a remote renamed to origin, widens the push
// SAP calls forge.push (R2). set-url is the documented residual.
func gitRemote(args []string) Class {
	var pos []string
	for _, a := range args {
		if strings.HasPrefix(a, "--mirror") {
			return ForgeOther
		}
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
		}
	}
	if len(pos) == 3 && pos[0] == "rename" && pos[2] == "origin" {
		return ForgeOther
	}
	return Plain
}

// gitRunner classes the commands git runs: rebase -x, difftool -x, filter-branch's
// filters, submodule foreach and bisect run (R4).
func (c Classifier) gitRunner(sub string, args []word, depth int) Class {
	var scripts []word
	switch sub {
	case "submodule", "bisect":
		verb := map[string]string{"submodule": "foreach", "bisect": "run"}[sub]
		i := slices.IndexFunc(args, func(w word) bool { return w.text == verb })
		if i < 0 {
			return Plain
		}
		rest := args[i+1:]
		for len(rest) > 0 && strings.HasPrefix(rest[0].text, "-") {
			rest = rest[1:]
		}
		if len(rest) > 0 {
			scripts = append(scripts, joined(rest)) // foreach hands its words to sh; bisect run's are read the same way
		}
	default:
		opts := gitRunnerOpts[sub]
		for i := 0; i < len(args); i++ {
			name, val, attached := splitOpt(args[i].text)
			if !opts[name] {
				continue
			}
			if attached {
				scripts = append(scripts, word{text: val, expanded: args[i].expanded})
			} else if i+1 < len(args) {
				i++
				scripts = append(scripts, args[i])
			}
		}
	}
	worst := Plain
	for _, s := range scripts {
		if s.expanded {
			return ForgeOther // git rebase -x "$CMD" (R4)
		}
		worst = worse(worst, c.nested([]string{s.text}, depth))
	}
	return worst
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
func (c Classifier) gh(args []word, appended bool) Class {
	if appended {
		return ForgeOther // xargs gh: the verb or its arguments come from stdin
	}
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
	case group == "api":
		if ghAPIWrites(wordTexts(args[1:])) {
			return ForgeOther
		}
		return Plain
	case ghNoVerb[group]:
		return Plain
	case ghReads[group] == nil:
		return ForgeOther // an extension, an alias (N7), copilot, or an expanded word ($G)
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
	return ghVerb(group, rest[0].text, wordTexts(rest[1:]))
}

// ghVerb is forge.pr for pr's writes, Plain for a read verb, and forge.other for
// anything else (R3).
func ghVerb(group, verb string, rest []string) Class {
	switch {
	case group == "pr" && slices.Contains(prWrites, verb):
		return ForgePR
	case !slices.Contains(ghReads[group], verb):
		return ForgeOther
	case group == "repo" && verb == "clone" && slices.Contains(rest, "--"):
		return ForgeOther // git options after -- (R1)
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

// unwrapped is a segment with its assignments and runners taken off.
type unwrapped struct {
	cmd      []word   // the command, head first
	env      []string // the assignments before it, NAME=value with quotes removed
	scripts  []word   // shell scripts a runner was given: flock -c, watch's and ssh's command
	appended bool     // a runner appends arguments the parser cannot see: xargs
	remote   bool     // the command runs on another host: ssh
}

// unwrap strips env assignments and runners (R4), down to the command they run.
func unwrap(w []word) unwrapped {
	var u unwrapped
	for len(w) > 0 {
		if assignment.MatchString(w[0].bare) {
			u.env = append(u.env, w[0].text)
			w = w[1:]
			continue
		}
		r, ok := runners[path.Base(w[0].text)]
		if !ok || w[0].expanded {
			break
		}
		var done bool
		w, done = r.options(w[1:], &u)
		for i := 0; !done && i < r.skip && len(w) > 0; i++ {
			w = w[1:]
		}
		if !done && r.skip > 0 {
			w, done = r.options(w, &u) // flock <lock> -c <script>
		}
		u.appended = u.appended || r.appends
		u.remote = u.remote || r.remote
		if done || r.shell {
			if !done && len(w) > 0 {
				u.scripts = append(u.scripts, joined(w))
			}
			w = nil
		}
	}
	u.cmd = w
	return u
}

// options consumes a runner's leading options. done reports a script option:
// its value is the command, and nothing after it is.
func (r runner) options(w []word, u *unwrapped) ([]word, bool) {
	for len(w) > 0 && len(w[0].text) > 1 && w[0].text[0] == '-' {
		opt := w[0]
		w = w[1:]
		if opt.text == "--" {
			break
		}
		name, val, attached := r.cluster(opt.text)
		switch {
		case r.scripts[name]:
			if attached {
				u.scripts = append(u.scripts, word{text: val, expanded: opt.expanded})
			} else if len(w) > 0 {
				u.scripts = append(u.scripts, w[0])
			}
			return nil, true
		case r.valued[name] && !attached && len(w) > 0:
			w = w[1:]
		}
	}
	return w, false
}

// cluster reads a short-option cluster the way getopt does: the first letter
// that takes a value ends it, with the rest as its value, or the next word when
// it is last (script -qc <script>, sudo -iu <user>).
func (r runner) cluster(o string) (name, val string, attached bool) {
	if strings.HasPrefix(o, "--") || len(o) <= 2 {
		return splitOpt(o)
	}
	for k := 1; k < len(o); k++ {
		if f := "-" + o[k:k+1]; r.scripts[f] || r.valued[f] {
			return f, o[k+1:], k+1 < len(o)
		}
	}
	return o, "", false
}

// splitOpt splits --name=value and -Xvalue.
func splitOpt(o string) (name, val string, attached bool) {
	if strings.HasPrefix(o, "--") {
		return strings.Cut(o, "=")
	}
	if len(o) > 2 {
		return o[:2], o[2:], true
	}
	return o, "", false
}

func joined(w []word) word {
	return word{text: strings.Join(wordTexts(w), " "), expanded: slices.ContainsFunc(w, func(x word) bool { return x.expanded })}
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
				if (ghReads[t[j]] != nil && ghVerb(t[j], t[j+1], nil) != Plain) || (t[j] == "api" && ghAPIWrites(t[j+1:])) {
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
