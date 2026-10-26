// SPDX-License-Identifier: Apache-2.0

package roomview

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/summary"
)

// maxCardNotes is how many of the agents' notes the card quotes.
const maxCardNotes = 5

// card writes one view in one format: the two differ in how they quote and link, not in what
// they say.
type card struct {
	b strings.Builder
	o Options
}

func (c *card) md() bool { return c.o.Format == Markdown }

// text is untrusted words as plain text: escaped in Markdown, cleaned on a terminal.
func (c *card) text(s string, cell bool) string {
	if c.md() {
		return escapeTitle(s, cell)
	}
	return clean(s)
}

// quoted is untrusted text, quoted: in a code span in Markdown, in double quotes on a terminal.
func (c *card) quoted(s string, cell bool) string {
	if c.md() {
		return code(s, cell)
	}
	return quote(s)
}

// plainToken is a value from the broker's own vocabulary: as is when it is made of nothing but
// identifier characters (a run id, a version), quoted otherwise.
var plainToken = regexp.MustCompile(`^[A-Za-z0-9._+/-]{1,64}$`)

func (c *card) token(s string, cell bool) string {
	if plainToken.MatchString(s) {
		if c.md() && strings.HasPrefix(s, "-") {
			return code(s, cell)
		}
		return s
	}
	if c.md() {
		return code(s, cell)
	}
	return clean(s)
}

// words is a phrase of the vocabulary, or the cleaned value escaped when the vocabulary lacks it.
func (c *card) words(raw, got string, cell bool) string {
	if got == clean(raw) {
		return c.text(got, cell)
	}
	return got
}

// bold marks s in Markdown, and in a terminal that shows colour.
func (c *card) bold(s string) string {
	switch c.o.Format {
	case Markdown:
		return "**" + s + "**"
	case TextColor:
		return "\x1b[1m" + s + "\x1b[0m"
	}
	return s
}

// head is a section's title, in bold in Markdown and plain on a terminal.
func (c *card) head(s string) string {
	if c.md() {
		return "**" + s + "**"
	}
	return s
}

// url links text to url, or leaves the text alone when the broker did not build url.
func (c *card) url(text, url string) string {
	if !allowed(url, c.o) {
		return text
	}
	if c.md() {
		return "[" + text + "](" + url + ")"
	}
	return text + " (" + url + ")"
}

func (c *card) line(s string) { c.b.WriteString(s + "\n") }

// Card writes view 2, the room card, with view 3, its runs line. A line whose data is absent is
// left out. It prints no cursor: the caller that needs one has the summary.
func Card(w io.Writer, s summary.Summary, o Options) error {
	c := &card{o: o}
	needs := len(s.NeedsYou) > 0
	c.title(s)
	c.stateLine(s, needs)
	c.facts(s)
	c.needs(s)
	c.runs(s)
	c.notes(s)
	c.ranOn(s)
	c.actions(s)
	_, err := io.WriteString(w, strings.TrimRight(c.b.String(), "\n")+"\n")
	return err
}

func (c *card) title(s summary.Summary) {
	h := c.text(s.Room, false)
	if i := s.Status.Issue; i != nil && i.Number > 0 {
		h += " · #" + strconv.Itoa(i.Number)
		if clean(i.Title) != "" {
			h += " " + c.text(i.Title, false)
		}
	}
	if c.md() {
		h = "### " + h
	}
	c.line(h)
}

func (c *card) stateLine(s summary.Summary, needs bool) {
	state := s.Status.State
	if state == "" {
		state = summary.Coarse(s.Status.Phase, s.Status.Phase, false)
	}
	word := stateWord(state, needs)
	if needs {
		word = c.bold(word)
	}
	parts := []string{word}
	if p := clean(s.Status.Phase); p != "" {
		parts = append(parts, c.text(p, false))
	}
	if r := repoOf(urlOf(s.Status.Issue), prURL(s.Status.PR)); r != "" {
		parts = append(parts, c.token(r, false))
	}
	if allowed(s.URL, c.o) {
		parts = append(parts, c.url("open the room", s.URL))
	}
	c.line(strings.Join(parts, " · "))
}

func urlOf(i *envelope.IssueFact) string {
	if i == nil {
		return ""
	}
	return i.URL
}

func prURL(p *envelope.PRFact) string {
	if p == nil {
		return ""
	}
	return p.URL
}

