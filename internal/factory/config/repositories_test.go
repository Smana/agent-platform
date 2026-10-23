// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// listShaped rewrites the legacy good into the repositories shape (v2 D1): the per-repository
// keys move into one entry, and api.repositories goes, since the list derives it.
func listShaped(t *testing.T, legacy string) string {
	t.Helper()
	var top, entry []string
	name, inRepo := "", false
	for _, l := range strings.Split(legacy, "\n") {
		switch {
		case strings.HasPrefix(l, "repository: "):
			name = strings.TrimPrefix(l, "repository: ")
			continue
		case strings.HasPrefix(l, "maintainers:"), strings.HasPrefix(l, "triggerLabel:"), strings.HasPrefix(l, "classes:"),
			strings.HasPrefix(l, "merge:"), strings.HasPrefix(l, "schedules:"):
			inRepo = true
		case l != "" && !strings.HasPrefix(l, " "):
			inRepo = false
		case strings.HasPrefix(l, "  repositories: "):
			continue
		}
		if inRepo {
			entry = append(entry, "    "+l)
		} else {
			top = append(top, l)
		}
	}
	if name == "" || len(entry) == 0 {
		t.Fatal("listShaped: nothing to move")
	}
	return strings.Join(top, "\n") + "\nrepositories:\n  - name: " + name + "\n" + strings.Join(entry, "\n") + "\n"
}

// The compatibility guarantee: today's single-repository file loads as a one-entry list equal to
// the same settings written in the list shape, keeps its api.repositories, and leaves no legacy
// key for code to read.
func TestLegacyShapeIsTheOneEntryList(t *testing.T) {
	sched := "schedules:\n  - {name: link-rot, cron: \"0 6 * * 1\", class: docs-links, text: Fix broken links.}\n"
	legacyRaw := strings.Replace(good, "triggerLabel: factory/ready\n", "triggerLabel: factory/ready\ncontrolIssue: 7\n", 1) + sched
	legacy, err := Parse([]byte(legacyRaw))
	if err != nil {
		t.Fatal(err)
	}
	listRaw := listShaped(t, strings.Replace(legacyRaw, "controlIssue: 7\n", "", 1)) +
		"control: {repository: Smana/cloud-native-ref, issue: 7}\n"
	list, err := Parse([]byte(listRaw))
	if err != nil {
		t.Fatalf("%v\n%s", err, listRaw)
	}
	if len(legacy.Repositories) != 1 || !reflect.DeepEqual(legacy.Repositories, list.Repositories) {
		t.Fatalf("legacy %+v\nlist   %+v", legacy.Repositories, list.Repositories)
	}
	if want := (Control{Repository: "Smana/cloud-native-ref", Issue: 7}); legacy.ControlFor() != want || list.ControlFor() != want {
		t.Fatalf("control: legacy %+v, list %+v", legacy.ControlFor(), list.ControlFor())
	}
	if !reflect.DeepEqual(legacy.API.Repositories, []string{"Smana/cloud-native-ref"}) || !reflect.DeepEqual(list.API.Repositories, legacy.API.Repositories) {
		t.Fatalf("api.repositories: legacy %v, list %v", legacy.API.Repositories, list.API.Repositories)
	}
	if legacy.Repository != "" || legacy.Maintainers != nil || legacy.TriggerLabel != "" || legacy.ControlIssue != 0 ||
		legacy.Classes != nil || !reflect.ValueOf(legacy.Merge).IsZero() || legacy.Schedules != nil {
		t.Fatalf("Parse leaves no legacy key to read: %+v", legacy)
	}
	if legacy.NeedsMerger() != list.NeedsMerger() || legacy.AnyLive() != list.AnyLive() || legacy.AnyCheckReruns() != list.AnyCheckReruns() {
		t.Fatal("the merger's needs read the same from both shapes")
	}
}

