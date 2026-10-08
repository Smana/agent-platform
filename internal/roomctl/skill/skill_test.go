// SPDX-License-Identifier: Apache-2.0

package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// frontMatter reads the top-level scalar fields of SKILL.md's YAML front matter.
func frontMatter(t *testing.T, b []byte) map[string]string {
	t.Helper()
	parts := strings.SplitN(string(b), "---\n", 3)
	if len(parts) != 3 || parts[0] != "" {
		t.Fatalf("no front matter in %q", b)
	}
	m := map[string]string{}
	for _, l := range strings.Split(parts[1], "\n") {
		if k, v, ok := strings.Cut(l, ": "); ok && !strings.HasPrefix(l, " ") {
			m[k] = v
		}
	}
	return m
}

func TestInstallWritesTheSkill(t *testing.T) {
	dir := t.TempDir()
	got, err := Install(dir, "v0.8.0")
	if err != nil || len(got) != 2 {
		t.Fatalf("Install = %v, %v", got, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "factory-handoff", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `roomctl-version: "v0.8.0"`) || strings.Contains(string(b), "{{VERSION}}") {
		t.Errorf("version not stamped:\n%s", b)
	}
	tpl, err := os.ReadFile(filepath.Join(dir, "factory-handoff", "references", "issue-template.md"))
	if err != nil || !strings.Contains(string(tpl), "Acceptance check") {
		t.Errorf("template: %v", err)
	}
	if fi, _ := os.Stat(got[0]); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	// A second install overwrites, stamping the new version.
	if err := os.WriteFile(got[0], []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(dir, "v0.9.0"); err != nil {
		t.Fatal(err)
	}
	if b, _ = os.ReadFile(got[0]); !strings.Contains(string(b), `"v0.9.0"`) {
		t.Errorf("not overwritten:\n%s", b)
	}
}

func TestFrontMatterMeetsTheAgentSkillsSpec(t *testing.T) {
	b, err := files.ReadFile("factory-handoff/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	fm := frontMatter(t, b)
	if fm["name"] != "factory-handoff" {
		t.Errorf("name = %q, must equal the directory name", fm["name"])
	}
	if d := fm["description"]; d == "" || len(d) > 1024 {
		t.Errorf("description length = %d, want 1..1024", len(d))
	}
	// Applying the label is the developer's decision (D1): no tool may do it.
	for _, bad := range []string{"gh issue edit", "label", "gh api"} {
		if strings.Contains(fm["allowed-tools"], bad) {
			t.Errorf("allowed-tools grants %q: %s", bad, fm["allowed-tools"])
		}
	}
}

func TestInstallRefusesASymlinkedSkillDir(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "factory-handoff")); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(dir, "v1"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want a symlink refusal", err)
	}
	if es, _ := os.ReadDir(outside); len(es) != 0 {
		t.Errorf("wrote through the symlink: %v", es)
	}
}

func TestInstallRefusesASymlinkedFile(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "factory-handoff"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "victim")
	if err := os.Symlink(target, filepath.Join(dir, "factory-handoff", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(dir, "v1"); err == nil {
		t.Fatal("want a refusal")
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("wrote through the file symlink")
	}
}
