// SPDX-License-Identifier: Apache-2.0

package slackapp

import (
	"errors"
	"regexp"
	"strings"
)

// Draft is an issue as typed in a thread: the repository, the title and the body.
type Draft struct{ Repo, Title, Body string }

var repoRE = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

// slackUnescape undoes the three escapes Slack applies to message text; &amp; goes last so
// "&amp;lt;" stays "&lt;".
var slackUnescape = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")

// example is the corrected line 1 a refusal shows: no angle bracket, so Slack renders it as typed.
const example = "Smana/ai-native-ref docs: fix a dead link"

// ParseMention shapes a mention into a draft (R22): the text starts with <@botUserID>, line 1's
// rest is "<owner/repo> <title>", and the remaining lines are the body, as typed. The broker's
// issue_draft judges the template; this only shapes it.
//
// Where the channel serves exactly one repository, only is that repository: line 1 is then the
// title alone (the repository named first is still accepted), and a title that starts with
// something owner/name-shaped stays a title. Otherwise only is "".
func ParseMention(text, botUserID, only string) (Draft, error) {
	rest, ok := strings.CutPrefix(text, "<@"+botUserID+">")
	if !ok {
		return Draft{}, errors.New("slackapp: the message does not start with a mention of the App")
	}
	first, body, _ := strings.Cut(strings.TrimSpace(rest), "\n")
	first = strings.TrimSpace(first)
	repo, title, _ := strings.Cut(first, " ")
	if only != "" {
		if repo == only {
			first = title
		}
		repo, title = only, first
	}
	title = strings.TrimSpace(slackUnescape.Replace(title))
	body = strings.TrimSpace(slackUnescape.Replace(body))
	switch {
	case !repoRE.MatchString(repo):
		return Draft{}, errors.New("slackapp: line 1 must be the repository, a space, then the title, for example: " + example)
	case title == "" && only != "":
		return Draft{}, errors.New("slackapp: line 1 is the title of the issue, for example: docs: fix a dead link")
	case title == "":
		return Draft{}, errors.New("slackapp: the title is empty, line 1 is the repository, a space, then the title, for example: " + example)
	case body == "":
		return Draft{}, errors.New("slackapp: the body is empty")
	}
	return Draft{Repo: repo, Title: title, Body: body}, nil
}