// The legacy shape's own allowlist stays as written: before the list it was a separate key, and
// a file that listed more than its repository keeps admitting them.
func TestLegacyAPIRepositoriesKept(t *testing.T) {
	c, err := Parse([]byte(strings.Replace(good, "repositories: [Smana/cloud-native-ref]", "repositories: [Smana/cloud-native-ref, Smana/other]", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.API.Repositories, []string{"Smana/cloud-native-ref", "Smana/other"}) {
		t.Fatalf("api.repositories: %v", c.API.Repositories)
	}
}

// A hand-built legacy Config, as tests build them, reads as Parse would make it.
func TestReposOfAHandBuiltLegacyConfig(t *testing.T) {
	c := &Config{Repository: "Smana/x", Maintainers: []string{"a"}, ControlIssue: 3, Classes: map[string]Class{"docs": {Live: true}}}
	if rs := c.Repos(); len(rs) != 1 || rs[0].Name != "Smana/x" || !rs[0].IsMaintainer("A") {
		t.Fatalf("%+v", rs)
	}
	if c.ControlFor() != (Control{Repository: "Smana/x", Issue: 3}) || !c.AnyLive() {
		t.Fatalf("%+v", c.ControlFor())
	}
}

func entry(name, maintainer string) string {
	return fmt.Sprintf(`  - name: %s
    maintainers: [%s]
    triggerLabel: factory/ready
    classes: {docs-links: {}, revert: {}}
    merge: {requiredChecks: [ci], verifyChecks: [ci], leakScanCheck: ci, policyBotLogin: "p[bot]", mergerLogin: "m[bot]", verifyFor: 30m, revertWindow: 168h, breaker: {window: 10, maxReverts: 1}}
`, name, maintainer)
}

func twoRepos(t *testing.T) string {
	t.Helper()
	base := listShaped(t, good)
	return base + entry("Smana/ai-native-ref", "Other")
}

func TestTwoRepositories(t *testing.T) {
	c, err := Parse([]byte(twoRepos(t)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.API.Repositories, []string{"Smana/cloud-native-ref", "Smana/ai-native-ref"}) {
		t.Fatalf("api.repositories is derived from the list: %v", c.API.Repositories)
	}
	a, b := c.RepositoryFor("smana/CLOUD-native-ref"), c.RepositoryFor("Smana/ai-native-ref")
	if a.Name != "Smana/cloud-native-ref" || b.Name != "Smana/ai-native-ref" {
		t.Fatalf("matched case-insensitively: %q %q", a.Name, b.Name)
	}
	if !a.IsMaintainer("Smana") || a.IsMaintainer("Other") || !b.IsMaintainer("other") || b.IsMaintainer("Smana") {
		t.Fatal("maintainers are per repository (v2 D5)")
	}
	if b.NeedsMerger() || !a.NeedsMerger() || !c.NeedsMerger() {
		t.Fatal("the merger is needed when any repository's class is live or shadow")
	}
	if u := c.RepositoryFor("Smana/unlisted"); u.Name != "Smana/unlisted" || len(u.Maintainers) != 0 || len(u.Classes) != 0 || len(u.Merge.RequiredChecks) != 0 {
		t.Fatalf("an unlisted repository among several gets nothing: %+v", u)
	}
}

// One entry governs every task, whatever repository the task names: the behaviour before the list.
func TestOneEntryGovernsEveryName(t *testing.T) {
	c, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if r := c.RepositoryFor("Smana/elsewhere"); r.Name != "Smana/cloud-native-ref" || !r.IsMaintainer("Smana") {
		t.Fatalf("%+v", r)
	}
}

func TestBadRepositoriesFail(t *testing.T) {
	nine := listShaped(t, good)
	for i := range MaxRepositories {
		nine += entry(fmt.Sprintf("Smana/r%d", i), "Smana")
	}
	emptyList, _, _ := strings.Cut(listShaped(t, good), "repositories:")
	emptyList += "repositories: []\n"
	sched := "    schedules: [{name: a, cron: \"0 6 * * 1\", class: review, text: fix}]\n"
	for name, c := range map[string]struct{ raw, err string }{
		"more than eight":     {nine, "repositories holds 9 entries, at most 8"},
		"a duplicate":         {twoRepos(t) + entry("smana/AI-native-ref", "Smana"), "repository smana/AI-native-ref is listed twice"},
		"an entry unnamed":    {strings.Replace(twoRepos(t), "name: Smana/ai-native-ref", "name: ''", 1), `repository "" is not owner/name`},
		"no maintainers":      {strings.Replace(twoRepos(t), "maintainers: [Other]", "maintainers: []", 1), "Smana/ai-native-ref: maintainers is empty"},
		"no trigger label":    {strings.Replace(twoRepos(t), "classes: {docs-links: {}, revert: {}}", "classes: {docs-links: {}, revert: {}}\n    triggerLabel: ''", 1), "Smana/ai-native-ref: triggerLabel is required"},
		"no revert class":     {strings.Replace(twoRepos(t), "classes: {docs-links: {}, revert: {}}", "classes: {docs-links: {}}", 1), "Smana/ai-native-ref: class revert is required"},
		"merge left empty":    {twoRepos(t) + "  - {name: Smana/z, maintainers: [Smana], triggerLabel: x, classes: {docs-links: {}, revert: {}}}\n", "Smana/z: merge.requiredChecks is empty"},
		"an empty list":       {emptyList, "repositories is empty"},
		"both shapes":         {"repository: Smana/cloud-native-ref\n" + listShaped(t, good), "write repositories or repository, never both"},
		"legacy maintainers":  {strings.Replace(listShaped(t, good), "factoryLogin:", "maintainers: [Smana]\nfactoryLogin:", 1), "maintainers is per repository"},
		"legacy controlIssue": {strings.Replace(listShaped(t, good), "factoryLogin:", "controlIssue: 3\nfactoryLogin:", 1), "controlIssue is per repository"},
		"api.repositories with the list": {strings.Replace(listShaped(t, good), "  listen: \":8443\"", "  listen: \":8443\"\n  repositories: [Smana/cloud-native-ref]", 1),
			"api.repositories is derived from repositories"},
		"control in the legacy shape":       {good + "control: {repository: Smana/cloud-native-ref, issue: 1}\n", "control belongs to the repositories shape"},
		"control on an unlisted repository": {listShaped(t, good) + "control: {repository: Smana/other, issue: 1}\n", `control.repository "Smana/other" is not a listed repository`},
		"a negative control issue":          {listShaped(t, good) + "control: {repository: Smana/cloud-native-ref, issue: -1}\n", "control.issue must be 0 or more"},
		// The schedule's task key carries its name only (taskid.ScheduleKey): two repositories
		// sharing a name would dedupe one of them away.
		"a schedule name in two repositories": {listShaped(t, good) + sched + entry("Smana/b", "Smana") + sched, `schedule name "a" is used twice`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(c.raw))
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("err = %v, want one naming %q", err, c.err)
			}
		})
	}
}

// A class that declares a monitor reads main's checks through the merger, so it needs the key as
// live and shadow do.
func TestAMonitoredClassNeedsTheMerger(t *testing.T) {
	r := &Repository{Classes: map[string]Class{"dead-links": {Monitor: &Monitor{}}}}
	if !r.NeedsMerger() {
		t.Fatal("monitor needs the merger")
	}
	if (&Repository{Classes: map[string]Class{"x": {}}}).NeedsMerger() {
		t.Fatal("an off class does not")
	}
}
