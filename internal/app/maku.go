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
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
	"golang.org/x/term"

	"github.com/Smana/maku/internal/httpx"
	"github.com/Smana/maku/internal/maku"
	"github.com/Smana/maku/internal/maku/skill"
	"github.com/Smana/maku/internal/roomview"
	"github.com/Smana/maku/internal/summary"
	"github.com/Smana/maku/internal/version"
)

// makuTimeout bounds one call to the broker or the IdP; actTimeout one act.
const (
	makuTimeout = 30 * time.Second
	actTimeout  = time.Minute
)

const makuUsage = `usage: maku <command>
  configure --url URL [--issuer URL --client-id ID --project-id ID]
                                  the broker's URL is enough: it serves the other three. A local install
                                  is http://localhost:<forwarded port>
  configure --from URL [--yes]    replace the whole config with the values the broker serves (a rooms page
                                  URL works): the fix for a stale client id after a rebuild
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
                                  claude mcp add factory -- maku mcp
  setup <claude|opencode|codex|gemini|cursor|copilot> [--project] [--print] [--restart-service]
                                  register maku mcp with that agent (through its own CLI, else the
                                  snippet to paste) and install the factory-handoff skill; --print writes nothing
  fork <room> --at SEQ [--role R] [--pr URL] [--egress pypi,npm] [--note TEXT] [--approvals attended|unattended]
                                  a room of your own from events 1..SEQ, with an optional run; the
                                  fork asks before risky actions (attended) unless you pass --approvals
                                  unattended and its source is unattended too
  run --repo owner/name --task TEXT [--role implementer|tester|triager]
                                  quickstart only (a local broker): create a room, queue TEXT as the run's
                                  task, start the run, apply its claim with your kubectl, follow it to the end
maku never steers, interrupts, moves the driver token or decides (ruling P18): use the web UI`

// Maku is maku's wiring: where its files live, and its two HTTP clients.
type Maku struct {
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

	// Apply creates a run's claim (maku run); nil runs kubectl apply -f - with the manifest on stdin.
	Apply func(ctx context.Context, manifest []byte) error

	// Capture runs a command and returns its output (setup's OpenCode service check); nil runs it.
	Capture func(ctx context.Context, argv []string) (string, error)

	// Ask puts a yes/no question to the developer; nil asks on the terminal when In is one, else
	// nothing is asked. Tests set it.
	Ask func(prompt string) (bool, error)
}

// RunMaku runs one maku command (SP2 §8): its files under maku.Dir; a pre-v0.30.0 CLI's directory
// (formerly `roomctl/`) is imported there once, and its egress goes through httpx.
func RunMaku(ctx context.Context, args []string, stdout io.Writer) error {
	return runMaku(ctx, args, stdout, os.Stderr)
}

// runMaku is RunMaku with its stderr injected. A failed import is a warning: the old CLI's files are a
// convenience, and a developer can always configure and log in again.
func runMaku(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	dir, err := maku.Dir()
	if err != nil {
		return err
	}
	if _, err := maku.ImportLegacy(filepath.Dir(dir), stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "maku: roomctl's config was not imported (%s): run maku configure and maku login if maku asks\n", maku.OneLine(err.Error()))
	}
	stream := httpx.NewStreaming(makuTimeout, nil) // a watch lasts as long as the developer wants it
	return Maku{Dir: dir, HC: httpx.New(makuTimeout, nil), Stream: stream, Out: stdout, Err: stderr}.Run(ctx, args)
}

