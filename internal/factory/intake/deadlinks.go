// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/forge"
	"github.com/Smana/maku/internal/factory/mdlinks"
)

// LinkFinding is one entry of a dead-links run's findings.json: a link target lychee could not reach
// with a status that means gone (404, 410) or a name that does not resolve (nxdomain).
type LinkFinding struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	URL     string `json:"url"`
	Status  string `json:"status"`
	Archive string `json:"archive,omitempty"`
}

// ErrFindingsRefused marks a run whose findings the probe does not trust: the schedule logs and
// counts it, and starts no task.
var ErrFindingsRefused = errors.New("intake: dead-links findings refused")

// maxFindingsBytes bounds findings.json: 1,000 findings of 1 KiB each.
const maxFindingsBytes = 1 << 20

// ParseFindings reads findings.json strictly: an unknown key or trailing data refuses the whole
// file, so a changed detector never feeds the probe half-read findings.
func ParseFindings(raw []byte) ([]LinkFinding, error) {
	if len(raw) > maxFindingsBytes {
		return nil, fmt.Errorf("intake: findings.json is %d bytes, over %d", len(raw), maxFindingsBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var fs []LinkFinding
	if err := dec.Decode(&fs); err != nil {
		return nil, fmt.Errorf("intake: findings.json: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("intake: findings.json holds more than one document")
	}
	for i, f := range fs {
		if f.File == "" || f.URL == "" || f.Status == "" || f.Line < 1 {
			return nil, fmt.Errorf("intake: findings.json entry %d lacks file, line, url or status", i)
		}
	}
	return fs, nil
}

// Gone reports whether a finding's status means the link is dead: 404, 410 or nxdomain.
func Gone(status string) bool { return status == "404" || status == "410" || status == "nxdomain" }

func findingKey(f LinkFinding) string { return f.File + "\x00" + f.URL }

// Agreed are newest's findings that previous reported too, same file and URL, both as a gone
// status (MC5: a 429, 5xx or timeout in either run is no finding). Only gone entries count, so a
// pair repeated in a run is gone if any of its entries is. newest's line and archive are kept;
// the result is in file-then-URL order.
func Agreed(newest, previous []LinkFinding) []LinkFinding {
	prev := make(map[string]bool, len(previous))
	for _, p := range previous {
		if Gone(p.Status) {
			prev[findingKey(p)] = true
		}
	}
	seen := map[string]bool{}
	var out []LinkFinding
	for _, f := range newest {
		if k := findingKey(f); Gone(f.Status) && prev[k] && !seen[k] {
			seen[k] = true
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b LinkFinding) int { return strings.Compare(findingKey(a), findingKey(b)) })
	return out
}

var archiveRE = regexp.MustCompile(`^https://web\.archive\.org/web/[0-9]{1,14}/https://[^\s<>()]+$`)

// keepable decides one agreed finding. keep is false for what is simply no finding of this class:
// not a gone status, not a clean relative path the class allows and does not protect, not an
// https target (R52 cannot retarget http). A finding whose URL or archive breaks the Task CRD's
// shape is an error: the run is not trusted (ErrFindingsRefused).
func keepable(f LinkFinding, cl config.Class) (_ LinkFinding, drop string, err error) {
	if path.Clean(f.File) != f.File || strings.HasPrefix(f.File, "/") || strings.HasPrefix(f.File, "..") ||
		len(f.File) > 512 || !matches(cl.AllowedFiles, f.File) || matches(cl.ProtectedFiles, f.File) {
		return f, "path_not_allowed", nil
	}
	u, perr := url.Parse(f.URL)
	if perr != nil || u.Scheme != "http" && u.Scheme != "https" {
		return f, "", fmt.Errorf("%w: %q is no URL", ErrFindingsRefused, f.URL)
	}
	if u.Scheme == "http" {
		return f, "http", nil
	}
	if u.Host == "" || u.User != nil || len(f.URL) > 2048 || strings.ContainsAny(f.URL, " \t\r\n<>()") {
		return f, "", fmt.Errorf("%w: %q is not a link target the task may hold", ErrFindingsRefused, f.URL)
	}
	if f.Archive != "" && (!archiveRE.MatchString(f.Archive) || !strings.HasSuffix(f.Archive, "/"+f.URL) || len(f.Archive) > 2048) {
		return f, "", fmt.Errorf("%w: %q is no Wayback snapshot of %q", ErrFindingsRefused, f.Archive, f.URL)
	}
	return f, "", nil
}

func matches(pats []string, p string) bool {
	return slices.ContainsFunc(pats, func(s string) bool {
		re, err := regexp.Compile(s)
		return err == nil && re.MatchString(p)
	})
}

// DeadLinksMerger is what the dead-links probe reads through the merger App: the detector's runs
// and their artifacts, and a file on main.
type DeadLinksMerger interface {
	WorkflowRuns(ctx context.Context, workflow string) ([]forge.WorkflowRun, error)
	RunArtifact(ctx context.Context, runID int64, name, file string) ([]byte, error)
	FileAt(ctx context.Context, path, ref string) (string, error)
}

// detectorRuns are the newest two completed runs of the class's workflow that a schedule or a
// maintainer started on main, newest first; nil while there are fewer than two or either did not
// succeed.
func detectorRuns(ctx context.Context, m DeadLinksMerger, rc *config.Repository, cl config.Class) ([]forge.WorkflowRun, error) {
	runs, err := m.WorkflowRuns(ctx, cl.Oracle.Workflow)
	if err != nil {
		return nil, err
	}
	var two []forge.WorkflowRun
	for _, r := range runs { // newest first
		if !DetectorRun(r, rc) {
			continue
		}
		if two = append(two, r); len(two) == 2 {
			break
		}
	}
	if len(two) < 2 || two[0].Conclusion != "success" || two[1].Conclusion != "success" {
		return nil, nil
	}
	return two, nil
}

// deadLinks is the dead-links probe (MC5, MC6): the findings of the newest two completed runs of
// the class's workflow on main.
func deadLinks(ctx context.Context, m DeadLinksMerger, rc *config.Repository, cl config.Class) ([]LinkFinding, error) {
	two, err := detectorRuns(ctx, m, rc, cl)
	if err != nil || two == nil {
		return nil, err
	}
	return evaluateRuns(ctx, m, two, cl, slog.New(slog.DiscardHandler))
}

// evaluateRuns keeps what both runs report as gone, the class may fix, and is still a link target
// at its file on main: the first oracle.maxFindings in file-then-URL order. Malformed or
// out-of-shape findings refuse the run (ErrFindingsRefused). It logs one line with what it dropped.
func evaluateRuns(ctx context.Context, m DeadLinksMerger, two []forge.WorkflowRun, cl config.Class, log *slog.Logger) ([]LinkFinding, error) {
	var sets [2][]LinkFinding
	for i, r := range two {
		raw, err := m.RunArtifact(ctx, r.ID, "findings", "findings.json")
		if err != nil {
			return nil, err
		}
		if sets[i], err = ParseFindings(raw); err != nil {
			return nil, fmt.Errorf("%w: run %d: %w", ErrFindingsRefused, r.ID, err)
		}
	}
	var out []LinkFinding
	files := map[string][]string{}
	drops := map[string]int{}
	for _, f := range sets[0] {
		if !Gone(f.Status) {
			drops["status"]++
		}
	}
	agreed := Agreed(sets[0], sets[1])
	drops["unagreed"] = len(sets[0]) - drops["status"] - len(agreed)
	for _, f := range agreed {
		// Shape checks run on every agreed finding: a malformed one refuses the run wherever it sorts.
		k, why, err := keepable(f, cl)
		if err != nil {
			return nil, err
		}
		if why != "" {
			drops[why]++
			continue
		}
		if k.Archive == "" { // the oracle allows no move without a snapshot: unfixable, so not a task
			drops["no_archive"]++
			continue
		}
		if len(out) == cl.Oracle.MaxFindings {
			drops["over_cap"]++
			continue
		}
		ts, read := files[k.File]
		if !read {
			text, err := m.FileAt(ctx, k.File, "main")
			if err != nil {
				return nil, err
			}
			_, ts = mdlinks.Targets(text)
			files[k.File] = ts
		}
		if !slices.Contains(ts, k.URL) {
			drops["not_a_link_target"]++
			continue
		}
		out = append(out, k)
	}
	log.Info("dead-links evaluated", "runs", []int64{two[0].ID, two[1].ID}, "kept", len(out), "dropped", drops)
	return out, nil
}

// DetectorRun reports whether r is a run on main that a schedule started, or a maintainer by
// hand. A pull request's run reads the pull request's tree, and anyone else's dispatch is not the
// detector's verdict.
func DetectorRun(r forge.WorkflowRun, rc *config.Repository) bool {
	switch {
	case r.Branch != "main":
		return false
	case r.Event == "schedule":
		return true
	default:
		return r.Event == "workflow_dispatch" && rc.IsMaintainer(r.Actor)
	}
}

// deadLinksTask renders the findings as the task's text (MC6): validated paths and URLs and the
// statuses' fixed words, nothing else from the artifact. It stops adding findings once the text
// would pass maxBytes (caps.maxTextBytes, 0 for no bound), and says how many it left out: they are
// over the cap like any past oracle.maxFindings, and a later run pair picks them up.
func deadLinksTask(fs []LinkFinding, maxBytes int) (text string, spec []v1alpha1.Finding, left int) {
	const head = "Dead external links, each reported by the last two nightly runs of the dead-links workflow on main:\n\n"
	const foot = "\nChange only these link targets, in these files: each to the snapshot given, exactly as written. " +
		"Leave link text, prose and code blocks alone, and add or delete no file."
	var b strings.Builder
	b.WriteString(head)
	spec = make([]v1alpha1.Finding, 0, len(fs))
	for i, f := range fs {
		why := map[string]string{"404": "404 Not Found", "410": "410 Gone", "nxdomain": "the host name does not resolve"}[f.Status]
		line := fmt.Sprintf("%d. `%s`, line %d: <%s> (%s).", len(spec)+1, f.File, f.Line, f.URL, why)
		if f.Archive != "" {
			line += fmt.Sprintf(" Snapshot: <%s>.", f.Archive)
		}
		line += "\n"
		if maxBytes > 0 && b.Len()+len(line)+len(foot) > maxBytes {
			left = len(fs) - i
			break
		}
		b.WriteString(line)
		spec = append(spec, v1alpha1.Finding{File: f.File, URL: f.URL, Archive: f.Archive})
	}
	b.WriteString(foot)
	return b.String(), spec, left
}
