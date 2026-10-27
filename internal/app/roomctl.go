// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/roomctl"
	"github.com/Smana/agent-platform/internal/roomctl/skill"
	"github.com/Smana/agent-platform/internal/roomview"
	"github.com/Smana/agent-platform/internal/summary"
	"github.com/Smana/agent-platform/internal/version"
)

// roomctlTimeout bounds one call to the broker or the IdP; actTimeout one act.
const (
	roomctlTimeout = 30 * time.Second
	actTimeout     = time.Minute
)

const roomctlUsage = `usage: roomctl <command>
  configure --url URL [--issuer URL --client-id ID --project-id ID]
                                  the broker's URL is enough: it serves the other three
  login                           sign in with the device flow
  doctor                          check the VPN, the CA, your login and your GitHub link, with the fix of each
  token                           print a valid access token, for scripts (task agent:run)
  status <room> [--json] [--after seq:N] [--format md|text]
                                  where a room stands: status, what needs you, notes
  changes <room> [--cursor seq:N] [--format md|text] [--json]
                                  what changed in a room since the cursor of your last call (the first call: 24 hours)
  rooms [--repo owner/name] [--mine] [--needs-me] [--all] [--format md|text] [--json]
                                  the rooms you can read, with each task's issue, title, phase and PR. --mine: the tasks of
                                  issues you filed or labelled, PRs you authored or review.
                                  --needs-me: an approval you could decide, a task held on your room,
                                  or a PR waiting for your merge. --all: the rooms that ended over a week ago too
  checkout <room>                 check the room's agent/<room> branch out into a new worktree beside this repository,
                                  never touching your checkout; it asks (y/N) before files that run code, and needs a terminal for that
  watch <room> [--tail N]         follow a room
  post <room> [--queue] <text>    chat, or queue it for the next run's brief
  skill install [--dir .agents/skills]   write the factory-handoff Agent Skill for local coding agents
  mcp                             the factory's MCP tools on stdio, for a local coding agent:
                                  claude mcp add factory -- roomctl mcp
  setup <claude|opencode|codex|gemini|cursor|copilot> [--project] [--print]
                                  register roomctl mcp with that agent (through its own CLI, else the
                                  snippet to paste) and install the factory-handoff skill; --print writes nothing
  fork <room> --at SEQ [--role R] [--pr URL] [--egress pypi,npm] [--note TEXT]
                                  a room of your own from events 1..SEQ, with an optional run
roomctl never steers, interrupts, moves the driver token or decides (ruling P18): use the web UI`

// Roomctl is roomctl's wiring: where its files live, and its two HTTP clients.
type Roomctl struct {
	Dir    string       // config.json and token.json, both owner-only
	HC     *http.Client // the room list and the IdP: bounded
	Stream *http.Client // the WebSocket: bounded by the context, not a whole-exchange timeout
	In     io.Reader    // mcp's messages; nil is stdin
	Out    io.Writer
	Err    io.Writer // hints; nil is stderr

	// setup's seams; the zero values are the real home, working directory, PATH and exec.
	Home, Cwd string
	LookPath  func(string) (string, error)
	RunCmd    func(ctx context.Context, argv []string) error
}

// RunRoomctl runs one roomctl command (SP2 §8), its files under roomctl.Dir and
// its egress through httpx.
func RunRoomctl(ctx context.Context, args []string, stdout io.Writer) error {
	dir, err := roomctl.Dir()
	if err != nil {
		return err
	}
	stream := httpx.NewStreaming(roomctlTimeout, nil) // a watch lasts as long as the developer wants it
	return Roomctl{Dir: dir, HC: httpx.New(roomctlTimeout, nil), Stream: stream, Out: stdout}.Run(ctx, args)
}

