// SPDX-License-Identifier: Apache-2.0

package roomview

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/yuin/goldmark/ast"
	extast "github.com/yuin/goldmark/extension/ast"

	"github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/summary"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/*.md and testdata/*.txt from the current views")

var now = time.Date(2026, 10, 7, 19, 20, 0, 0, time.UTC)

// overviewRows are view 1's example in the design, plus a room that ended 8 days ago.
func overviewRows() []Row {
	const repo = "Smana/cloud-native-ref"
	return []Row{
		{ID: "huwttc64", URL: rooms + "/r/huwttc64", State: summary.StateDone, Repository: repo, Issue: 2230, Title: "docs: wrong port in guide", Task: v1alpha1.PhaseDone, PR: 2239, LastEventAt: now.Add(-2 * time.Hour)},
		{ID: "tvcpihkr", URL: rooms + "/r/tvcpihkr", State: summary.StateQueued, Repository: repo, Issue: 2244, Title: "links: dead link in runbook 04", Task: v1alpha1.PhaseQueued, Reason: "waiting_run_slot", LastEventAt: now.Add(-6 * time.Minute)},
		{ID: "k3vlmkwq", URL: rooms + "/r/k3vlmkwq", State: summary.StateRunning, Repository: repo, Issue: 2241, Title: "deps: bump renovate preset", Task: v1alpha1.PhaseImplementing, LastEventAt: now.Add(-1 * time.Minute)},
		{ID: "26zfnuxm", URL: rooms + "/r/26zfnuxm", State: summary.StateWaiting, Repository: repo, Issue: 2238, Title: "docs: chart version is wrong", Task: v1alpha1.PhaseAwaitingHuman, NeedsYou: 1, NeedsMe: true,
			Need:        &summary.Need{Kind: "approval", ID: "a1", What: "git push to agent/26zfnuxm", Deadline: now.Add(10 * time.Minute), URL: rooms + "/r/26zfnuxm#approval-a1"},
			LastEventAt: now.Add(-3 * time.Minute)},
		{ID: "oldroom1", URL: rooms + "/r/oldroom1", State: summary.StateDone, Repository: repo, Issue: 2100, Title: "docs: old", Task: v1alpha1.PhaseDone, LastEventAt: now.Add(-8 * 24 * time.Hour)},
	}
}

func render(t *testing.T, rows []Row, o Options) string {
	t.Helper()
	var b bytes.Buffer
	if err := Overview(&b, rows, o); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the view:\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}

func TestOverviewGolden(t *testing.T) {
	md := render(t, overviewRows(), Options{Format: Markdown, Now: now, Rooms: rooms})
	golden(t, "overview.md", md)
	golden(t, "overview.txt", render(t, overviewRows(), Options{Format: Text, Now: now, Rooms: rooms}))
	if !strings.HasSuffix(md, "\n\n1 room ended more than 7 days ago: `roomctl rooms --all` shows it.\n") {
		t.Errorf("footer: %q", md)
	}
	assertInert(t, md, rooms, []string{"git push"})
}

func TestOverviewOrder(t *testing.T) {
	rows := []Row{
		{ID: "ended1", State: summary.StateDone, LastEventAt: now.Add(-time.Minute)},
		{ID: "queued1", State: summary.StateQueued, LastEventAt: now.Add(-time.Minute)},
		{ID: "run-old", State: summary.StateRunning, LastEventAt: now.Add(-30 * time.Minute)},
		{ID: "run-new", State: summary.StateRunning, LastEventAt: now.Add(-time.Minute)},
		{ID: "need-old", State: summary.StateWaiting, NeedsYou: 1, Need: &summary.Need{Kind: "merge", What: "merge it"}, LastEventAt: now.Add(-20 * time.Minute)},
		{ID: "need-new", State: summary.StateRunning, NeedsYou: 1, Need: &summary.Need{Kind: "hold", What: "hold"}, LastEventAt: now.Add(-2 * time.Minute)},
	}
	out := render(t, rows, Options{Format: Text, Now: now, Rooms: rooms})
	last := -1
	for _, id := range []string{"need-new", "need-old", "run-new", "run-old", "queued1", "ended1"} {
		i := strings.Index(out, id)
		if i < 0 || i < last {
			t.Fatalf("%s is out of order in\n%s", id, out)
		}
		last = i
	}
}

func TestOverviewAllShowsEnded(t *testing.T) {
	out := render(t, overviewRows(), Options{Format: Markdown, Now: now, Rooms: rooms, All: true})
	if !strings.Contains(out, "oldroom1") || strings.Contains(out, "ended more than") {
		t.Errorf("--all: %s", out)
	}
	if !strings.Contains(out, "5 open") {
		t.Errorf("header counts the rows shown: %s", out)
	}
}

func TestOverviewRepoColumnOnlyWhenMixed(t *testing.T) {
	rows := overviewRows()
	for _, f := range []Format{Markdown, Text} {
		if out := render(t, rows, Options{Format: f, Now: now, Rooms: rooms}); strings.Contains(out, "Repo") || strings.Contains(out, "REPO") {
			t.Errorf("one repository, yet a Repo column:\n%s", out)
		}
	}
	rows[0].Repository = "Smana/other"
	md := render(t, rows, Options{Format: Markdown, Now: now, Rooms: rooms})
	header := strings.Split(strings.TrimSpace(strings.Split(md, "\n")[2]), "|")
	if !strings.Contains(md, "| Repo |") || len(header)-2 > 7 {
		t.Errorf("two repositories: want a Repo column in at most 7 columns, got %d:\n%s", len(header)-2, md)
	}
	if out := render(t, rows, Options{Format: Text, Now: now, Rooms: rooms}); !strings.Contains(out, "REPO") {
		t.Errorf("terminal view lacks the REPO column:\n%s", out)
	}
}

// tableCells counts the cells of every row of every table in md.
func tableCells(t *testing.T, md string) [][]int {
	t.Helper()
	root, _ := parse(md)
	var out [][]int
	_ = ast.Walk(root, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		if tb, ok := n.(*extast.Table); ok && enter {
			var counts []int
			for r := tb.FirstChild(); r != nil; r = r.NextSibling() {
				counts = append(counts, r.ChildCount())
			}
			out = append(out, counts)
		}
		return ast.WalkContinue, nil
	})
	return out
}

func TestOverviewHostile(t *testing.T) {
	b, err := os.ReadFile("../summary/testdata/hostile.json")
	if err != nil {
		t.Fatal(err)
	}
	var tc struct {
		Want struct {
			Status   struct{ Issue struct{ Title string } }
			NeedsYou []summary.Need
		}
	}
	if err := json.Unmarshal(b, &tc); err != nil {
		t.Fatal(err)
	}
	if len(tc.Want.NeedsYou) == 0 {
		t.Fatal("the fixture lost its hostile need")
	}
	title := tc.Want.Status.Issue.Title
	hostile := tc.Want.NeedsYou[0]
	hostile.What = "\x1b[2Jrm -rf / \u202e\u200b"
	pipes := []summary.Need{
		{Kind: "approval", What: "a|b", Deadline: now.Add(time.Hour)},
		{Kind: "approval", What: `a\|b`, Deadline: now.Add(time.Hour)},
		{Kind: "hold", What: "x | y"},
	}
	rows := []Row{{ID: "26zfnuxm", URL: rooms + "/r/26zfnuxm", State: summary.StateWaiting, Issue: 42, Title: title, Task: "<b>x</b> | y", Reason: "evil|reason",
		Repository: "Smana/cloud-native-ref", NeedsYou: 1, Need: &hostile, LastEventAt: now}}
	for i := range pipes {
		rows = append(rows, Row{ID: "pipe" + string(rune('a'+i)), URL: rooms + "/r/pipe" + string(rune('a'+i)), State: summary.StateWaiting, Issue: i + 1, Repository: "Smana/other", Title: "t|" + title,
			NeedsYou: 1, Need: &pipes[i], LastEventAt: now.Add(-time.Duration(i+1) * time.Minute)})
	}
	// Titles and reasons are escaped plain text, so only the needs must stay in code spans.
	untrusted := []string{"rm -rf", "a|b", `a\|b`, "x | y"}

	md := render(t, rows, Options{Format: Markdown, Now: now, Rooms: rooms})
	assertInert(t, md, rooms, untrusted)
	counts := tableCells(t, md)
	if len(counts) != 1 {
		t.Fatalf("want one table, got %d in\n%s", len(counts), md)
	}
	if len(counts[0]) != len(rows)+1 {
		t.Fatalf("table has %d rows, want %d: %s", len(counts[0]), len(rows)+1, md)
	}
	for i, n := range counts[0] {
		if n != counts[0][0] {
			t.Errorf("row %d has %d cells, the header has %d:\n%s", i, n, counts[0][0], md)
		}
	}

	for _, f := range []Format{Text, TextColor} {
		out := render(t, rows, Options{Format: f, Now: now, Rooms: rooms})
		for i := 0; i < len(out); i++ {
			if out[i] < 0x20 && out[i] != '\n' && (f != TextColor || out[i] != 0x1b) {
				t.Fatalf("format %d: byte %#x at %d of %q", f, out[i], i, out)
			}
		}
		if f == Text && strings.Contains(out, "\x1b") {
			t.Errorf("plain text holds an escape: %q", out)
		}
	}
	golden(t, "overview_hostile.md", md)
	golden(t, "overview_hostile.txt", render(t, rows, Options{Format: Text, Now: now, Rooms: rooms}))
}

func TestOverviewNoIssue(t *testing.T) {
	rows := []Row{{ID: "apiroom1", URL: rooms + "/r/apiroom1", State: summary.StateRunning, Task: v1alpha1.PhaseImplementing, LastEventAt: now}}
	md := render(t, rows, Options{Format: Markdown, Now: now, Rooms: rooms})
	if cells := strings.Split(strings.Split(md, "\n")[4], " | "); strings.Contains(md, "#0") || len(cells) < 2 || cells[1] != "—" {
		t.Errorf("Markdown: want an em dash in the Task cell, never #0:\n%s", md)
	}
	txt := render(t, rows, Options{Format: Text, Now: now, Rooms: rooms})
	if strings.Contains(txt, "#0") {
		t.Errorf("terminal: #0 in\n%s", txt)
	}
	line := strings.Fields(strings.Split(txt, "\n")[3])
	if len(line) < 2 || line[1] != "-" {
		t.Errorf("terminal: want - in the task column: %q", line)
	}
}

func TestOverviewNeedsYouOnlyForTheCaller(t *testing.T) {
	rows := []Row{{ID: "someone1", URL: rooms + "/r/someone1", State: summary.StateWaiting, NeedsMe: true, Issue: 7, Title: "t", Task: v1alpha1.PhaseAwaitingHuman, LastEventAt: now}}
	for _, f := range []Format{Markdown, Text} {
		out := render(t, rows, Options{Format: f, Now: now, Rooms: rooms})
		if !strings.Contains(out, "waiting for a human") || strings.Contains(out, "needs you") {
			t.Errorf("format %d: %s", f, out)
		}
	}
}

// Every phase the factory emits reads as words, never as its raw value.
func TestOverviewEveryPhaseHasWords(t *testing.T) {
	for _, p := range []string{
		v1alpha1.PhaseReceived, v1alpha1.PhaseRejected, v1alpha1.PhaseTriaged, v1alpha1.PhaseQueued, v1alpha1.PhaseImplementing,
		v1alpha1.PhaseNoOp, v1alpha1.PhaseReviewing, v1alpha1.PhaseAwaitingCI, v1alpha1.PhaseAutoMerging, v1alpha1.PhaseAwaitingHuman,
		v1alpha1.PhaseMerged, v1alpha1.PhaseVerifying, v1alpha1.PhaseDone, v1alpha1.PhaseReverted, v1alpha1.PhaseEscalated,
		v1alpha1.PhaseClosed, v1alpha1.PhaseStopped, v1alpha1.PhaseSplit,
	} {
		if got := phaseWords(p); got == p || got == "" {
			t.Errorf("phaseWords(%q) = %q, want words", p, got)
		}
	}
}

// An empty Repository is no second repository: one real repo and one empty makes no Repo column.
func TestOverviewEmptyRepositoryIsNotMixed(t *testing.T) {
	rows := overviewRows()[:2]
	rows[1].Repository = ""
	for _, f := range []Format{Markdown, Text} {
		if out := render(t, rows, Options{Format: f, Now: now, Rooms: rooms}); strings.Contains(out, "Repo") || strings.Contains(out, "REPO") {
			t.Errorf("format %d: a Repo column for one repository:\n%s", f, out)
		}
	}
}

// Terminal columns line up by display width: a CJK rune and an emoji take two cells.
func TestOverviewTerminalWidth(t *testing.T) {
	rows := overviewRows()[:3]
	rows[0].Title = "日本語のタイトル"
	rows[1].Title = "party 🎉 time"
	out := render(t, rows, Options{Format: Text, Now: now, Rooms: rooms})
	lines := strings.Split(strings.TrimSpace(out), "\n")[3:]
	col := -1
	for _, l := range lines {
		i := strings.Index(l, "running")
		if i < 0 {
			i = strings.Index(l, "queued")
		}
		if i < 0 {
			i = strings.Index(l, "done")
		}
		if i < 0 {
			t.Fatalf("no state in %q", l)
		}
		if w := runewidth.StringWidth(l[:i]); col >= 0 && w != col {
			t.Errorf("state column at width %d, want %d:\n%s", w, col, out)
		} else {
			col = w
		}
	}
}
