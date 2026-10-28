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

// example is the corrected line 1 a refusal shows when the user typed nothing to build one from.
const example = "Smana/ai-native-ref docs: fix a dead link"

// ParseMention shapes a mention into a draft (R22): the text starts with <@botUserID>, line 1's
// rest is "[<owner/repo>] <title>" and composeBody turns the remaining lines into the template the
// factory's validator holds (the App adapts, the validator stays strict).
//
// Where the channel serves exactly one repository, line 1 is the title alone (the repository named
// first, bare or in brackets, is still accepted), and a title that starts with something
// owner/name-shaped stays a title. Otherwise only is "".
func ParseMention(text, botUserID string, repos []string) (Draft, error) {
	only := ""
	if len(repos) == 1 {
		only = repos[0]
	}
	rest, ok := strings.CutPrefix(text, "<@"+botUserID+">")
	if !ok {
		return Draft{}, errors.New("slackapp: the message does not start with a mention of the App")
	}
	first, body, _ := strings.Cut(strings.TrimSpace(rest), "\n")
	first = strings.TrimSpace(first)
	repo, title, _ := strings.Cut(first, " ")
	if r, ok := strings.CutPrefix(repo, "["); ok {
		if r, ok = strings.CutSuffix(r, "]"); ok {
			repo = r
		}
	}
	if only != "" {
		if repo == only {
			first = title
		}
		repo, title = only, first
	}
	title = strings.TrimSpace(slackUnescape.Replace(title))
	typed := strings.TrimSpace(slackUnescape.Replace(first)) // line 1 as the user wrote it
	body = strings.TrimSpace(slackUnescape.Replace(body))
	switch {
	case !repoRE.MatchString(repo):
		return Draft{}, errors.New("slackapp: line 1 must be the repository, a space, then the title. " + tryLine(typed, repos))
	case title == "" && only != "":
		return Draft{}, errors.New("slackapp: line 1 is the title of the issue, for example: docs: fix a dead link")
	case title == "":
		return Draft{}, errors.New("slackapp: the title is empty, line 1 is the repository, a space, then the title, for example: " + example)
	}
	return Draft{Repo: repo, Title: areaTitle(title, body), Body: composeBody(title, body)}, nil
}

// titleRE is the factory's title rule (sanitize/draft.go draftTitleRE), copied: a title that
// already has it is used as typed.
var titleRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*(\([A-Za-z0-9._/-]+\))?: \S`)

var docWordRE = regexp.MustCompile(`(?i)\b(docs?|documentation|runbooks?|readme)\b`)

const maxTitleRunes = 256 // the factory's title cap

// areaTitle keeps a title that has the factory's `<area>: <what>` shape. Otherwise it prefixes
// `docs: ` when the text names a doc, runbook or README, else `chore: `: the two areas that never
// claim a behaviour change. Nothing in the factory is relaxed.
func areaTitle(title, body string) string {
	if !titleRE.MatchString(title) {
		area := "chore"
		if docWordRE.MatchString(title + "\n" + body) {
			area = "docs"
		}
		title = area + ": " + title
	}
	if r := []rune(title); len(r) > maxTitleRunes {
		title = string(r[:maxTitleRunes])
	}
	return title
}

const (
	noAcceptance = "None given in Slack: the factory's scoper asks the author on this issue."
	asTheTitle   = "As the title says."
)

var (
	// A line opening a section, once Slack's decoration is stripped from its start.
	acceptanceRE = regexp.MustCompile(`(?i)^(acceptance check|acceptance|done when|check)[*_\s]*(:|$)`)
	shouldRE     = regexp.MustCompile(`(?i)^(should|expected|want)\s*:`)
	// The keyword glued to the end of a sentence ("...the API.Acceptance"): only after . ! or ?,
	// and only with a colon or the end of the line after it.
	gluedRE  = regexp.MustCompile(`(?i)([.!?])(acceptance check|acceptance|done when)\s*(:|$)`)
	spacedRE = regexp.MustCompile(`(?i)([.!?])\s+(acceptance check|acceptance|done when)\s*:`)
)

const decoration = "*_#•-> \t"

// composeBody builds the template's body from free text: the lines before any keyword are what is
// wrong (the title when there are none), a `should:` line opens what it should be, an
// `acceptance:` line opens the acceptance check. A body that already has the template's first
// heading is passed as typed. Slack's `•` bullets become `- `.
func composeBody(title, body string) string {
	if hasHeading(body, "## What is wrong") {
		return body
	}
	var sec [3][]string // wrong, should, acceptance
	cur := 0
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if loc := gluedRE.FindStringSubmatchIndex(line); loc != nil {
			sec[cur] = append(sec[cur], line[:loc[3]])
			line, cur = line[loc[4]:], 2
		} else if loc := spacedRE.FindStringSubmatchIndex(line); loc != nil {
			sec[cur] = append(sec[cur], line[:loc[3]])
			line, cur = line[loc[4]:], 2
		}
		stripped := strings.TrimLeft(line, decoration)
		switch {
		case acceptanceRE.MatchString(stripped):
			cur = 2
			line = afterKeyword(stripped)
		case shouldRE.MatchString(stripped):
			cur = 1
			line = afterKeyword(stripped)
		}
		if strings.TrimLeft(line, decoration) == "" && line != "" {
			line = "" // only decoration left, as in "*Acceptance:*"
		}
		if t := strings.TrimLeft(line, " \t"); strings.HasPrefix(t, "• ") {
			line = line[:len(line)-len(t)] + "- " + strings.TrimPrefix(t, "• ")
		}
		sec[cur] = append(sec[cur], line)
	}
	text := func(i int, def string) string {
		if t := strings.TrimSpace(strings.Join(sec[i], "\n")); t != "" {
			return t
		}
		return def
	}
	return "## What is wrong\n" + text(0, title) +
		"\n\n## What it should be\n" + text(1, asTheTitle) +
		"\n\n## Acceptance check\n" + text(2, noAcceptance)
}

// afterKeyword is what follows the keyword's colon on its line, minus the closing decoration.
func afterKeyword(s string) string {
	_, rest, _ := strings.Cut(s, ":")
	return strings.TrimSpace(strings.TrimLeft(rest, "*_ "))
}

func hasHeading(body, heading string) bool {
	for line := range strings.SplitSeq(body, "\n") {
		if strings.TrimSpace(line) == heading {
			return true
		}
	}
	return false
}

// tryLine is the corrected line 1 a refusal offers, built from the user's own words: the
// repository (the channel's first when several) in front of what they typed, which is cut short
// and stripped of backticks so it cannot end the code span. The caller's shown() escapes it.
func tryLine(typed string, repos []string) string {
	typed = strings.Join(strings.Fields(strings.ReplaceAll(typed, "`", "")), " ")
	if r := []rune(typed); len(r) > 80 {
		typed = string(r[:80]) + "…"
	}
	repo := "<owner/repo>"
	if len(repos) > 0 {
		repo = repos[0]
	}
	if typed == "" {
		return "For example: `@factory " + repo + " docs: fix a dead link`"
	}
	out := "Try: `@factory " + repo + " " + typed + "`"
	if len(repos) > 1 {
		out += " (repositories here: " + strings.Join(repos, ", ") + ")"
	}
	return out
}
