// SPDX-License-Identifier: Apache-2.0

package roomview

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/Smana/agent-platform/internal/summary"
)

// Row is one room of GET /api/rooms, the fields the overview reads.
type Row struct {
	ID          string        `json:"id"`
	URL         string        `json:"url"`
	Phase       string        `json:"phase"`
	State       string        `json:"state"`
	Repository  string        `json:"repository"`
	Issue       int           `json:"issue"`
	Title       string        `json:"title"`
	Task        string        `json:"task"`
	PR          int           `json:"pr"`
	Reason      string        `json:"reason"`
	LastSeq     int64         `json:"lastSeq"`
	LastEventAt time.Time     `json:"lastEventAt"`
	NeedsMe     bool          `json:"needsMe"`
	NeedsYou    int           `json:"needsYou"`
	Need        *summary.Need `json:"need,omitempty"`
	// Step is the one thing the caller does for the room and where: the same sentence as the card's.
	Step *summary.Step `json:"step,omitempty"`
}

// endedAfter is how long a room may have ended before the overview hides it.
const endedAfter = 7 * 24 * time.Hour

// needsYou reports whether an item of the room names the caller: the broker sets Need or NeedsYou
// only then, so NeedsMe (someone) never reads as "needs you".
func (r Row) needsYou() bool { return r.Need != nil || r.NeedsYou > 0 }

func (r Row) ended() bool {
	switch r.State {
	case summary.StateDone, summary.StateFailed, summary.StateStopped:
		return true
	}
	return false
}

// group orders the rows: those that need you, then running (and waiting), queued and ended.
func (r Row) group() int {
	switch {
	case r.needsYou():
		return 0
	case r.State == summary.StateQueued:
		return 2
	case r.ended():
		return 3
	}
	return 1
}

