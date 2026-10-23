// SPDX-License-Identifier: Apache-2.0

package slackapp

import "testing"

// R22: line 1 after the mention is <owner/repo> <title>; the rest is the body, as typed.
func TestParseMention(t *testing.T) {
	text := "<@U0BOT> Smana/ai-native-ref docs(agents): the runbook link is dead\n## What is wrong\nx\n## What it should be\ny\n## Acceptance check\nz"
	d, err := ParseMention(text, "U0BOT")
	if err != nil || d.Repo != "Smana/ai-native-ref" || d.Title != "docs(agents): the runbook link is dead" ||
		d.Body != "## What is wrong\nx\n## What it should be\ny\n## Acceptance check\nz" {
		t.Fatalf("%+v %v", d, err)
	}
	for _, bad := range []string{"<@U0BOT>", "<@U0BOT> not-a-repo title\nbody", "<@U0OTHER> Smana/x t\nb", "<@U0BOT> Smana/x\nbody"} {
		if _, err := ParseMention(bad, "U0BOT"); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// Slack escapes &, < and > in message text; the draft is what the user typed.
func TestParseMentionUnescapes(t *testing.T) {
	d, err := ParseMention("<@U0BOT> Smana/x fix: a &lt;b&gt; &amp; c\nuse &lt;tag&gt;", "U0BOT")
	if err != nil || d.Title != "fix: a <b> & c" || d.Body != "use <tag>" {
		t.Fatalf("%+v %v", d, err)
	}
}
