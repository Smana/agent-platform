// SPDX-License-Identifier: Apache-2.0

package filefence

import (
	"testing"

	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/forge"
)

func TestRefusal(t *testing.T) {
	cl := config.Class{AllowedFiles: []string{`apps/.+\.yaml`}, ProtectedFiles: []string{`^apps/secret/`}, ModifyOnly: true,
		MaxPatchFiles: 2, MaxModifiedLines: 4}
	m := func(p string, n int) forge.FileStat {
		return forge.FileStat{Path: p, Status: "modified", Additions: n, Deletions: n}
	}
	for name, c := range map[string]struct {
		stats []forge.FileStat
		want  string
	}{
		"inside":    {[]forge.FileStat{m("apps/a.yaml", 1)}, ""},
		"outside":   {[]forge.FileStat{m("mise.toml", 1)}, "file_not_allowed"},
		"anchored":  {[]forge.FileStat{m("x/apps/a.yaml", 1)}, "file_not_allowed"},
		"protected": {[]forge.FileStat{m("apps/secret/a.yaml", 1)}, "file_protected"},
		"added":     {[]forge.FileStat{{Path: "apps/a.yaml", Status: "added"}}, "file_added_or_deleted"},
		"files":     {[]forge.FileStat{m("apps/a.yaml", 1), m("apps/b.yaml", 1), m("apps/c.yaml", 1)}, "patch_too_large"},
		"lines":     {[]forge.FileStat{m("apps/a.yaml", 3)}, "patch_too_large"},
		"a rename":  {[]forge.FileStat{{Path: "apps/a.yaml", Previous: "mise.toml", Status: "modified"}}, "file_not_allowed"},
	} {
		if got := Refusal(cl, c.stats); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}
