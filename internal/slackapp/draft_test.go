// SPDX-License-Identifier: Apache-2.0

package slackapp

import (
	"strings"
	"testing"
)

// R22: line 1 after the mention is <owner/repo> <title>; the rest is the body, as typed.
func TestParseMention(t *testing.T) {
	text := "<@U0BOT> Smana/ai-native-ref docs(agents): the runbook link is dead\n## What is wrong\nx\n## What it should be\ny\n## Acceptance check\nz"
	d, err := ParseMention(text, "U0BOT", "")
	if err != nil || d.Repo != "Smana/ai-native-ref" || d.Title != "docs(agents): the runbook link is dead" ||
		d.Body != "## What is wrong\nx\n## What it should be\ny\n## Acceptance check\nz" {
		t.Fatalf("%+v %v", d, err)
	}
	for _, bad := range []string{"<@U0BOT>", "<@U0BOT> not-a-repo title\nbody", "<@U0OTHER> Smana/x t\nb", "<@U0BOT> Smana/x\nbody"} {
		if _, err := ParseMention(bad, "U0BOT", ""); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// Slack escapes &, < and > in message text; the draft is what the user typed.
func TestParseMentionUnescapes(t *testing.T) {
	d, err := ParseMention("<@U0BOT> Smana/x fix: a &lt;b&gt; &amp; c\nuse &lt;tag&gt;", "U0BOT", "")
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
		d, err := ParseMention(text, "U0BOT", "Smana/ai-native-ref")
		if err != nil || d.Repo != "Smana/ai-native-ref" || d.Title != "docs(agents): the link is dead" || d.Body != "## What is wrong\nx" {
			t.Errorf("%s: %+v %v", name, d, err)
		}
	}
	// A title whose first word looks like owner/name is a title when the channel's repository is known.
	d, err := ParseMention("<@U0BOT> docs/agents is stale\nbody", "U0BOT", "Smana/ai-native-ref")
	if err != nil || d.Repo != "Smana/ai-native-ref" || d.Title != "docs/agents is stale" {
		t.Errorf("%+v %v", d, err)
	}
	if _, err := ParseMention("<@U0BOT>\nbody", "U0BOT", "Smana/ai-native-ref"); err == nil {
		t.Error("an empty title was accepted")
	}
}

// The refusal shows a corrected example.
func TestParseMentionRefusalShowsAnExample(t *testing.T) {
	_, err := ParseMention("<@U0BOT> not-a-repo title\nbody", "U0BOT", "")
	if err == nil || !strings.Contains(err.Error(), "for example") || !strings.Contains(err.Error(), "Smana/ai-native-ref docs: fix a dead link") {
		t.Fatalf("%v", err)
	}
}