// flags parses args with fs, flags anywhere among the positional arguments, as
// the usage writes them (`post <room> --queue <text>`).
func flags(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, fmt.Errorf("%s: %w\n%s", fs.Name(), err, roomctlUsage)
		}
		if args = fs.Args(); len(args) == 0 {
			return pos, nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

// Run runs one command. A transport failure leaves it naming its cause and fix (#48).
func (r Roomctl) Run(ctx context.Context, args []string) error {
	return roomctl.Cause(r.run(ctx, args))
}

func (r Roomctl) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("a command is required\n%s", roomctlUsage)
	}
	cmd, args := args[0], args[1:]
	cfgPath, tokPath := filepath.Join(r.Dir, "config.json"), filepath.Join(r.Dir, "token.json")
	switch cmd {
	case "configure":
		return r.configure(ctx, cfgPath, args)
	case "skill":
		return r.skill(args)
	case "setup":
		return r.setup(ctx, args)
	case "help", "-h", "--help":
		_, err := fmt.Fprintln(r.Out, roomctlUsage)
		return err
	case "login", "token", "rooms", "status", "changes", "checkout", "watch", "post", "fork", "doctor", "mcp":
	default:
		return fmt.Errorf("unknown command %q\n%s", cmd, roomctlUsage)
	}
	var cfg roomctl.Config
	if err := roomctl.LoadJSON(cfgPath, &cfg); err != nil {
		return errors.New("not configured: run roomctl configure --url https://rooms.<your private domain>")
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("%s: %w: run roomctl configure again", cfgPath, err)
	}
	if cmd == "login" {
		tok, err := roomctl.Login(ctx, r.HC, cfg, r.Out)
		if err != nil {
			return err
		}
		if err := roomctl.SaveJSON(tokPath, tok); err != nil {
			return err
		}
		_, err = fmt.Fprintln(r.Out, "logged in; roomctl rooms lists the rooms you can read, roomctl doctor checks the setup")
		return err
	}
	if cmd == "doctor" {
		dirs, err := r.skillDirs(ctx)
		if err != nil {
			return err
		}
		return roomctl.Doctor{Config: cfg, HC: r.HC, TokenPath: tokPath,
			SkillDirs: dirs, Version: version.Version}.Run(ctx, r.Out)
	}
	if cmd == "mcp" {
		return r.mcp(ctx, cfg, tokPath, args)
	}
	c := roomctl.Client{URL: cfg.URL, HC: r.HC, Stream: r.Stream, Issuer: cfg.Issuer, Err: r.Err,
		Token: func(ctx context.Context) (string, error) { return roomctl.Token(ctx, r.HC, cfg, tokPath) }}
	switch cmd {
	case "token":
		// For scripts: the human's own JWT access token, never a stored secret (ruling TU).
		tok, err := c.Token(ctx)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(r.Out, tok)
		return err
	case "rooms":
		fs := flag.NewFlagSet("rooms", flag.ContinueOnError)
		var f roomctl.RoomFilter
		fs.StringVar(&f.Repo, "repo", "", "only this repository, owner/name")
		fs.BoolVar(&f.Mine, "mine", false, "only the tasks of issues you filed or labelled, PRs you authored or review")
		fs.BoolVar(&f.NeedsMe, "needs-me", false, "only rooms with an approval you could decide, a task held on your room, or a PR waiting for your merge")
		fs.BoolVar(&f.JSON, "json", false, "print the broker's rows as is")
		fs.BoolVar(&f.All, "all", false, "the rooms that ended over a week ago too")
		format := fs.String("format", "", "md or text")
		pos, err := flags(fs, args)
		if err != nil || len(pos) != 0 {
			return fmt.Errorf("rooms [--repo owner/name] [--mine] [--needs-me] [--all] [--format md|text] [--json]: %w", errors.Join(err, errors.New("no arguments")))
		}
		if f.Format, err = viewFormat(r.Out, *format); err != nil {
			return err
		}
		return c.Rooms(ctx, r.Out, f)
	case "status":
		return r.status(ctx, c, args)
	case "changes":
		return r.changes(ctx, c, filepath.Join(r.Dir, "cursors.json"), args)
	case "checkout":
		return r.checkout(ctx, c, args)
	case "watch":
		fs := flag.NewFlagSet("watch", flag.ContinueOnError)
		tail := fs.Int("tail", 50, "events to show first")
		pos, err := flags(fs, args)
		if err != nil || len(pos) != 1 {
			return fmt.Errorf("watch <room> [--tail N]: %w", errors.Join(err, errors.New("one room")))
		}
		return c.Watch(ctx, pos[0], *tail, r.Out)
	case "post":
		return r.post(ctx, c, args)
	}
	return r.fork(ctx, c, cfg.URL, args)
}