// facts writes the label/value rows: a table in Markdown, aligned lines on a terminal.
func (c *card) facts(s summary.Summary) {
	var rows [][2]string
	if r := s.Status.Run; r != nil {
		rows = append(rows, [2]string{"Run", c.runFact(*r)})
	}
	if v := c.spend(s); v != "" {
		rows = append(rows, [2]string{"Spend", v})
	}
	if v := c.prFact(s); v != "" {
		rows = append(rows, [2]string{"PR", v})
	}
	if s.Status.Next != "" {
		rows = append(rows, [2]string{"Next", c.broker(s.Status.Next)})
	}
	if s.Status.Reason != "" {
		rows = append(rows, [2]string{"Why", c.text(s.Status.Reason, c.md())})
	}
	if len(rows) == 0 {
		return
	}
	c.line("")
	if c.md() {
		c.line("| | |\n|---|---|")
		for _, r := range rows {
			c.line("| " + r[0] + " | " + r[1] + " |")
		}
		return
	}
	for _, r := range rows {
		c.line(r[0] + strings.Repeat(" ", 6-utf8.RuneCountInString(r[0])) + r[1])
	}
}

// broker is a sentence the broker wrote (what happens next): cleaned, as it may hold a code span of
// its own, and without a pipe in a table cell.
func (c *card) broker(s string) string {
	return strings.ReplaceAll(clean(s), "|", cellPipe)
}

func (c *card) runFact(r envelope.RunFact) string {
	s := c.text(r.Role, c.md()) + " " + c.token(r.ID, c.md())
	if t := clean(r.Trigger); t != "" {
		s += " (" + c.words(r.Trigger, triggerWords(r.Trigger), c.md()) + ")"
	}
	if !r.StartedAt.IsZero() {
		s += " · started " + stamp(r.StartedAt, c.o.Now) + ", " + ago(r.StartedAt, c.o.Now)
	}
	return s
}

// spend is the task's dollars and tokens. A spend of 0 says nothing (an unpriced run costs 0), so
// the tokens read on their own, against their limit.
func (c *card) spend(s summary.Summary) string {
	sp, b := s.Status.Spend, s.Status.Budget
	if sp != nil && sp.UsedMicroUSD > 0 {
		out := dollars(sp.UsedMicroUSD)
		if sp.LimitMicroUSD > 0 {
			out += fmt.Sprintf(" of %s (%d %%)", dollars(sp.LimitMicroUSD), percent(sp.UsedMicroUSD, sp.LimitMicroUSD))
		}
		if b != nil && b.UsedTokens > 0 {
			out += " · " + tokens(b.UsedTokens) + " tokens"
		}
		return out
	}
	if b == nil || b.UsedTokens <= 0 && b.LimitTokens <= 0 {
		return ""
	}
	if b.LimitTokens > 0 {
		return fmt.Sprintf("%s of %s tokens (%d %%)", tokens(b.UsedTokens), tokens(b.LimitTokens), percent(b.UsedTokens, b.LimitTokens))
	}
	return tokens(b.UsedTokens) + " tokens"
}

func percent(used, limit int64) int64 { return (100*used + limit/2) / limit }

func (c *card) prFact(s summary.Summary) string {
	var parts []string
	if p := s.Status.PR; p != nil && p.Number > 0 {
		label := "#" + strconv.Itoa(p.Number)
		if c.md() && allowed(p.URL, c.o) {
			label = "[" + label + "](" + p.URL + ")"
		} else if allowed(p.URL, c.o) {
			label += " (" + p.URL + ")"
		}
		parts = append(parts, label)
	}
	if v := s.Status.LastVerdict; v != nil {
		parts = append(parts, c.text(v.By, c.md())+" "+c.words(v.Verdict, verdictWords(v.Verdict), c.md())+" at "+stamp(v.At, c.o.Now))
	}
	return strings.Join(parts, " · ")
}

