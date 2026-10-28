// SPDX-License-Identifier: Apache-2.0

package slackapp

import (
	"errors"
	"strings"
	"testing"
)

// `@factory link` answers the user alone with the link page, names them for the page when Slack
// says, and drafts nothing; `@factory unlink` removes the link; with the broker's link off, both
// say so.
func TestLinkCommands(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	rig.slack.names = map[string]string{"U02DEF": "dana"}
	rig.mention("C03", "1729.0001", "<@U0BOT>  Link ")
	w := rig.slack.whispers()
	if len(w) != 1 || w[0].user != "U02DEF" || w[0].channel != "C03" || w[0].thread != "1729.0001" ||
		!strings.Contains(w[0].text, "<https://rooms.example/link/slack?code=abcdefghijklmnopqrstuvwxyz|") || !strings.Contains(w[0].text, "Keep it to yourself") {
		t.Fatalf("%+v", w)
	}
	if len(rig.posts()) != 0 || len(rig.broker.links) != 1 || rig.broker.links[0] != "T01ABC/U02DEF/dana" {
		t.Fatalf("a link posted in the channel or drafted: %d posts, %v", len(rig.posts()), rig.broker.links)
	}
	if rig.metrics.count("events", "link") != 1 {
		t.Fatal("uncounted")
	}
	rig.broker.unlinked["T01ABC/U02DEF"] = true
	rig.mention("C03", "1729.0002", "<@U0BOT> unlink")
	rig.mention("C03", "1729.0003", "<@U0BOT> unlink")
	w = rig.slack.whispers()
	if len(w) != 3 || !strings.HasPrefix(w[1].text, "Unlinked:") || !strings.HasPrefix(w[2].text, "Nothing to unlink") {
		t.Fatalf("%+v", w)
	}
	rig.broker.linkErr = ErrUnknownTool
	rig.mention("C03", "1729.0004", "<@U0BOT> link")
	if w := rig.slack.whispers(); !strings.Contains(w[len(w)-1].text, "not turned on here") {
		t.Fatalf("off: %+v", w[len(w)-1])
	}
	rig.broker.linkErr = Refused("too_many_codes: too many links are waiting")
	rig.mention("C03", "1729.0005", "<@U0BOT> link")
	if w := rig.slack.whispers(); !strings.HasPrefix(w[len(w)-1].text, "Link refused: too_many_codes") {
		t.Fatalf("refused: %+v", w[len(w)-1])
	}
	// "link" inside a title is a draft, never the command.
	rig.mention("C03", "1729.0006", "<@U0BOT> docs: link the runbook")
	if p := rig.lastPost(); p.meta == nil {
		t.Fatalf("a draft with the word link: %+v", p)
	}
}

// The Filed line names the sponsor the factory chose, so a wrong link shows at its first use; a
// sponsor that is not a login is left out.
func TestFiledNamesTheSponsor(t *testing.T) {
	for sponsor, want := range map[string]string{"smana": "Filed #42 for <@U02DEF>, sponsored by GitHub @smana; ", "<!channel>": "Filed #42 for <@U02DEF>; "} {
		rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
		rig.broker.sponsor = sponsor
		rig.mention("C03", "1729.0001", draftText)
		rig.react("C03", rig.lastPost().ts, "U02DEF", "white_check_mark")
		if got := rig.lastPost().text; !strings.HasPrefix(got, want) {
			t.Fatalf("%q, want %q", got, want)
		}
	}
}

