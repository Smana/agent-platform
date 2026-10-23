// SPDX-License-Identifier: Apache-2.0

package semverdiff_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/reconciler"
	"github.com/Smana/agent-platform/internal/factory/semverdiff"
)

// A replay case is one merged Renovate PR of the host, as hack/renovate-replay.sh writes it.
type replayCase struct {
	Number int               `json:"number"`
	Update string            `json:"update"`
	Base   map[string]string `json:"base"`
	Head   map[string]string `json:"head"`
}

// replayCases reads the committed cases, and $SEMVER_REPLAY_DIR's when it is set (the whole
// replay, fetched by hand).
func replayCases(t *testing.T) []replayCase {
	t.Helper()
	dirs := []string{filepath.Join("testdata", "replay")}
	if d := os.Getenv("SEMVER_REPLAY_DIR"); d != "" {
		dirs = append(dirs, d)
	}
	seen := map[int]bool{} // a case in both places counts once
	var out []replayCase
	for _, d := range dirs {
		files, err := filepath.Glob(filepath.Join(d, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Fatalf("no replay case in %s", d)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var c replayCase
			if err := json.Unmarshal(b, &c); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			if !seen[c.Number] {
				seen[c.Number] = true
				out = append(out, c)
			}
		}
	}
	return out
}

func kind(update string) string {
	switch update {
	case "patch", "minor", "major", "digest":
		return update
	}
	return "other"
}

// summary renders "patch accepted 4 of 5, minor 0 of 1, ..." over the kinds, in a fixed order.
func summary(accepted, total map[string]int) string {
	var parts []string
	for _, k := range []string{"patch", "minor", "major", "digest", "other"} {
		word := ""
		if k == "patch" {
			word = "accepted "
		}
		parts = append(parts, fmt.Sprintf("%s %s%d of %d", k, word, accepted[k], total[k]))
	}
	return strings.Join(parts, ", ")
}

func TestReplay(t *testing.T) {
	cases := replayCases(t)
	if len(cases) < 8 {
		t.Fatalf("%d replay cases, the committed eight are missing", len(cases))
	}
	accepted, total := map[string]int{}, map[string]int{}
	for _, c := range cases {
		k := kind(c.Update)
		total[k]++
		ok, why := semverdiff.Patch(c.Base, c.Head)
		if ok {
			accepted[k]++
		}
		if ok && c.Update != "patch" {
			t.Errorf("#%d (%s): Patch accepted a bump Renovate did not call a patch", c.Number, c.Update)
		}
		t.Logf("#%d %s: accepted=%v %s", c.Number, c.Update, ok, why)
	}
	t.Log(summary(accepted, total))
}

// diffStats are what GitHub would count for a modified file: the lines a side has that the other
// has not, as a multiset.
func diffStats(c replayCase) []forge.FileStat {
	var out []forge.FileStat
	for _, p := range slices.Sorted(mapsKeys(c.Head)) {
		st := forge.FileStat{Path: p, Status: "modified"}
		switch {
		case c.Base[p] == "":
			st.Status = "added"
		case c.Head[p] == "":
			st.Status = "removed"
		}
		st.Additions, st.Deletions = lineDelta(c.Base[p], c.Head[p])
		out = append(out, st)
	}
	return out
}

func mapsKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func lineDelta(base, head string) (adds, dels int) {
	count := map[string]int{}
	for _, l := range strings.Split(base, "\n") {
		count[l]++
	}
	for _, l := range strings.Split(head, "\n") {
		if count[l] > 0 {
			count[l]--
			continue
		}
		adds++
	}
	for _, n := range count {
		dels += n
	}
	return adds, dels
}

// TestReplayWithinLimits runs the cases Patch accepts through the lane's fence as the first slice
// built it (allowedFiles, maxPatchFiles, maxModifiedLines, modifyOnly): shape and fence are two
// decisions, and a patch can pass the first and not the second. The allow list stands in for the
// declared one: the manifests a Renovate bump edits, never workflows, OpenTofu or docs.
func TestReplayWithinLimits(t *testing.T) {
	cl := config.Class{
		AllowedFiles:     []string{`(flux|infrastructure|security|apps|tooling|observability)/.+\.ya?ml`},
		ModifyOnly:       true,
		MaxPatchFiles:    3,
		MaxModifiedLines: 10,
	}
	var shaped, taken, patches int
	for _, c := range replayCases(t) {
		ok, _ := semverdiff.Patch(c.Base, c.Head)
		if c.Update == "patch" {
			patches++
		}
		if !ok {
			continue
		}
		shaped++
		in := reconciler.ArmInputs{Stats: diffStats(c), DiffRead: true}
		reason := ""
		for _, cond := range reconciler.Conditions() {
			if !cond.Declared(cl) {
				continue
			}
			if r, _ := cond.Check(cl, in); r != "" {
				reason = cond.Key + ": " + r
				break
			}
		}
		if reason == "" {
			taken++
		}
		t.Logf("#%d fence: %q", c.Number, reason)
	}
	t.Logf("patch merges %d, accepted by semver-patch %d, taken through the fence %d", patches, shaped, taken)
	if taken > shaped {
		t.Fatalf("the fence took %d of %d shaped diffs", taken, shaped)
	}
}
