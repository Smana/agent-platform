// SPDX-License-Identifier: Apache-2.0

package roomctl

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
	"path/filepath"
	"strings"

	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/roomctl/skill"
)

// setupPath is the broker's unauthenticated endpoint for the values `roomctl configure` needs.
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
// otherwise. A broker with no roomctl client yet is refused with that reason.
func Discover(ctx context.Context, hc *http.Client, base string) (Config, error) {
	base = strings.TrimRight(base, "/")
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
	if got.ClientID == "" || got.ProjectID == "" {
		return Config{}, fmt.Errorf("%s has no roomctl client yet: ask an admin to finish the broker's setup", base)
	}
	got.URL = base
	return got, nil
}

// Doctor checks what a first `roomctl rooms` needs, in the order it fails, and prints the fix of
// each failure (#48).
type Doctor struct {
	Config    Config
	HC        *http.Client
	TokenPath string
	Lookup    func(ctx context.Context, host string) ([]string, error) // nil is the system resolver
	SkillDirs []string                                                 // where agents read skills from
	Version   string                                                   // this binary's version
}

// Run prints one line per check and returns an error if any failed.
func (d Doctor) Run(ctx context.Context, out io.Writer) error {
	failed := false
	report := func(ok bool, name, msg string) {
		mark := "ok  "
		if !ok {
			mark, failed = "FAIL", true
		}
		_, _ = fmt.Fprintf(out, "%s %s: %s\n", mark, name, oneLine(msg))
	}
	skip := func(name, why string) { _, _ = fmt.Fprintf(out, "skip %s: %s\n", name, why) }

	u, err := url.Parse(d.Config.URL)
	if err != nil || u.Hostname() == "" {
		report(false, "dns", fmt.Sprintf("%q is not a URL: run roomctl configure --url https://rooms.<domain>", d.Config.URL))
		d.skill(report, skip) // needs no network
		return errors.New("doctor: the configuration is unusable")
	}
	lookup := d.Lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupHost
	}
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
		if err != nil {
			report(false, "tls", fmt.Sprint(Cause(err)))
		} else {
			reachable = true
			report(true, "tls", "the certificate chain is trusted")
		}
	}

	tok, terr := Token(ctx, d.HC, d.Config, d.TokenPath)
	if terr != nil {
		report(false, "login", "not logged in or the session expired: run roomctl login")
	} else {
		report(true, "login", "the token is valid")
	}

	switch {
	case !reachable || terr != nil:
		skip("github", "needs tls and login")
	default:
		d.github(ctx, tok, report)
	}
	d.skill(report, skip)
	if failed {
		return errors.New("doctor: a check failed, see the fix above")
	}
	return nil
}

// skill compares the roomctl-version each installed factory-handoff skill carries with this
// binary's: a copy from another release names tools or commands this one lacks.
func (d Doctor) skill(report func(bool, string, string), skip func(string, string)) {
	found := false
	for _, dir := range d.SkillDirs {
		v, ok, err := skill.Version(dir)
		switch {
		case err != nil:
			found = true
			report(false, "skill", fmt.Sprintf("%v: run roomctl setup <client> to rewrite it", err))
		case !ok:
		case v != d.Version:
			found = true
			report(false, "skill", fmt.Sprintf("%s holds factory-handoff %s, this roomctl is %s: run roomctl setup <client> (add --project for a repository copy), or delete the directory", filepath.Join(dir, "factory-handoff"), v, d.Version))
		default:
			found = true
			report(true, "skill", fmt.Sprintf("%s is %s", filepath.Join(dir, "factory-handoff"), v))
		}
	}
	if !found {
		skip("skill", "no factory-handoff skill installed: run roomctl setup <client>")
	}
}

// github asks the room list, whose X-Rooms-Access says whether the caller's GitHub link works.
func (d Doctor) github(ctx context.Context, tok string, report func(bool, string, string)) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.Config.URL+"/api/rooms", nil)
	if err != nil {
		report(false, "github", err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := d.HC.Do(req)
	if err != nil {
		report(false, "github", fmt.Sprint(Cause(err)))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	switch access := resp.Header.Get("X-Rooms-Access"); {
	case resp.StatusCode == http.StatusUnauthorized:
		report(false, "github", "the broker refused the token: run roomctl login")
	case resp.StatusCode != http.StatusOK:
		report(false, "github", (&Refusal{Op: "rooms", Code: resp.StatusCode}).Error())
	case access == "unlinked":
		report(false, "github", "your account has no linked GitHub identity: ask an admin to link GitHub to it at "+d.Config.Issuer)
	case access == "unverified":
		report(false, "github", "the broker cannot verify your GitHub access right now: retry in a minute, and if it persists ask an admin")
	default:
		report(true, "github", "your GitHub identity is linked")
	}
}