// B: a ✅ on the App's draft is never refused without a word. Each refusal names the reason and
// the fix, once per draft, user and reason; a passing failure is said each time.
func TestNoSilentCheckMark(t *testing.T) {
	rig := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2})
	rig.mention("C03", "1729.0001", draftText)
	draft := rig.lastPost()
	n := len(rig.posts())
	said := func(want string) {
		t.Helper()
		if got := rig.posts(); len(got) != n+1 || !strings.Contains(got[len(got)-1].text, want) {
			t.Fatalf("want one line %q, got %d posts: %+v", want, len(got)-n, got[len(got)-1])
		}
		n++
	}
	silent := func(what string) {
		t.Helper()
		if len(rig.posts()) != n {
			t.Fatalf("%s: answered %q", what, rig.lastPost().text)
		}
	}

	// The channel no longer lists the draft's repository.
	rig.app.Channels["C03"] = Channel{Repositories: []string{"Smana/other"}, MaxCostUSD: 2}
	rig.react("C03", draft.ts, "U02DEF", "white_check_mark")
	said("Not filed: Smana/ai-native-ref isn't served in this channel; ask an operator to add it to the channel's repositories.")
	rig.react("C03", draft.ts, "U02DEF", "white_check_mark")
	silent("the same refusal to the same user")
	rig.react("C03", draft.ts, "U07GHI", "white_check_mark")
	said("isn't served in this channel")
	// The channel is no longer served at all.
	delete(rig.app.Channels, "C03")
	rig.react("C03", draft.ts, "U02DEF", "white_check_mark")
	said("this channel is no longer served")
	rig.app.Channels["C03"] = Channel{Repositories: []string{"Smana/ai-native-ref"}, MaxCostUSD: 2}

	// Factory refusals, as sentences with their fix.
	for code, want := range map[string]string{
		"not_a_maintainer":           "<@U02DEF>'s GitHub account is not a maintainer of Smana/ai-native-ref; a maintainer must react.",
		"repository_not_allowed":     "ask an operator to add it to the Slack App's repositories",
		"principal_budget_exhausted": "budget for today is spent",
	} {
		rig.broker.runErr = Refused("rejected: " + code)
		rig.react("C03", draft.ts, "U02DEF", "white_check_mark")
		said(want)
	}
	// A passing one is said on each ✅: reacting again is its fix.
	rig.broker.runErr = Refused("rejected: link_unverified")
	rig.react("C03", draft.ts, "U02DEF", "white_check_mark")
	said("could not check this just now; react again to retry")
	rig.react("C03", draft.ts, "U02DEF", "white_check_mark")
	said("react again to retry")
	rig.broker.runErr = errors.New("connection refused")
	rig.react("C03", draft.ts, "U02DEF", "white_check_mark")
	said("the factory could not be reached")
	rig.broker.runErr = nil

	// Slack failing mid-way is said; the thread too long is said once.
	rig.slack.threadErr = errors.New("ratelimited")
	rig.react("C03", draft.ts, "U02DEF", "white_check_mark")
	said("Slack could not be read just now; react again to retry")
	rig.slack.threadErr = ErrThreadTooLong
	rig.react("C03", draft.ts, "U02DEF", "white_check_mark")
	said("this thread is too long")
	rig.react("C03", draft.ts, "U02DEF", "white_check_mark")
	silent("the too-long thread again")
	rig.slack.threadErr = nil

	// A ✅ typed as a reply files as the reaction does, and its refusal is said once too.
	rig.broker.refuse("slack:T01ABC/U08NEW", "rejected: not_linked")
	rig.message("C03", "1729.0001", "U08NEW", ":white_check_mark:")
	said("<@U08NEW> is not linked to a GitHub account here; mention me with `link` to link it, then react again. A maintainer reacts :white_check_mark: to file it.")
	rig.message("C03", "1729.0001", "U08NEW", "✅")
	silent("a second typed ✅")
	if rig.broker.created() != 0 {
		t.Fatal("an unlinked typed ✅ filed")
	}

	// Silent on purpose: a ✅ on a human's message, a message Slack cannot return, the App's own user.
	rig.react("C03", "1729.0001", "U02DEF", "white_check_mark")
	silent("a human's message")
	rig.react("C03", draft.ts, "U0BOT", "white_check_mark")
	silent("the App's own reaction")
	rig.slack.msgErr = errors.New("message_not_found")
	rig.react("C03", draft.ts, "U02DEF", "white_check_mark")
	silent("an unreadable message: no thread to answer in")
}