// flags parses args with fs, flags anywhere among the positional arguments, as
// the usage writes them (`post <room> --queue <text>`).
func flags(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, fmt.Errorf("%s: %w\n%s", fs.Name(), err, makuUsage)
		}
		if args = fs.Args(); len(args) == 0 {
			return pos, nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

// Run runs one command. A transport failure leaves it naming its cause and fix (#48).
func (r Maku) Run(ctx context.Context, args []string) error {
	return maku.Cause(r.run(ctx, args))
}

func (r Maku) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("a command is required\n%s", makuUsage)
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
		_, err := fmt.Fprintln(r.Out, makuUsage)
		return err
	case "login", "token", "rooms", "status", "changes", "checkout", "watch", "post", "fork", "run", "doctor", "mcp":
	default:
		return fmt.Errorf("unknown command %q\n%s", cmd, makuUsage)
	}
	var cfg maku.Config
	if err := maku.LoadJSON(cfgPath, &cfg); err != nil {
		return errors.New("not configured: run maku configure --url https://rooms.<your private domain>")
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("%s: %w: run maku configure again", cfgPath, err)
	}
	if cmd == "login" {
		if cfg.HumanMode == maku.HumanModeLocal {
			// A local broker has no IdP: naming the port-forward is the whole of the login.
			_, err := fmt.Fprintln(r.Out, "local broker; no login needed")
			return err
		}
		tok, err := maku.Login(ctx, r.HC, cfg, r.Out)
		if err != nil {
			return err
		}
		if err := maku.SaveJSON(tokPath, tok); err != nil {
			return err
		}
		_, err = fmt.Fprintln(r.Out, "logged in; maku rooms lists the rooms you can read, maku doctor checks the setup")
		return err
	}
	if cmd == "doctor" {
		dirs, err := r.skillDirs(ctx)
		if err != nil {
			return err
		}
		home, cwd, herr := r.homeAndCwd()
		if herr != nil {
			return herr
		}
		top, terr := repoTop(ctx, cwd)
		if terr != nil {
			top = "" // outside a repository: no project-scoped entry to check
		}
		return maku.Doctor{Config: cfg, HC: r.HC, TokenPath: tokPath,
			SkillDirs: dirs, Version: version.Version,
			OpenCodeConfigs: maku.OpenCodeConfigs(home), ServiceStart: opencodeServiceStart(ctx),
			LegacyMCP: maku.LegacyEntries(home, top)}.Run(ctx, r.Out)
	}
	if cmd == "mcp" {
		return r.mcp(ctx, cfg, tokPath, args)
	}
	c := maku.Client{URL: cfg.URL, HC: r.HC, Stream: r.Stream, Issuer: cfg.Issuer, Err: r.Err,
		Token: func(ctx context.Context) (string, error) { return maku.Token(ctx, r.HC, cfg, tokPath) }}
	switch cmd {
	case "token":
		if cfg.HumanMode == maku.HumanModeLocal {
			// A local broker takes no token: a script pointing at it sends none, and there is none to print.
			_, err := fmt.Fprintln(r.Out, "local broker: no token required")
			return err
		}
		// For scripts: the human's own JWT access token, never a stored secret (ruling TU).
		tok, err := c.Token(ctx)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(r.Out, tok)
		return err
	case "rooms":
		fs := flag.NewFlagSet("rooms", flag.ContinueOnError)
		var f maku.RoomFilter
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
	case "run":
		return r.quickstartRun(ctx, cfg, c, args)
	}
	return r.fork(ctx, c, cfg.URL, args)
}