// configure saves the config. The URL alone is enough: whatever the flags leave empty is read from
// the broker, which serves the three values unauthenticated (#48).
func (r Roomctl) configure(ctx context.Context, path string, args []string) error {
	var cfg roomctl.Config
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	fs.StringVar(&cfg.URL, "url", "", "https://rooms.<private domain>")
	fs.StringVar(&cfg.Issuer, "issuer", "", "the ZITADEL issuer URL (default: the broker's)")
	fs.StringVar(&cfg.ClientID, "client-id", "", "the roomctl client id (default: the broker's)")
	fs.StringVar(&cfg.ProjectID, "project-id", "", "the rooms project id (default: the broker's)")
	if _, err := flags(fs, args); err != nil {
		return err
	}
	cfg.URL, cfg.Issuer = strings.TrimRight(cfg.URL, "/"), strings.TrimRight(cfg.Issuer, "/")
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.ProjectID == "" {
		if u, err := url.Parse(cfg.URL); err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("configure: url %q is not an https URL: roomctl configure --url https://rooms.<your private domain>", cfg.URL)
		}
		hc := r.HC
		if hc == nil {
			hc = httpx.New(roomctlTimeout, nil)
		}
		found, err := roomctl.Discover(ctx, hc, cfg.URL)
		if err != nil {
			return fmt.Errorf("configure: %w", err)
		}
		cfg.Issuer, cfg.ClientID, cfg.ProjectID = cmp.Or(cfg.Issuer, found.Issuer), cmp.Or(cfg.ClientID, found.ClientID), cmp.Or(cfg.ProjectID, found.ProjectID)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	if err := roomctl.SaveJSON(path, cfg); err != nil {
		return err
	}
	// A hint, on the hint stream: stdout stays what a script can read.
	hints := r.Err
	if hints == nil {
		hints = os.Stderr
	}
	_, err := fmt.Fprintf(hints, "configured %s; next: roomctl login\n", cfg.URL)
	return err
}

// mcp relays the broker's MCP server on stdio until the client closes it: one token source for
// every request, so a refresh happens once (factory interfaces F2).
func (r Roomctl) mcp(ctx context.Context, cfg roomctl.Config, tokPath string, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("mcp: no arguments\n%s", roomctlUsage)
	}
	in := r.In
	if in == nil {
		in = os.Stdin
	}
	relay := &roomctl.Relay{URL: cfg.URL, HC: r.HC, Token: roomctl.SharedToken(r.HC, cfg, tokPath), File: roomctl.GH, Label: roomctl.GHLabel}
	return relay.Serve(ctx, in, r.Out)
}

// skill installs the factory-handoff Agent Skill; it needs no configuration or login.
func (r Roomctl) skill(args []string) error {
	fs := flag.NewFlagSet("skill install", flag.ContinueOnError)
	dir := fs.String("dir", filepath.Join(".agents", "skills"), "where the skills live in the repo")
	pos, err := flags(fs, args)
	if err != nil || len(pos) != 1 || pos[0] != "install" {
		return fmt.Errorf("skill install [--dir .agents/skills]: %w", errors.Join(err, errors.New("the install subcommand")))
	}
	paths, err := skill.Install(*dir, version.Version)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if _, err := fmt.Fprintln(r.Out, p); err != nil {
			return err
		}
	}
	return nil
}

// homeAndCwd is the home directory and the working directory, the process's when not set.
func (r Roomctl) homeAndCwd() (home, cwd string, err error) {
	if home, cwd = r.Home, r.Cwd; home == "" {
		if home, err = os.UserHomeDir(); err != nil {
			return "", "", err
		}
	}
	if cwd == "" {
		if cwd, err = os.Getwd(); err != nil {
			return "", "", err
		}
	}
	return home, cwd, nil
}

// skillDirs is where the doctor looks: under home, and under the repository top, as setup writes
// them. Outside a repository the project scope is skipped.
func (r Roomctl) skillDirs(ctx context.Context) ([]string, error) {
	home, cwd, err := r.homeAndCwd()
	if err != nil {
		return nil, err
	}
	top, err := repoTop(ctx, cwd)
	if err != nil {
		top = ""
	}
	return roomctl.SkillDirs(home, top), nil
}

// repoTop is the top of the repository holding dir: --project files belong to the repository,
// whatever subdirectory the developer is in.
func repoTop(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("--project needs a git repository: %s is not inside one (run setup from the repository, or drop --project for a user-level setup)", dir)
	}
	return strings.TrimSpace(string(out)), nil
}