func (c *card) needs(s summary.Summary) {
	if len(s.NeedsYou) == 0 {
		return
	}
	c.line("")
	c.line(c.head("Needs you"))
	for i, n := range s.NeedsYou {
		what := c.quoted(n.What, false)
		item, label := what, "open the room"
		switch n.Kind {
		case "approval":
			item, label = "approve "+what, "decide in the room"
			if !n.Deadline.IsZero() {
				item += " by " + stamp(n.Deadline, c.o.Now)
			}
		case "merge":
			label = "open the pull request"
		case "answer":
			label = "answer on the issue"
		}
		if allowed(n.URL, c.o) {
			if c.md() {
				item += " → " + c.url(label, n.URL)
			} else {
				item += " → " + n.URL
			}
		}
		c.line(strconv.Itoa(i+1) + ". " + item)
	}
}

// runs is view 3, the runs line: every run, oldest first, one phrase each.
func (c *card) runs(s summary.Summary) {
	if len(s.Runs) == 0 {
		return
	}
	parts := make([]string, len(s.Runs))
	for i, r := range s.Runs {
		parts[i] = c.runPhrase(r)
	}
	c.line("")
	c.line(c.head("Runs") + " " + strings.Join(parts, " → "))
}

// runPhrase is a run as the line reads it: its role, its trigger, how it ended, how long it took
// and what it cost. A run with no end and no outcome is running, and its time counts to Now. An
// outcome the factory set without an end (a stopped task's last run) is that outcome, never
// running, and has no time to count.
func (c *card) runPhrase(r envelope.RunFact) string {
	reviews := r.Role == "reviewer" || r.Role == "tester"
	s := c.text(r.Role, false)
	if !reviews && clean(r.Trigger) != "" {
		s += " (" + c.words(r.Trigger, triggerWords(r.Trigger), false) + ")"
	}
	var how string
	switch {
	case r.Outcome == "" && r.EndedAt.IsZero():
		how = c.bold("running")
	case r.Outcome == "":
		how = "ended"
	case reviews && r.Outcome == "succeeded" && r.Verdict != "":
		how = c.words(r.Verdict, verdictWords(r.Verdict), false)
	default:
		how = c.words(r.Outcome, outcomeWords(r.Outcome), false)
	}
	s += " " + how
	if d, ok := duration(r, c.o.Now); ok {
		s += " · " + d
	}
	if r.MicroUSD > 0 {
		s += " · " + dollars(r.MicroUSD)
	}
	return s
}

// duration is how long a run took, or has taken so far.
func duration(r envelope.RunFact, now time.Time) (string, bool) {
	end := r.EndedAt
	switch {
	case r.StartedAt.IsZero():
		return "", false
	case end.IsZero() && r.Outcome == "":
		end = now
	case end.IsZero():
		return "", false
	}
	m := max(0, int(end.Sub(r.StartedAt)/time.Minute))
	switch {
	case m < 1:
		return "under 1 min", true
	case m < 60:
		return strconv.Itoa(m) + " min", true
	case m%60 == 0:
		return strconv.Itoa(m/60) + " h", true
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60), true
}

func (c *card) notes(s summary.Summary) {
	items := s.Notes.Items
	if len(items) == 0 {
		return
	}
	if len(items) > maxCardNotes {
		items = items[len(items)-maxCardNotes:]
	}
	c.line("")
	c.line(c.head("Agents' notes") + ", their claims, not checked")
	for _, n := range items {
		c.line("- " + stamp(n.At, c.o.Now) + " " + c.token(n.Run, false) + " " + c.quoted(n.Text, false))
	}
}

func (c *card) ranOn(s summary.Summary) {
	r := s.Status.Run
	if r == nil || r.Provenance == nil {
		return
	}
	var parts []string
	if v := r.Provenance.FactoryVersion; v != "" {
		parts = append(parts, "factory "+c.token(v, false))
	}
	for _, m := range r.Provenance.ServedModels {
		parts = append(parts, c.token(m, false))
	}
	if len(parts) == 0 {
		return
	}
	c.line("")
	c.line("Ran on " + strings.Join(parts, " · "))
}

func (c *card) actions(s summary.Summary) {
	var can []string
	for _, a := range s.Actions {
		switch a.Kind {
		case "stop":
			can = append(can, "stop it with the label "+c.lit("factory/stop"))
		default:
			can = append(can, c.text(a.What, false))
		}
	}
	if len(can) == 0 {
		return
	}
	c.line("")
	c.line(c.head("You can") + " " + strings.Join(can, " · "))
}

// lit is a fixed literal of this package, in a code span in Markdown.
func (c *card) lit(s string) string {
	if c.md() {
		return "`" + s + "`"
	}
	return s
}
