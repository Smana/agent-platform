// SPDX-License-Identifier: Apache-2.0

package slackapp

import (
	"strings"
	"testing"
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
	if err != nil || d.Title != "fix: a <b> & c" || d.Body != "use <tag>" {
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
	if err != nil || d.Repo != "Smana/ai-native-ref" || d.Title != "docs/agents is stale" {
		t.Errorf("%+v %v", d, err)
	}
	if _, err := ParseMention("<@U0BOT>\nbody", "U0BOT", []string{"Smana/ai-native-ref"}); err == nil {
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
		"one repo, no body": {"<@U0BOT> the runbooks README should link to runbook 19", one,
			[]string{"the body is empty"}, nil},
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