// setup registers roomctl mcp with a coding agent and installs the skill; it needs no configuration.
func (r Roomctl) setup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	project := fs.Bool("project", false, "register and install in this repository, not for the user")
	dry := fs.Bool("print", false, "print what setup would do, and write nothing")
	pos, err := flags(fs, args)
	if err != nil || len(pos) != 1 {
		return fmt.Errorf("setup <claude|opencode|codex|gemini|cursor|copilot> [--project] [--print]: %w", errors.Join(err, errors.New("one client")))
	}
	agent, err := roomctl.LookupAgent(pos[0])
	if err != nil {
		return err
	}
	s := roomctl.Setup{Agent: agent, Project: *project, Print: *dry, Home: r.Home, Dir: r.Cwd, Version: version.Version,
		LookPath: r.LookPath, Run: r.RunCmd, Out: r.Out}
	if s.LookPath == nil {
		s.LookPath = exec.LookPath
	}
	if s.Run == nil {
		s.Run = func(ctx context.Context, argv []string) error {
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv[0] is one of six fixed CLI names
			// The output rides in the error: setup reads "already exists" from it.
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		}
	}
	if s.Home, s.Dir, err = r.homeAndCwd(); err != nil {
		return err
	}
	if s.Project {
		if s.Dir, err = repoTop(ctx, s.Dir); err != nil {
			return err
		}
	}
	s.Cmd = roomctl.SetupCmd(s.LookPath, os.Executable)
	return s.Do(ctx)
}

// status prints where a room stands; --json is the broker's body unchanged.
func (r Roomctl) status(ctx context.Context, c roomctl.Client, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the broker's summary/v1 as is")
	cursor := fs.String("after", "0", "only notes after this cursor: the summary's seq:N, or N")
	format := fs.String("format", "", "md or text")
	pos, err := flags(fs, args)
	// The summary prints its cursor as seq:N, so the next call can pass it back as is.
	after, aerr := strconv.ParseInt(strings.TrimPrefix(*cursor, "seq:"), 10, 64)
	if err != nil || len(pos) != 1 || aerr != nil || after < 0 {
		return fmt.Errorf("status <room> [--json] [--after seq:N] [--format md|text]: %w", errors.Join(err, errors.New("one room, and a cursor of seq:N or N")))
	}
	f, err := viewFormat(r.Out, *format)
	if err != nil {
		return err
	}
	raw, err := c.Summary(ctx, pos[0], after)
	if err != nil {
		return err
	}
	if *asJSON {
		_, err = fmt.Fprintln(r.Out, string(raw))
		return err
	}
	var sum summary.Summary
	if err := json.Unmarshal(raw, &sum); err != nil {
		return fmt.Errorf("status: the broker's answer: %w", err)
	}
	if err := roomview.Card(r.Out, sum, roomview.Options{Format: f, Now: time.Now(), Rooms: roomsBase(sum, c.URL)}); err != nil {
		return err
	}
	// The card has no cursor: status prints it last, so --after can take it back.
	_, err = fmt.Fprintf(r.Out, "cursor: %s\n", roomctl.OneLine(sum.Cursor))
	return err
}

// roomsBase is the base of the room links the card may print: the broker's PublicURL, which the
// summary's own URL ends with ("/r/<room>"), else the URL roomctl is configured with. A broker
// reached by another name (a port-forward) still gets its links.
func roomsBase(s summary.Summary, configured string) string {
	if base, ok := strings.CutSuffix(s.URL, "/r/"+s.Room); ok && s.Room != "" && strings.HasPrefix(base, "https://") {
		return base
	}
	return configured
}

// textFormat is TextColor when w is a terminal and NO_COLOR is unset, else Text.
func textFormat(w io.Writer) roomview.Format {
	f, ok := w.(*os.File)
	if !ok || os.Getenv("NO_COLOR") != "" {
		return roomview.Text
	}
	if fi, err := f.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return roomview.Text
	}
	return roomview.TextColor
}

// post chats, or queues a message for the next run's brief: never steering.
func (r Roomctl) post(ctx context.Context, c roomctl.Client, args []string) error {
	fs := flag.NewFlagSet("post", flag.ContinueOnError)
	queue := fs.Bool("queue", false, "queue it for the next run's brief instead of chatting")
	pos, err := flags(fs, args)
	if err != nil || len(pos) < 2 {
		return fmt.Errorf("post <room> [--queue] <text>: %w", errors.Join(err, errors.New("a room and a text")))
	}
	delivery := "none"
	if *queue {
		delivery = "queued"
	}
	ctx, cancel := context.WithTimeout(ctx, actTimeout)
	defer cancel()
	f, err := c.Act(ctx, pos[0], map[string]any{"kind": "message", "delivery": delivery, "text": strings.Join(pos[1:], " ")})
	if err != nil {
		return err
	}
	if f.Rejected != "" {
		return fmt.Errorf("rejected: %s", roomctl.Explain(f.Rejected))
	}
	_, err = fmt.Fprintln(r.Out, "seq", f.Seq)
	return err
}

