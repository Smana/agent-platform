// SPDX-License-Identifier: Apache-2.0

package maku

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Smana/maku/internal/httpx"
	"github.com/Smana/maku/internal/maku/skill"
)

// setupPath is the broker's unauthenticated endpoint for the values `maku configure` needs.
// None of them is a secret: the CLI setup view renders them to any member.
const setupPath = "/.well-known/roomctl"

// maxSetupBody bounds the setup answer.
const maxSetupBody = 64 << 10

// Cause names the cause of a transport failure and its fix, wrapping err so errors.Is still
// finds it: the runbook's developer saw a resolver address and an x509 sentence, and neither
// said "connect your VPN" or "trust the platform CA" (#48). Any other error passes unchanged.
func Cause(err error) error {
	if err == nil {
		return nil
	}
	host := hostOf(err)
	var dns *net.DNSError
	var unknownCA x509.UnknownAuthorityError
	var timeout interface{ Timeout() bool }
	switch {
	case errors.As(err, &dns):
		return fmt.Errorf("cannot resolve %s: connect your VPN (Tailscale), the rooms host is private: %w", firstNonEmpty(dns.Name, host), err)
	case errors.As(err, &unknownCA):
		return fmt.Errorf("the certificate of %s is signed by an authority this machine does not trust: "+
			"trust the platform CA (add it to the system trust store, or point SSL_CERT_FILE at a bundle holding it): %w", host, err)
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()):
		return fmt.Errorf("%s did not answer in time: is your VPN connected? %w", host, err)
	case strings.Contains(err.Error(), "connection refused"):
		return fmt.Errorf("%s refused the connection: the broker may be down or the host is not the rooms host: %w", host, err)
	}
	return err
}

// hostOf is the host a failed request was for, "" when err does not say.
func hostOf(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil {
			return u.Hostname()
		}
	}
	return ""
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Discover reads the issuer, client id and project id from the broker at base, so `configure`
// takes the URL alone. The URL is the one asked for: a proxy in front may name the broker
// otherwise. A broker with no maku client yet is refused with that reason.
//
// A local install is the one broker discovered over plain http, and only on this machine: its
// listener is the port-forward, and there is no token for TLS to protect. Every other plain-http
// URL is refused before any request, so a URL a mistyped config names is never asked.
func Discover(ctx context.Context, hc *http.Client, base string) (Config, error) {
	base = strings.TrimRight(base, "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return Config{}, fmt.Errorf("%q is not an http(s) URL", base)
	}
	if u.Scheme == "http" && !LoopbackURL(u) {
		return Config{}, fmt.Errorf("%s: a plain-http broker is only a local install's loopback listener (http://localhost:<port>): "+
			"use the https rooms URL", base)
	}
	noRedirect := *hc
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+setupPath, nil)
	if err != nil {
		return Config{}, err
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		return Config{}, Cause(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Config{}, fmt.Errorf("%s does not serve its setup values at %s (HTTP %d): "+
			"copy them from the room list's CLI setup view and pass --issuer, --client-id and --project-id", base, setupPath, resp.StatusCode)
	}
	body, err := httpx.ReadBody(resp.Body, maxSetupBody)
	if err != nil {
		return Config{}, fmt.Errorf("setup values of %s: %w", base, err)
	}
	var got Config
	if err := json.Unmarshal(body, &got); err != nil {
		return Config{}, fmt.Errorf("setup values of %s: %w", base, err)
	}
	switch {
	case got.HumanMode == HumanModeLocal:
		// No IdP values to take: the loopback listener is the whole credential.
		if err := samePort(u, got.URL); err != nil {
			return Config{}, err
		}
	case got.HumanMode != "" && got.HumanMode != HumanModeOIDC:
		return Config{}, fmt.Errorf("%s reports humanMode %q, which this maku does not know: upgrade maku", base, got.HumanMode)
	case u.Scheme == "http":
		return Config{}, fmt.Errorf("%s answers at plain http but reports humanMode %q: "+
			"only a local broker is reached without TLS", base, got.HumanMode)
	case got.ClientID == "" || got.ProjectID == "":
		return Config{}, fmt.Errorf("%s has no maku client yet: ask an admin to finish the broker's setup", base)
	}
	got.URL = base
	return got, nil
}

