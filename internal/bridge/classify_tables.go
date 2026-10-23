// SPDX-License-Identifier: Apache-2.0

package bridge

import "regexp"

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
// mcp.write. search (v3 feature 5, ST8) is another: the harness's read-only child,
// whose own actions never reach the bridge; Classifier.SearchBuiltin says a run has it.
var builtins = map[string]bool{"terminal": true, "execute_bash": true, "file_editor": true, "str_replace_editor": true,
	"task_tracker": true, "think": true, "finish": true, "browser": true}

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
