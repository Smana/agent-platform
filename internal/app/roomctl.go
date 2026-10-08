// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/roomctl"
	"github.com/Smana/agent-platform/internal/roomctl/skill"
	"github.com/Smana/agent-platform/internal/version"
)

// roomctlTimeout bounds one call to the broker or the IdP; actTimeout one act.
const (
	roomctlTimeout = 30 * time.Second
	actTimeout     = time.Minute
)

const roomctlUsage = `usage: roomctl <command>
  configure --url URL --issuer URL --client-id ID --project-id ID
                                  the values are on the room list's CLI setup view
  login                           sign in with the device flow
  token                           print a valid access token, for scripts (task agent:run)
  status <room> [--json] [--after SEQ]   where a room stands: status, what needs you, notes
  rooms [--repo owner/name] [--mine] [--needs-me]
                                  the rooms you can read. --mine: the tasks of
                                  issues you filed or labelled, PRs you authored or review.
                                  --needs-me: an approval you could decide
  watch <room> [--tail N]         follow a room
  post <room> [--queue] <text>    chat, or queue it for the next run's brief
  skill install [--dir .agents/skills]   write the factory-handoff Agent Skill for local coding agents
  fork <room> --at SEQ [--role R] [--pr URL] [--egress pypi,npm] [--note TEXT]
                                  a room of your own from events 1..SEQ, with an optional run
roomctl never steers, interrupts, moves the driver token or decides (ruling P18): use the web UI`

// Roomctl is roomctl's wiring: where its files live, and its two HTTP clients.
type Roomctl struct {
	Dir    string       // config.json and token.json, both owner-only
	HC     *http.Client // the room list and the IdP: bounded
	Stream *http.Client // the WebSocket: bounded by the context, not a whole-exchange timeout
	Out    io.Writer
	Err    io.Writer // hints; nil is stderr
}

// RunRoomctl runs one roomctl command (SP2 §8), its files under roomctl.Dir and
// its egress through httpx.
func RunRoomctl(ctx context.Context, args []string, stdout io.Writer) error {
	dir, err := roomctl.Dir()
	if err != nil {
		return err
	}
	stream := httpx.New(roomctlTimeout, nil)
	stream.Timeout = 0 // a watch lasts as long as the developer wants it
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

// Run runs one command.
func (r Roomctl) Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("a command is required\n%s", roomctlUsage)
	}
	cmd, args := args[0], args[1:]
	cfgPath, tokPath := filepath.Join(r.Dir, "config.json"), filepath.Join(r.Dir, "token.json")
	switch cmd {
	case "configure":
		return r.configure(cfgPath, args)
	case "skill":
		return r.skill(args)
	case "help", "-h", "--help":
		_, err := fmt.Fprintln(r.Out, roomctlUsage)
		return err
	case "login", "token", "rooms", "status", "watch", "post", "fork":
	default:
		return fmt.Errorf("unknown command %q\n%s", cmd, roomctlUsage)
	}
	var cfg roomctl.Config
	if err := roomctl.LoadJSON(cfgPath, &cfg); err != nil {
		return errors.New("run roomctl configure first: the room list's CLI setup view has the values")
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
		_, err = fmt.Fprintln(r.Out, "logged in")
		return err
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
		fs.BoolVar(&f.NeedsMe, "needs-me", false, "only rooms with an approval you could decide")
		pos, err := flags(fs, args)
		if err != nil || len(pos) != 0 {
			return fmt.Errorf("rooms [--repo owner/name] [--mine] [--needs-me]: %w", errors.Join(err, errors.New("no arguments")))
		}
		return c.Rooms(ctx, r.Out, f)
	case "status":
		return r.status(ctx, c, args)
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

func (r Roomctl) configure(path string, args []string) error {
	var cfg roomctl.Config
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	fs.StringVar(&cfg.URL, "url", "", "https://rooms.<private domain>")
	fs.StringVar(&cfg.Issuer, "issuer", "", "the ZITADEL issuer URL")
	fs.StringVar(&cfg.ClientID, "client-id", "", "the roomctl client id")
	fs.StringVar(&cfg.ProjectID, "project-id", "", "the rooms project id")
	if _, err := flags(fs, args); err != nil {
		return err
	}
	cfg.URL, cfg.Issuer = strings.TrimRight(cfg.URL, "/"), strings.TrimRight(cfg.Issuer, "/")
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	return roomctl.SaveJSON(path, cfg)
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

// status prints where a room stands; --json is the broker's body unchanged.
func (r Roomctl) status(ctx context.Context, c roomctl.Client, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the broker's summary/v1 as is")
	after := fs.Int64("after", 0, "only notes after this seq")
	pos, err := flags(fs, args)
	if err != nil || len(pos) != 1 || *after < 0 {
		return fmt.Errorf("status <room> [--json] [--after SEQ]: %w", errors.Join(err, errors.New("one room")))
	}
	raw, err := c.Summary(ctx, pos[0], *after)
	if err != nil {
		return err
	}
	if *asJSON {
		_, err = fmt.Fprintln(r.Out, string(raw))
		return err
	}
	return roomctl.RenderSummary(r.Out, raw, time.Now())
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