// samePort refuses a local broker reached on another port than its publicURL names: its Host
// check answers only that port, so every later call would be a bare 403. An answer with no url
// (a broker before install-anywhere) is not checked.
func samePort(base *url.URL, public string) error {
	if public == "" {
		return nil
	}
	p, err := url.Parse(public)
	if err != nil || p.Port() == "" {
		return fmt.Errorf("%s reports publicURL %q, which names no port: its config is wrong", base, public)
	}
	if p.Port() != base.Port() {
		return fmt.Errorf("%s: the broker's publicURL is %s, so it answers only on port %s: forward that port "+
			"(kubectl port-forward deploy/room-broker %s:8080) and configure --url http://localhost:%s, or reinstall with LOCAL_PORT=%s",
			base, public, p.Port(), p.Port(), p.Port(), base.Port())
	}
	return nil
}

// Doctor checks what a first `maku rooms` needs, in the order it fails, and prints the fix of
// each failure (#48).
type Doctor struct {
	Config    Config
	HC        *http.Client
	TokenPath string
	Lookup    func(ctx context.Context, host string) ([]string, error) // nil is the system resolver
	SkillDirs []string                                                 // where agents read skills from
	Version   string                                                   // this binary's version
	// OpenCodeConfigs are the OpenCode config files setup writes, and ServiceStart when its
	// background service started (false: none runs); nil ServiceStart skips the check.
	OpenCodeConfigs []string
	ServiceStart    func() (time.Time, bool)
	// LegacyMCP are the agents' factory entries that still run roomctl (LegacyEntries).
	LegacyMCP []LegacyEntry
}

// Run prints one line per check and returns an error if any failed.
func (d Doctor) Run(ctx context.Context, out io.Writer) error {
	failed := false
	report := func(ok bool, name, msg string) {
		mark := "ok  "
		if !ok {
			mark, failed = "FAIL", true
		}
		_, _ = fmt.Fprintf(out, "%s %s: %s\n", mark, name, OneLine(msg))
	}
	warn := func(name, msg string) { _, _ = fmt.Fprintf(out, "warn %s: %s\n", name, OneLine(msg)) }
	skip := func(name, why string) { _, _ = fmt.Fprintf(out, "skip %s: %s\n", name, why) }

	u, err := url.Parse(d.Config.URL)
	if err != nil || u.Hostname() == "" {
		report(false, "dns", fmt.Sprintf("%q is not a URL: run maku configure --url https://rooms.<domain>", d.Config.URL))
		d.skill(report, skip) // needs no network
		return errors.New("doctor: the configuration is unusable")
	}
	lookup := d.Lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupHost
	}
	// A local broker is the loopback listener: no VPN, no certificate, no login.
	local := d.Config.HumanMode == HumanModeLocal
	reachable := false
	if _, err := lookup(ctx, u.Hostname()); err != nil {
		report(false, "dns", fmt.Sprintf("%s does not resolve: connect your VPN (Tailscale), the rooms host is private (%v)", u.Hostname(), err))
		skip("tls", "needs dns")
	} else {
		report(true, "dns", u.Hostname()+" resolves")
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, d.Config.URL+setupPath, nil)
		if rerr != nil {
			return rerr
		}
		resp, err := d.HC.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		switch {
		case err != nil:
			report(false, "tls", fmt.Sprint(Cause(err)))
		case local && u.Scheme == "http":
			reachable = true
			report(true, "tls", "plain http on the loopback listener: no certificate to check")
		default:
			reachable = true
			report(true, "tls", "the certificate chain is trusted")
		}
	}

	tok, terr := "", error(nil)
	if local {
		report(true, "login", "local broker; no login needed")
	} else if tok, terr = Token(ctx, d.HC, d.Config, d.TokenPath); terr != nil {
		report(false, "login", "not logged in or the session expired: run maku login")
	} else {
		report(true, "login", "the token is valid")
	}

	switch {
	case !reachable || terr != nil:
		skip("github", "needs tls and login")
	default:
		d.github(ctx, tok, report, warn)
	}
	d.skill(report, skip)
	d.opencode(report, warn)
	d.opencodeCodeMode(warn)
	for _, e := range d.LegacyMCP {
		warn("mcp", e.Fix())
	}
	if failed {
		return errors.New("doctor: a check failed, see the fix above")
	}
	return nil
}