// Overview writes view 1, one row per room: the ones that need the caller first, newest first within
// a group. Rooms that ended more than a week ago are left out, and counted in a footer, unless
// o.All.
func Overview(w io.Writer, rows []Row, o Options) error {
	shown := make([]Row, 0, len(rows))
	hidden := 0
	for _, r := range rows {
		if !o.All && r.ended() && !r.LastEventAt.IsZero() && o.Now.Sub(r.LastEventAt) > endedAfter {
			hidden++
			continue
		}
		shown = append(shown, r)
	}
	sort.SliceStable(shown, func(i, j int) bool {
		a, b := shown[i], shown[j]
		if a.group() != b.group() {
			return a.group() < b.group()
		}
		if !a.LastEventAt.Equal(b.LastEventAt) {
			return a.LastEventAt.After(b.LastEventAt)
		}
		return a.ID < b.ID
	})
	needs := 0
	repos := map[string]bool{}
	for _, r := range shown {
		if r.needsYou() {
			needs++
		}
		if clean(r.Repository) != "" {
			repos[r.Repository] = true
		}
	}
	mixed := len(repos) > 1

	md := o.Format == Markdown
	head := fmt.Sprintf("Factory rooms · %d open", len(shown))
	if md {
		head = fmt.Sprintf("**Factory rooms** · %d open", len(shown))
	}
	if needs > 0 {
		head += fmt.Sprintf(" · %d needs you", needs)
	}
	var b strings.Builder
	b.WriteString(head + "\n")
	if len(shown) > 0 {
		b.WriteString("\n")
		// The task leads, as a developer names a room, and the room id comes last.
		cols := []string{"Task"}
		if mixed {
			cols = append(cols, "Repo")
		}
		cols = append(cols, "State", "Where it is", "PR", "Updated", "Room")
		var table [][]string
		for _, r := range shown {
			if md {
				table = append(table, markdownRow(r, mixed, o))
			} else {
				table = append(table, textRow(r, mixed, o))
			}
		}
		if md {
			writeTable(&b, cols, table)
		} else {
			writeColumns(&b, cols, table, o.Format == TextColor)
		}
	}
	if hidden > 0 {
		// The way to list them is the caller's own: a command on a terminal, the tool's argument in
		// the Markdown an agent reads.
		how, verb, noun := "roomctl rooms --all shows it", "see it", "room"
		if hidden > 1 {
			how, verb, noun = "roomctl rooms --all shows them", "see them", "rooms"
		}
		if md {
			how = "call `rooms_list` with `all: true` to " + verb
		}
		fmt.Fprintf(&b, "\n%d %s ended more than 7 days ago: %s.\n", hidden, noun, how)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// markdownRow is r as the cells of a Markdown table.
func markdownRow(r Row, mixed bool, o Options) []string {
	task := "—"
	switch {
	case r.Issue > 0 && clean(r.Title) != "":
		task = "#" + strconv.Itoa(r.Issue) + " " + escapeTitle(r.Title, true)
	case r.Issue > 0:
		task = "#" + strconv.Itoa(r.Issue)
	case clean(r.Title) != "":
		task = escapeTitle(r.Title, true)
	}
	state := stateWord(r.State, r.needsYou())
	if r.needsYou() {
		state = "**" + state + "**"
	}
	pr := "—"
	if r.PR > 0 {
		pr = "#" + strconv.Itoa(r.PR)
		if repo := repoOf("https://github.com/" + r.Repository + "/issues/1"); repo != "" {
			if url := "https://github.com/" + repo + "/pull/" + strconv.Itoa(r.PR); allowed(url, o) {
				pr = "[" + pr + "](" + url + ")"
			}
		}
	}
	cells := []string{task}
	if mixed {
		repo := "—"
		if clean(r.Repository) != "" {
			repo = code(r.Repository, true)
		}
		cells = append(cells, repo)
	}
	return append(cells, state, markdownWhere(r, o), pr, updated(r, o), markdownRoom(r, o))
}

// markdownRoom is the room id, linked to its page when the URL is the broker's.
func markdownRoom(r Row, o Options) string {
	if !allowed(r.URL, o) {
		return escapeTitle(r.ID, true)
	}
	return "[" + escapeTitle(r.ID, true) + "](" + r.URL + ")"
}

// githubStep reports whether the caller's step is on GitHub (a PR to merge, an issue to answer): the cell then says so, in place of the
// need's own words, and links it.
func (r Row) githubStep() bool {
	return r.Step != nil && (r.Step.Kind == summary.StepPR || r.Step.Kind == summary.StepAnswer)
}

func markdownWhere(r Row, o Options) string {
	if r.githubStep() {
		return link(r.Step.Label, r.Step.URL, o, true)
	}
	if n := r.Need; n != nil {
		if n.Kind == "approval" {
			s := "approve " + code(n.What, true)
			if !n.Deadline.IsZero() {
				s += " by " + stamp(n.Deadline, o.Now)
			}
			return s
		}
		if p := needPhrase(n); p != "" {
			return p
		}
		return code(n.What, true)
	}
	return cellWords(r)
}

// needPhrase is a need of the broker's own kinds as a short phrase for a row's cell, "" for a need
// it does not know. The need's full text is the card's: a cell stays one short line.
func needPhrase(n *summary.Need) string {
	switch {
	case n.Kind == "merge" && digits(n.ID):
		return "merge #" + n.ID
	case n.Kind == "answer" && digits(n.ID):
		return "answer on #" + n.ID
	case n.Kind == "hold" && n.ID == "waiting_human_driver":
		return "hand the driver back"
	case n.Kind == "hold" && n.ID == "waiting_room_busy":
		return "wait for the other run to end"
	}
	return ""
}

// roomWords is the phase a room's row reads: its task's, else its own.
func roomWords(r Row) (raw, got string) {
	if r.Task != "" {
		return r.Task, phaseWords(r.Task)
	}
	return r.Phase, phaseWords(r.Phase)
}

// cellWords is the reason, else the phase, in words as plain text: a phrase this package knows is
// as is, and a value it does not is untrusted, so escaped.
func cellWords(r Row) string {
	raw, got := roomWords(r)
	if r.Reason != "" {
		raw, got = r.Reason, reasonWords(r.Reason)
	}
	if got == clean(raw) {
		return escapeTitle(got, true)
	}
	return got
}

// textRow is r as the cells of the terminal table.
func textRow(r Row, mixed bool, o Options) []string {
	task := "-"
	switch {
	case r.Issue > 0 && clean(r.Title) != "":
		task = "#" + strconv.Itoa(r.Issue) + " " + clean(r.Title)
	case r.Issue > 0:
		task = "#" + strconv.Itoa(r.Issue)
	case clean(r.Title) != "":
		task = clean(r.Title)
	}
	where := ""
	switch n := r.Need; {
	case r.githubStep():
		where = link(r.Step.Label, r.Step.URL, o, true)
	case n != nil && n.Kind == "approval":
		where = "approve " + quote(n.What)
		if !n.Deadline.IsZero() {
			where += " " + stamp(n.Deadline, o.Now)
		}
	case n != nil && needPhrase(n) != "":
		where = needPhrase(n)
	case n != nil:
		where = clean(n.What)
	case r.Reason != "":
		where = reasonWords(r.Reason)
	default:
		_, where = roomWords(r)
	}
	pr := "-"
	if r.PR > 0 {
		pr = "#" + strconv.Itoa(r.PR)
	}
	cells := []string{task}
	if mixed {
		repo := clean(r.Repository)
		if repo == "" {
			repo = "-"
		}
		cells = append(cells, repo)
	}
	return append(cells, stateWord(r.State, r.needsYou()), where, pr, updated(r, o), clean(r.ID))
}

func updated(r Row, o Options) string {
	if r.LastEventAt.IsZero() {
		if o.Format == Markdown {
			return "—"
		}
		return "-"
	}
	return ago(r.LastEventAt, o.Now)
}

// writeTable writes a GitHub Flavored Markdown table.
func writeTable(b *strings.Builder, cols []string, rows [][]string) {
	b.WriteString("| " + strings.Join(cols, " | ") + " |\n|" + strings.Repeat("---|", len(cols)) + "\n")
	for _, r := range rows {
		b.WriteString("| " + strings.Join(r, " | ") + " |\n")
	}
}

// writeColumns writes aligned columns, two spaces apart. It pads by hand, by display width (wide runes take two cells), rather than with
// text/tabwriter: colour codes would count as width there, and a padded cell keeps its colour.
func writeColumns(b *strings.Builder, cols []string, rows [][]string, color bool) {
	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = strings.ToUpper(c)
	}
	all := append([][]string{header}, rows...)
	width := make([]int, len(cols))
	for _, r := range all {
		for i, c := range r {
			width[i] = max(width[i], runewidth.StringWidth(c))
		}
	}
	for n, r := range all {
		var line strings.Builder
		for i, c := range r {
			cell := c
			if color && n > 0 && c == "needs you" && cols[i] == "State" {
				cell = "\x1b[1m" + c + "\x1b[0m"
			}
			line.WriteString(cell)
			if i < len(r)-1 {
				line.WriteString(strings.Repeat(" ", width[i]-runewidth.StringWidth(c)+2))
			}
		}
		b.WriteString(line.String() + "\n")
	}
}
