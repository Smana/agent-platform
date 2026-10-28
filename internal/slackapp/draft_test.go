// SPDX-License-Identifier: Apache-2.0

package slackapp

import (
	"strings"
	"testing"

	"github.com/Smana/maku/internal/factory/sanitize"
)

// R22: line 1 after the mention is <owner/repo> <title>; the rest is the body, as typed.
func TestParseMention(t *testing.T) {
	text := "<@U0BOT> Smana/ai-native-ref docs(agents): the runbook link is dead\n## What is wrong\nx\n## What it should be\ny\n## Acceptance check\nz"
	d, err := ParseMention(text, "U0BOT", nil)
	if err != nil || d.Repo != "Smana/ai-native-ref" || d.Title != "docs(agents): the runbook link is dead" ||
		d.Body != "## What is wrong\nx\n## What it should be\ny\n## Acceptance check\nz" {
		t.Fatalf("%+v %v", d, err)
	}
	for _, bad := range []string{"<@U0BOT>", "<@U0BOT> not-a-repo title\nbody", "<@U0OTHER> Smana/x t\nb", "<@U0BOT> Smana/x\nbody"} {
		if _, err := ParseMention(bad, "U0BOT", nil); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// Slack escapes &, < and > in message text; the draft is what the user typed.
func TestParseMentionUnescapes(t *testing.T) {
	d, err := ParseMention("<@U0BOT> Smana/x fix: a &lt;b&gt; &amp; c\nuse &lt;tag&gt;", "U0BOT", nil)
	if err != nil || d.Title != "fix: a <b> & c" || !strings.Contains(d.Body, "## What is wrong\nuse <tag>\n") {
		t.Fatalf("%+v %v", d, err)
	}
}

// O14: in a channel that serves one repository, line 1 is only the title and the repository is the
// channel's; naming it anyway still works. Live, "Draft refused: line 1 must be <owner/repo> <title>"
// answered a one-repository channel.
func TestParseMentionDefaultsTheChannelsRepository(t *testing.T) {
	for name, text := range map[string]string{
		"title only":         "<@U0BOT> docs(agents): the link is dead\n## What is wrong\nx",
		"the repo named too": "<@U0BOT> Smana/ai-native-ref docs(agents): the link is dead\n## What is wrong\nx",
		"a path-like title":  "<@U0BOT> docs(agents): the link is dead\n## What is wrong\nx",
	} {
		d, err := ParseMention(text, "U0BOT", []string{"Smana/ai-native-ref"})
		if err != nil || d.Repo != "Smana/ai-native-ref" || d.Title != "docs(agents): the link is dead" || d.Body != "## What is wrong\nx" {
			t.Errorf("%s: %+v %v", name, d, err)
		}
	}
	// A title whose first word looks like owner/name is a title when the channel's repository is known.
	d, err := ParseMention("<@U0BOT> docs/agents is stale\nbody", "U0BOT", []string{"Smana/ai-native-ref"})
	if err != nil || d.Repo != "Smana/ai-native-ref" || d.Title != "docs: docs/agents is stale" {
		t.Errorf("%+v %v", d, err)
	}
	if _, err := ParseMention("<@U0BOT>", "U0BOT", []string{"Smana/ai-native-ref"}); err == nil {
		t.Error("an empty title was accepted")
	}
}

// The refusal offers a corrected line built from the user's own text, never a live mention.
func TestParseMentionRefusalBuildsATryLine(t *testing.T) {
	one := []string{"Smana/ai-native-ref"}
	two := []string{"Smana/ai-native-ref", "Smana/cloud-native-ref"}
	for name, tc := range map[string]struct {
		text  string
		repos []string
		want  []string
		deny  []string
	}{
		"no repos, repo-less line": {"<@U0BOT> not-a-repo title\nbody", nil,
			[]string{"Try: `@factory <owner/repo> not-a-repo title`"}, nil},
		"one repo, empty title": {"<@U0BOT>", one,
			[]string{"line 1 is the title of the issue"}, []string{"## What", "template", "sections"}},
		"many repos list them": {"<@U0BOT> the runbooks README should link to runbook 19\nbody", two,
			[]string{"Try: `@factory Smana/ai-native-ref the runbooks README should link to runbook 19`", "repositories here: Smana/ai-native-ref, Smana/cloud-native-ref"}, nil},
		"nothing typed": {"<@U0BOT>", two,
			[]string{"For example: `@factory Smana/ai-native-ref docs: fix a dead link`"}, nil},
		"backticks stripped": {"<@U0BOT> a `b` c\nbody", two,
			[]string{"Try: `@factory Smana/ai-native-ref a b c`"}, nil},
		"long text cut": {"<@U0BOT> " + strings.Repeat("x", 200) + "\nbody", two,
			[]string{"…`"}, []string{strings.Repeat("x", 81)}},
	} {
		_, err := ParseMention(tc.text, "U0BOT", tc.repos)
		if err == nil {
			t.Fatalf("%s: no refusal", name)
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: %q lacks %q", name, err, w)
			}
		}
		for _, d := range tc.deny {
			if strings.Contains(err.Error(), d) {
				t.Errorf("%s: %q has %q", name, err, d)
			}
		}
	}
}

// Through the App the Try line is inert: a mention or link in the user's text is escaped.
func TestRefusalTryLineIsInert(t *testing.T) {
	two := newAppRig(t, Channel{Repositories: []string{"Smana/ai-native-ref", "Smana/cloud-native-ref"}, MaxCostUSD: 2})
	two.mention("C03", "1729.0001", "<@U0BOT> hi <!channel> <@U0EVIL> <http://x|y>\nbody")
	p := two.lastPost().text
	if !strings.Contains(p, "Try: `@factory Smana/ai-native-ref hi &lt;!channel&gt;") || strings.Contains(p, "<!channel>") || strings.Contains(p, "<@U0EVIL>") || strings.Contains(p, "<http") {
		t.Fatalf("%q", p)
	}
}

const (
	wrongH  = "## What is wrong\n"
	shouldH = "\n\n## What it should be\n"
	accH    = "\n\n## Acceptance check\n"
)

// The App composes the template from free text, shaped as Slack sends it.
func TestParseMentionComposesTheTemplate(t *testing.T) {
	one := []string{"Smana/ai-native-ref"}
	glued := "<@U0BOT> Runbook 18 should say where the key lives\nThe runbook never names the factory's API.Acceptance\n" +
		"docs/runbooks/18.md gains one last line\nNo other line changes"
	for name, tc := range map[string]struct{ text, title, body string }{
		"one line": {"<@U0BOT> docs: fix a dead link", "docs: fix a dead link",
			wrongH + "docs: fix a dead link" + shouldH + asTheTitle + accH + noAcceptance},
		"repo in brackets": {"<@U0BOT> [Smana/ai-native-ref] docs: fix a dead link", "docs: fix a dead link",
			wrongH + "docs: fix a dead link" + shouldH + asTheTitle + accH + noAcceptance},
		"no area, a doc word": {"<@U0BOT> The README is stale", "docs: The README is stale",
			wrongH + "The README is stale" + shouldH + asTheTitle + accH + noAcceptance},
		"no area, nothing about docs": {"<@U0BOT> Bump the base image", "chore: Bump the base image",
			wrongH + "Bump the base image" + shouldH + asTheTitle + accH + noAcceptance},
		"one line plus detail": {"<@U0BOT> fix: the queue stalls\nIt stalls after two hours.\nSee the log.", "fix: the queue stalls",
			wrongH + "It stalls after two hours.\nSee the log." + shouldH + asTheTitle + accH + noAcceptance},
		"bulleted acceptance": {"<@U0BOT> fix: x\nIt is broken.\nAcceptance:\n• first\n• second", "fix: x",
			wrongH + "It is broken." + shouldH + asTheTitle + accH + "- first\n- second"},
		"bold acceptance": {"<@U0BOT> fix: x\nIt is broken.\n*Acceptance:* the test passes\n• and more", "fix: x",
			wrongH + "It is broken." + shouldH + asTheTitle + accH + "the test passes\n- and more"},
		"bold keyword alone": {"<@U0BOT> fix: x\ndetail\n*Acceptance check*\n> it works", "fix: x",
			wrongH + "detail" + shouldH + asTheTitle + accH + "> it works"},
		"done when": {"<@U0BOT> fix: x\ndetail\nDone when: it works", "fix: x",
			wrongH + "detail" + shouldH + asTheTitle + accH + "it works"},
		"should and acceptance": {"<@U0BOT> fix: x\ndetail\nShould: say so\nacceptance: grep finds it", "fix: x",
			wrongH + "detail" + shouldH + "say so" + accH + "grep finds it"},
		"the glued keyword": {glued, "docs: Runbook 18 should say where the key lives",
			wrongH + "The runbook never names the factory's API." + shouldH + asTheTitle + accH +
				"docs/runbooks/18.md gains one last line\nNo other line changes"},
		"check without colon in prose is detail": {"<@U0BOT> fix: x\nPlease check this again\ncheck it", "fix: x",
			wrongH + "Please check this again" + shouldH + asTheTitle + accH + noAcceptance},
		"a pasted template is kept": {"<@U0BOT> fix: x\n## What is wrong\na\n## What it should be\nb\n## Acceptance check\nc", "fix: x",
			"## What is wrong\na\n## What it should be\nb\n## Acceptance check\nc"},
		"escapes undone": {"<@U0BOT> fix: a &lt;b&gt;\nx &amp; y", "fix: a <b>",
			wrongH + "x & y" + shouldH + asTheTitle + accH + noAcceptance},
	} {
		d, err := ParseMention(tc.text, "U0BOT", one)
		if name == "check without colon in prose is detail" {
			// "check it" alone is not a keyword line either: no colon, not the line's end after "check".
			tc.body = wrongH + "Please check this again\ncheck it" + shouldH + asTheTitle + accH + noAcceptance
		}
		if err != nil || d.Repo != one[0] || d.Title != tc.title || d.Body != tc.body {
			t.Errorf("%s:\n%+v\n%v\nwant title %q\nbody %q", name, d, err, tc.title, tc.body)
		}
	}
}

// What the App composes passes the factory's own validator, whatever the shape.
func TestComposedDraftsPassTheFactoryValidator(t *testing.T) {
	for _, text := range []string{"<@U0BOT> hello", "<@U0BOT> docs: x\ny\nacceptance: z", "<@U0BOT> " + strings.Repeat("é", 300)} {
		d, err := ParseMention(text, "U0BOT", []string{"Smana/ai-native-ref"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sanitize.CheckDraft(d.Title, d.Body); err != nil {
			t.Errorf("%q: %v", text, err)
		}
	}
}

// A mention with several repositories and none named is refused with the Try example, never with
// template headings.
func TestRefusalNeverAsksForHeadings(t *testing.T) {
	two := []string{"Smana/ai-native-ref", "Smana/cloud-native-ref"}
	_, err := ParseMention("<@U0BOT> the README is stale\ndetail", "U0BOT", two)
	if err == nil || !strings.Contains(err.Error(), "Try: `@factory Smana/ai-native-ref the README is stale`") ||
		strings.Contains(err.Error(), "##") {
		t.Fatalf("%v", err)
	}
}

func TestApplyReplyAreaRule(t *testing.T) {
	d := Draft{Repo: "a/b", Title: "chore: tidy", Body: composeBody("chore: tidy", "x")}
	got, ok := ApplyReply(d, "Title: tidy the logs")
	if !ok || got.Title != "chore: tidy the logs" {
		t.Fatalf("%+v", got)
	}
	got, _ = ApplyReply(d, "Title: feat(api): add a route")
	if got.Title != "feat(api): add a route" {
		t.Fatalf("%q", got.Title)
	}
	if _, ok := ApplyReply(d, "Acceptance:"); ok {
		t.Fatal("an empty section is no change")
	}
	if _, ok := ApplyReply(Draft{Repo: "a/b", Title: "t", Body: "no template"}, "more"); ok {
		t.Fatal("a body without the template is not edited")
	}
}