// opencode warns when OpenCode's background service started before its config last changed: the
// service keeps the config it started with, so its sessions have no factory tools although
// `opencode mcp list` says connected. It says nothing when no service runs.
func (d Doctor) opencode(report func(bool, string, string), warn func(string, string)) {
	if d.ServiceStart == nil {
		return
	}
	start, running := d.ServiceStart()
	if !running {
		return
	}
	var newest time.Time
	var file string
	for _, p := range d.OpenCodeConfigs {
		if fi, err := os.Stat(p); err == nil && fi.ModTime().After(newest) {
			newest, file = fi.ModTime(), p
		}
	}
	switch {
	case file == "":
		return
	case start.Before(newest):
		warn("opencode", fmt.Sprintf("the background service started %s, before %s changed (%s): its sessions have no factory tools until `opencode service restart`",
			start.Format(time.DateTime), file, newest.Format(time.DateTime)))
	default:
		report(true, "opencode", "the background service started after its config changed")
	}
}

// opencodeCodeMode warns when an `execute` deny rule turns OpenCode's Code Mode off and the factory
// entry does not opt out with codemode:false: Code Mode servers are hidden, the factory's with them.
func (d Doctor) opencodeCodeMode(warn func(string, string)) {
	for _, p := range d.OpenCodeConfigs {
		b, err := os.ReadFile(filepath.Clean(p))
		if err != nil {
			continue
		}
		var cfg map[string]any
		if json.Unmarshal(b, &cfg) != nil { // comments: not ours to read
			continue
		}
		factory, ok := mapAt(mapAt(cfg, "mcp"), "servers")["factory"].(map[string]any)
		if ok && executionTimeout(factory) == 0 {
			warn("opencode", p+" sets no timeout.execution on mcp.servers.factory: OpenCode cuts issue_file's form at its default tool timeout (add \"timeout\": {\"execution\": 900000}; maku setup opencode does it)")
		}
		if ok && factory["codemode"] != false && hasExecuteDeny(cfg) {
			warn("opencode", p+" denies execute, which turns Code Mode off and hides the factory tools: add \"codemode\": false to mcp.servers.factory (maku setup opencode does it)")
			return
		}
	}
}

// hasExecuteDeny is whether any object in v is a permission rule that denies the execute action.
func hasExecuteDeny(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if x["action"] == "execute" && x["effect"] == "deny" {
			return true
		}
		for _, c := range x {
			if hasExecuteDeny(c) {
				return true
			}
		}
	case []any:
		for _, c := range x {
			if hasExecuteDeny(c) {
				return true
			}
		}
	}
	return false
}

// skill compares the maku-version each installed factory-handoff skill carries with this
// binary's: a copy from another release names tools or commands this one lacks.
func (d Doctor) skill(report func(bool, string, string), skip func(string, string)) {
	found := false
	for _, dir := range d.SkillDirs {
		v, ok, err := skill.Version(dir)
		switch {
		case err != nil:
			found = true
			report(false, "skill", fmt.Sprintf("%v: run maku setup <client> to rewrite it", err))
		case !ok:
		case v != d.Version:
			found = true
			report(false, "skill", fmt.Sprintf("%s holds factory-handoff %s, this maku is %s: run maku setup <client> (add --project for a repository copy), or delete the directory", filepath.Join(dir, "factory-handoff"), v, d.Version))
		default:
			found = true
			report(true, "skill", fmt.Sprintf("%s is %s", filepath.Join(dir, "factory-handoff"), v))
		}
	}
	if !found {
		skip("skill", "no factory-handoff skill installed: run maku setup <client>")
	}
}