// configure saves the config. The URL alone is enough: whatever the flags leave empty is read from
// the broker, which serves the three values unauthenticated (#48).
func (r Maku) configure(ctx context.Context, path string, args []string) error {
	var cfg maku.Config
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	fs.StringVar(&cfg.URL, "url", "", "https://rooms.<private domain>, or http://localhost:<port> for a local install")
	from := fs.String("from", "", "a rooms URL: take every value from the broker, whatever is stored")
	yes := fs.Bool("yes", false, "with --from: write without asking, and accept an issuer on another domain than the URL")
	fs.StringVar(&cfg.Issuer, "issuer", "", "the ZITADEL issuer URL (default: the broker's)")
	fs.StringVar(&cfg.ClientID, "client-id", "", "the maku client id (default: the broker's)")
	fs.StringVar(&cfg.ProjectID, "project-id", "", "the rooms project id (default: the broker's)")
	if _, err := flags(fs, args); err != nil {
		return err
	}
	if *from != "" {
		if cfg.URL != "" || cfg.Issuer != "" || cfg.ClientID != "" || cfg.ProjectID != "" {
			return errors.New("configure: --from takes every value from the broker: drop the other flags, or use --url to override one")
		}
		base, err := originOf(*from)
		if err != nil {
			return fmt.Errorf("configure: %w", err)
		}
		cfg.URL = base
	}
	cfg.URL, cfg.Issuer = strings.TrimRight(cfg.URL, "/"), strings.TrimRight(cfg.Issuer, "/")
	// A plain-http broker is a local install's loopback listener: refused before any request unless it
	// is on this machine, whatever the answer would say (Discover enforces it too, for its callers).
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("configure: url %q is not an http(s) URL: maku configure --url https://rooms.<your private domain>", cfg.URL)
	}
	if u.Scheme == "http" && !maku.LoopbackURL(u) {
		return fmt.Errorf("configure: url %q: a plain-http broker is only a local install's loopback listener "+
			"(http://localhost:<port>): use the https rooms URL", cfg.URL)
	}
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.ProjectID == "" {
		hc := r.HC
		if hc == nil {
			hc = httpx.New(makuTimeout, nil)
		}
		found, err := maku.Discover(ctx, hc, cfg.URL)
		if err != nil {
			return fmt.Errorf("configure: %w", err)
		}
		cfg.Issuer, cfg.ClientID, cfg.ProjectID = cmp.Or(cfg.Issuer, found.Issuer), cmp.Or(cfg.ClientID, found.ClientID), cmp.Or(cfg.ProjectID, found.ProjectID)
		cfg.HumanMode = found.HumanMode
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	if *from != "" {
		var old maku.Config
		_ = maku.LoadJSON(path, &old) // none yet is fine
		if err := r.ConfirmConfig(cfg, old, *yes); err != nil {
			return fmt.Errorf("configure: %w", err)
		}
	}
	if err := maku.SaveJSON(path, cfg); err != nil {
		return err
	}
	// A hint, on the hint stream: stdout stays what a script can read.
	hints := r.Err
	if hints == nil {
		hints = os.Stderr
	}
	next := "next: maku login"
	if cfg.HumanMode == maku.HumanModeLocal {
		next = "a local broker needs no login; next: maku rooms"
	}
	_, err = fmt.Fprintf(hints, "configured %s; %s\n", cfg.URL, next)
	return err
}

// ConfirmConfig shows what `configure --from` will write, and what it replaces, then decides: an
// issuer outside the URL's registrable domain is refused unless yes; a terminal is asked unless yes.
// The values come from the broker's answer, so the developer sees the two a hostile one would plant.
func (r Maku) ConfirmConfig(next, old maku.Config, yes bool) error {
	hints := r.Err
	if hints == nil {
		hints = os.Stderr
	}
	line := func(name, now, was string) {
		note := ""
		if was != "" && was != now {
			note = " (was " + maku.SafeText(was) + ")"
		}
		_, _ = fmt.Fprintf(hints, "  %-10s %s%s\n", name, maku.SafeText(now), note)
	}
	_, _ = fmt.Fprintln(hints, "maku configure --from will write:")
	line("url", next.URL, old.URL)
	line("issuer", next.Issuer, old.Issuer)
	line("client id", next.ClientID, old.ClientID)
	line("project id", next.ProjectID, old.ProjectID)
	if !sameRegistrableDomain(next.URL, next.Issuer) {
		_, _ = fmt.Fprintf(hints, "warning: the issuer %s is not under the same domain as %s: a broker could point you at an identity provider of its own\n",
			maku.SafeText(next.Issuer), maku.SafeText(next.URL))
		if !yes {
			return errors.New("the issuer is on another domain than the URL: check it above, then pass --yes if it is yours")
		}
	}
	if yes {
		return nil
	}
	ask := r.asker()
	if ask == nil {
		return nil // a script: --yes is what it would pass, and the values are above
	}
	ok, err := ask("Write this configuration? [y/N] ")
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("not written: the configuration is unchanged")
	}
	return nil
}

// asker asks a yes/no question on the terminal, or is r.Ask; nil when stdin is no terminal.
func (r Maku) asker() func(string) (bool, error) {
	if r.Ask != nil {
		return r.Ask
	}
	in := r.In
	if in == nil {
		in = os.Stdin
	}
	f, ok := in.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) { //nolint:gosec // a file descriptor fits an int
		return nil
	}
	hints := r.Err
	if hints == nil {
		hints = os.Stderr
	}
	return func(prompt string) (bool, error) {
		_, _ = fmt.Fprint(hints, prompt)
		line, err := bufio.NewReader(io.LimitReader(f, 64)).ReadString('\n')
		if err != nil && line == "" {
			return false, err
		}
		return strings.EqualFold(strings.TrimSpace(line), "y"), nil
	}
}

