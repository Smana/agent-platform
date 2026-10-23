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

// ParseMention shapes a mention into a draft (R22): the text starts with <@botUserID>, line 1's
// rest is "<owner/repo> <title>", and the remaining lines are the body, as typed. The broker's
// issue_draft judges the template; this only shapes it.
func ParseMention(text, botUserID string) (Draft, error) {
	rest, ok := strings.CutPrefix(text, "<@"+botUserID+">")
	if !ok {
		return Draft{}, errors.New("slackapp: the message does not start with a mention of the App")
	}
	first, body, _ := strings.Cut(strings.TrimSpace(rest), "\n")
	repo, title, _ := strings.Cut(strings.TrimSpace(first), " ")
	title = strings.TrimSpace(slackUnescape.Replace(title))
	body = strings.TrimSpace(slackUnescape.Replace(body))
	switch {
	case !repoRE.MatchString(repo):
		return Draft{}, errors.New("slackapp: line 1 must be <owner/repo> <title>")
	case title == "":
		return Draft{}, errors.New("slackapp: the title is empty")
	case body == "":
		return Draft{}, errors.New("slackapp: the body is empty")
	}
	return Draft{Repo: repo, Title: title, Body: body}, nil
}