// github asks the room list, whose X-Rooms-Access says whether the caller's GitHub link works. A
// local broker has no GitHub check at all: its one user is the port-forward's, and no bearer rides.
func (d Doctor) github(ctx context.Context, tok string, report func(bool, string, string), warn func(string, string)) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.Config.URL+"/api/rooms", nil)
	if err != nil {
		report(false, "github", err.Error())
		return
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := d.HC.Do(req)
	if err != nil {
		report(false, "github", fmt.Sprint(Cause(err)))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	access := resp.Header.Get("X-Rooms-Access")
	var rows []struct {
		Repository string `json:"repository"`
	}
	if resp.StatusCode == http.StatusOK {
		body, _ := httpx.ReadBody(resp.Body, maxSetupBody)
		_ = json.Unmarshal(body, &rows)
	}
	switch {
	case d.Config.HumanMode == HumanModeLocal && resp.StatusCode == http.StatusOK:
		report(true, "github", "a local broker has no GitHub identity check")
	case resp.StatusCode == http.StatusUnauthorized:
		report(false, "github", "the broker refused the token: run maku login")
	case resp.StatusCode != http.StatusOK:
		report(false, "github", (&Refusal{Op: "rooms", Code: resp.StatusCode}).Error())
	case access == "unlinked":
		report(false, "github", "your account has no linked GitHub identity: ask an admin to link GitHub to it at "+d.Config.Issuer)
	case access == "unverified":
		report(false, "github", "the broker cannot verify your GitHub access right now: retry in a minute, and if it persists ask an admin")
	default:
		// "ok" is no proof: an admin reads every room with no link, and a list with no room checks
		// nothing. issue_draft's canStart looks the link up, so ask it.
		d.linkProbe(ctx, tok, rows, report, warn)
	}
}

// maxProbes is how many repositories the link probe asks about.
const maxProbes = 3

// linkProbe checks the GitHub link the way issue_draft's canStart does, on the repositories the
// room list shows, up to maxProbes of them: a repository outside human.mcp.sessionStart cannot
// answer, another may. Only "linked" and "not linked" are answers; anything else is a warning.
func (d Doctor) linkProbe(ctx context.Context, tok string, rows []struct {
	Repository string `json:"repository"`
}, report func(bool, string, string), warn func(string, string)) {
	var repos []string
	for _, r := range rows {
		if r.Repository != "" && !slices.Contains(repos, r.Repository) && len(repos) < maxProbes {
			repos = append(repos, r.Repository)
		}
	}
	const unverified = "unverified: the room list does not say whether GitHub is linked to your account (an admin reads every room without one)"
	if len(repos) == 0 {
		warn("github", unverified+", and no room shows a repository to check against")
		return
	}
	rl := &Relay{URL: d.Config.URL, HC: d.HC, Token: func(context.Context) (string, error) { return tok, nil }}
	last := "issue_draft did not answer"
	for _, repo := range repos {
		_, sc, isErr, err := rl.callBroker(ctx, "issue_draft", map[string]string{"repo": repo, "title": "doctor: check the GitHub link",
			"body": "## What is wrong\nnothing\n\n## What it should be\nnothing\n\n## Acceptance check\nnothing\n"})
		var got draftShape
		if err != nil || isErr || json.Unmarshal(sc, &got) != nil {
			continue
		}
		switch {
		case got.CanStart:
			report(true, "github", "your GitHub identity is linked")
			return
		case got.CanStartWhy == "not_linked":
			report(false, "github", "your account has no linked GitHub identity: ask an admin to link GitHub to it at "+d.Config.Issuer)
			return
		}
		last = "issue_draft: " + OneLine(got.CanStartWhy)
	}
	warn("github", unverified+" ("+last+")")
}