// fork asks for a room of the developer's own from events 1..at, and a run in it with --role.
func (r Roomctl) fork(ctx context.Context, c roomctl.Client, base string, args []string) error {
	fs := flag.NewFlagSet("fork", flag.ContinueOnError)
	at := fs.Int64("at", 0, "the last seq to copy")
	role := fs.String("role", "", "request a run in the new room: implementer, reviewer, tester or triager")
	pr := fs.String("pr", "", "the reviewer's pull request")
	egress := fs.String("egress", "", "extra egress profiles for that run: pypi,npm,golang,crates")
	note := fs.String("note", "", "why you fork")
	pos, err := flags(fs, args)
	if err != nil || len(pos) != 1 || *at < 1 {
		return fmt.Errorf("fork <room> --at SEQ: %w", errors.Join(err, errors.New("one room and a seq of 1 or more")))
	}
	action := map[string]any{"kind": "fork", "seq": *at}
	for k, v := range map[string]string{"role": *role, "prUrl": *pr, "note": *note} {
		if v != "" {
			action[k] = v
		}
	}
	if *egress != "" {
		action["egressProfiles"] = strings.Split(*egress, ",")
	}
	ctx, cancel := context.WithTimeout(ctx, actTimeout)
	defer cancel()
	f, err := c.Act(ctx, pos[0], action)
	if err != nil {
		return err
	}
	if f.Rejected != "" {
		return fmt.Errorf("rejected: %s", roomctl.Explain(f.Rejected))
	}
	var res struct {
		RoomID   string          `json:"roomId"`
		Run      json.RawMessage `json:"run"`
		RunError string          `json:"runError"`
	}
	if err := json.Unmarshal(f.Result, &res); err != nil {
		return fmt.Errorf("fork: the broker's answer: %w", err)
	}
	out := fmt.Sprintf("forked into %s: %s/r/%s\n", res.RoomID, base, res.RoomID)
	if res.RunError != "" {
		out += "its run was not started: " + roomctl.Explain(res.RunError) + "\n"
	}
	if claim, err := json.MarshalIndent(res.Run, "", "  "); err == nil && len(res.Run) > 0 && string(res.Run) != "null" {
		out += "# Before SP3 the owner creates the run (C3): kubectl create -f - with\n" + string(claim) + "\n"
	}
	_, err = io.WriteString(r.Out, roomctl.SafeText(out))
	return err
}

// checkout makes the worktree of a room's branch. The repository and the pull request come from the
// room's row. The files that run code are confirmed with y/N on a terminal, and refused without one.
func (r Roomctl) checkout(ctx context.Context, c roomctl.Client, args []string) error {
	pos, err := flags(flag.NewFlagSet("checkout", flag.ContinueOnError), args)
	if err != nil || len(pos) != 1 {
		return fmt.Errorf("checkout <room>: %w", errors.Join(err, errors.New("one room")))
	}
	rows, _, err := c.RoomRows(ctx, roomctl.RoomFilter{All: true})
	if err != nil {
		return err
	}
	i := slices.IndexFunc(rows, func(row roomview.Row) bool { return row.ID == pos[0] })
	if i < 0 {
		return fmt.Errorf("checkout: %s is not one of the rooms you can read: roomctl rooms --all", roomctl.SafeText(pos[0]))
	}
	dir := r.Cwd
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return err
		}
	}
	hints := r.Err
	if hints == nil {
		hints = os.Stderr
	}
	in := r.In
	if in == nil {
		in = os.Stdin
	}
	co := roomctl.Checkout{Workspace: dir, Room: pos[0], Repository: rows[i].Repository, PR: rows[i].PR}
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) { //nolint:gosec // a file descriptor fits an int
		co.Confirm = func(_ context.Context, files []string) (bool, error) {
			_, _ = fmt.Fprintf(hints, "agent/%s changes files that run code on your machine:\n  %s\nCreate the worktree? [y/N] ", pos[0], strings.Join(files, "\n  "))
			line, err := bufio.NewReader(io.LimitReader(f, 64)).ReadString('\n')
			if err != nil && line == "" {
				return false, err
			}
			return strings.EqualFold(strings.TrimSpace(line), "y"), nil
		}
	}
	got, err := co.Run(ctx)
	var refusal *roomctl.CheckoutRefusal
	if errors.As(err, &refusal) {
		return fmt.Errorf("checkout: %s", roomctl.SafeText(refusal.Error()))
	}
	if err != nil {
		return err
	}
	note := "created"
	if got.Reused {
		note = "already there"
	}
	_, _ = fmt.Fprintf(hints, "%s: %s%s\n", got.Branch, note, map[bool]string{true: ", " + got.PR, false: ""}[got.PR != ""])
	_, err = fmt.Fprintln(r.Out, got.Path)
	return err
}