// sameRegistrableDomain is whether two URLs' hosts share a registrable domain (rooms.ogenki.io and
// auth.ogenki.io do). A host with none (an address, localhost) matches only itself.
func sameRegistrableDomain(a, b string) bool {
	host := func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		return strings.ToLower(u.Hostname())
	}
	ha, hb := host(a), host(b)
	if ha == "" || hb == "" {
		return false
	}
	if ha == hb {
		return true
	}
	da, errA := publicsuffix.EffectiveTLDPlusOne(ha)
	db, errB := publicsuffix.EffectiveTLDPlusOne(hb)
	return errA == nil && errB == nil && da == db
}

// originOf is the https origin of a rooms URL: the room list's or a room page's address both name
// the broker. A plain-http URL would carry the token in clear.
func originOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", fmt.Errorf("--from %q is not an https URL: maku configure --from https://rooms.<your private domain>", raw)
	}
	return "https://" + u.Host, nil
}

// mcp relays the broker's MCP server on stdio until the client closes it: one token source for
// every request, so a refresh happens once (factory interfaces F2).
func (r Maku) mcp(ctx context.Context, cfg maku.Config, tokPath string, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("mcp: no arguments\n%s", makuUsage)
	}
	in := r.In
	if in == nil {
		in = os.Stdin
	}
	hints := r.Err
	if hints == nil {
		hints = os.Stderr
	}
	relay := &maku.Relay{URL: cfg.URL, HC: r.HC, Token: maku.SharedToken(r.HC, cfg, tokPath), File: maku.GH, Label: maku.GHLabel,
		Log: slog.New(slog.NewTextHandler(hints, nil))}
	return relay.Serve(ctx, in, r.Out)
}

// skill installs the factory-handoff Agent Skill; it needs no configuration or login.
func (r Maku) skill(args []string) error {
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
func (r Maku) homeAndCwd() (home, cwd string, err error) {
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
func (r Maku) skillDirs(ctx context.Context) ([]string, error) {
	home, cwd, err := r.homeAndCwd()
	if err != nil {
		return nil, err
	}
	top, err := repoTop(ctx, cwd)
	if err != nil {
		top = ""
	}
	return maku.SkillDirs(home, top), nil
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

// setup registers maku mcp with a coding agent and installs the skill; it needs no configuration.
func (r Maku) setup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	project := fs.Bool("project", false, "register and install in this repository, not for the user")
	dry := fs.Bool("print", false, "print what setup would do, and write nothing")
	restart := fs.Bool("restart-service", false, "opencode: restart its background service if it runs, without asking")
	pos, err := flags(fs, args)
	if err != nil || len(pos) != 1 {
		return fmt.Errorf("setup <claude|opencode|codex|gemini|cursor|copilot> [--project] [--print] [--restart-service]: %w", errors.Join(err, errors.New("one client")))
	}
	agent, err := maku.LookupAgent(pos[0])
	if err != nil {
		return err
	}
	s := maku.Setup{Agent: agent, Project: *project, Print: *dry, Home: r.Home, Dir: r.Cwd, Version: version.Version,
		LookPath: r.LookPath, Run: r.RunCmd, Out: r.Out, Capture: r.Capture, Ask: r.asker(), RestartService: *restart}
	if s.Capture == nil {
		s.Capture = func(ctx context.Context, argv []string) (string, error) {
			out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output() //nolint:gosec // argv is opencode service status
			return string(out), err
		}
	}
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
	s.Cmd = maku.SetupCmd(s.LookPath, os.Executable)
	return s.Do(ctx)
}

// status prints where a room stands; --json is the broker's body unchanged.
func (r Maku) status(ctx context.Context, c maku.Client, args []string) error {
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
	_, err = fmt.Fprintf(r.Out, "cursor: %s\n", maku.OneLine(sum.Cursor))
	return err
}

// roomsBase is the base of the room links the card may print: the broker's PublicURL, which the
// summary's own URL ends with ("/r/<room>"), else the URL maku is configured with. A broker
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
func (r Maku) post(ctx context.Context, c maku.Client, args []string) error {
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
		return fmt.Errorf("rejected: %s", maku.Explain(f.Rejected))
	}
	_, err = fmt.Fprintln(r.Out, "seq", f.Seq)
	return err
}

// fork asks for a room of the developer's own from events 1..at, and a run in it with --role.
func (r Maku) fork(ctx context.Context, c maku.Client, base string, args []string) error {
	fs := flag.NewFlagSet("fork", flag.ContinueOnError)
	at := fs.Int64("at", 0, "the last seq to copy")
	role := fs.String("role", "", "request a run in the new room: implementer, reviewer or triager")
	pr := fs.String("pr", "", "the reviewer's pull request")
	egress := fs.String("egress", "", "extra egress profiles for that run: pypi,npm,golang,crates")
	note := fs.String("note", "", "why you fork")
	approvals := fs.String("approvals", "", "attended (the default): the fork asks before risky actions; unattended keeps an unattended source's policy")
	pos, err := flags(fs, args)
	if err == nil && *approvals != "" && *approvals != "attended" && *approvals != "unattended" {
		err = errors.New("--approvals is attended or unattended")
	}
	if err != nil || len(pos) != 1 || *at < 1 {
		return fmt.Errorf("fork <room> --at SEQ: %w", errors.Join(err, errors.New("one room and a seq of 1 or more")))
	}
	action := map[string]any{"kind": "fork", "seq": *at}
	for k, v := range map[string]string{"role": *role, "prUrl": *pr, "note": *note, "approvals": *approvals} {
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
		return fmt.Errorf("rejected: %s", maku.Explain(f.Rejected))
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
		out += "its run was not started: " + maku.Explain(res.RunError) + "\n"
	}
	if claim, err := json.MarshalIndent(res.Run, "", "  "); err == nil && len(res.Run) > 0 && string(res.Run) != "null" {
		out += "# Before SP3 the owner creates the run (C3): kubectl create -f - with\n" + string(claim) + "\n"
	}
	_, err = io.WriteString(r.Out, maku.SafeText(out))
	return err
}

// checkout makes the worktree of a room's branch. The repository and the pull request come from the
// room's row. The files that run code are confirmed with y/N on a terminal, and refused without one.
func (r Maku) checkout(ctx context.Context, c maku.Client, args []string) error {
	pos, err := flags(flag.NewFlagSet("checkout", flag.ContinueOnError), args)
	if err != nil || len(pos) != 1 {
		return fmt.Errorf("checkout <room>: %w", errors.Join(err, errors.New("one room")))
	}
	rows, _, err := c.RoomRows(ctx, maku.RoomFilter{All: true})
	if err != nil {
		return err
	}
	i := slices.IndexFunc(rows, func(row roomview.Row) bool { return row.ID == pos[0] })
	if i < 0 {
		return fmt.Errorf("checkout: %s is not one of the rooms you can read: maku rooms --all", maku.SafeText(pos[0]))
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
	co := maku.Checkout{Workspace: dir, Room: pos[0], Repository: rows[i].Repository, PR: rows[i].PR}
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) { //nolint:gosec // a file descriptor fits an int
		co.Confirm = func(_ context.Context, files []string, more int) (bool, error) {
			_, _ = fmt.Fprintf(hints, "agent/%s changes files that run code on your machine:\n  %s\nCreate the worktree? [y/N] ", pos[0], strings.Join(maku.CodeSpans(files, more), "\n  "))
			line, err := bufio.NewReader(io.LimitReader(f, 64)).ReadString('\n')
			if err != nil && line == "" {
				return false, err
			}
			return strings.EqualFold(strings.TrimSpace(line), "y"), nil
		}
	}
	got, err := co.Run(ctx)
	var refusal *maku.CheckoutRefusal
	if errors.As(err, &refusal) {
		return fmt.Errorf("checkout: %s", maku.SafeText(refusal.Error()))
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

// opencodeServiceStart is when OpenCode's background service started, read from its process
// (`ps -o lstart`); false when none runs or ps cannot say.
func opencodeServiceStart(ctx context.Context) func() (time.Time, bool) {
	return func() (time.Time, bool) {
		out, err := exec.CommandContext(ctx, "pgrep", "-f", `(^|/)opencode(\.exe)? serve --service$`).Output()
		pid := strings.Fields(string(out))
		if err != nil || len(pid) == 0 {
			return time.Time{}, false
		}
		ls, err := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", pid[0]).Output() //nolint:gosec // a pid pgrep printed
		if err != nil {
			return time.Time{}, false
		}
		t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.Join(strings.Fields(string(ls)), " "), time.Local)
		return t, err == nil
	}
}
