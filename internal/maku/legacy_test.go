// SPDX-License-Identifier: Apache-2.0

package maku

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func putFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a test's own temp directory
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// roomctl's files move once, so a developer upgrading to maku does not sign in again.
func TestImportLegacy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, base string)
		want  bool
		check func(t *testing.T, base string)
	}{
		{"copies roomctl's files, owner-only, and skips temp files and directories", func(t *testing.T, base string) {
			putFile(t, filepath.Join(base, "roomctl", "config.json"), `{"url":"https://rooms.example"}`)
			putFile(t, filepath.Join(base, "roomctl", "token.json"), `{"refresh_token":"r"}`)
			putFile(t, filepath.Join(base, "roomctl", "cursors.json"), `{}`)
			putFile(t, filepath.Join(base, "roomctl", ".roomctl-123"), `partial`)
			putFile(t, filepath.Join(base, "roomctl", "sub", "x.json"), `{}`)
		}, true, func(t *testing.T, base string) {
			for _, f := range []string{"config.json", "token.json", "cursors.json"} {
				p := filepath.Join(base, "maku", f)
				if read(t, p) != read(t, filepath.Join(base, "roomctl", f)) {
					t.Errorf("%s differs from roomctl's", f)
				}
				if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
					t.Errorf("%s mode %v (%v), want 0600", f, st.Mode().Perm(), err)
				}
			}
			for _, f := range []string{".roomctl-123", "sub"} {
				if _, err := os.Stat(filepath.Join(base, "maku", f)); !os.IsNotExist(err) {
					t.Errorf("%s was copied", f)
				}
			}
			if st, _ := os.Stat(filepath.Join(base, "maku")); st.Mode().Perm() != 0o700 {
				t.Errorf("maku dir mode %v, want 0700", st.Mode().Perm())
			}
		}},
		{"a maku directory already there wins, and roomctl's is not read", func(t *testing.T, base string) {
			putFile(t, filepath.Join(base, "maku", "config.json"), `{"url":"https://new"}`)
			putFile(t, filepath.Join(base, "roomctl", "config.json"), `{"url":"https://old"}`)
		}, false, func(t *testing.T, base string) {
			if got := read(t, filepath.Join(base, "maku", "config.json")); got != `{"url":"https://new"}` {
				t.Errorf("maku config = %s, want the new one", got)
			}
		}},
		{"nothing to import creates nothing", func(*testing.T, string) {}, false, func(t *testing.T, base string) {
			if _, err := os.Stat(filepath.Join(base, "maku")); !os.IsNotExist(err) {
				t.Errorf("maku dir created with nothing to import: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			tc.setup(t, base)
			got, err := ImportLegacy(base, io.Discard)
			if err != nil || got != tc.want {
				t.Fatalf("ImportLegacy = %v, %v; want %v, nil", got, err, tc.want)
			}
			tc.check(t, base)
		})
	}
}

// Once is once: a second run, after roomctl's file changed, keeps what the first copied.
func TestImportLegacyRunsOnce(t *testing.T) {
	base := t.TempDir()
	putFile(t, filepath.Join(base, "roomctl", "config.json"), `{"url":"https://a"}`)
	if ok, err := ImportLegacy(base, io.Discard); !ok || err != nil {
		t.Fatalf("first import = %v, %v", ok, err)
	}
	putFile(t, filepath.Join(base, "roomctl", "config.json"), `{"url":"https://b"}`)
	if ok, err := ImportLegacy(base, io.Discard); ok || err != nil {
		t.Fatalf("second import = %v, %v; want false, nil", ok, err)
	}
	if got := read(t, filepath.Join(base, "maku", "config.json")); got != `{"url":"https://a"}` {
		t.Fatalf("config = %s, want the first import's", got)
	}
}

// Two first runs at once: the one that loses the rename finds maku's directory full and reports
// that nothing was imported, not an error.
func TestImportLegacyLosesARaceCleanly(t *testing.T) {
	base := t.TempDir()
	putFile(t, filepath.Join(base, "roomctl", "config.json"), `{"url":"https://a"}`)
	won := func() { putFile(t, filepath.Join(base, "maku", "config.json"), `{"url":"https://winner"}`) }
	if ok, err := importLegacy(base, io.Discard, won); ok || err != nil {
		t.Fatalf("losing import = %v, %v; want false, nil", ok, err)
	}
	if got := read(t, filepath.Join(base, "maku", "config.json")); got != `{"url":"https://winner"}` {
		t.Fatalf("config = %s, want the winner's", got)
	}
}

// A symlinked file is not followed: the developer is told it was not imported, so a second login
// is no surprise.
func TestImportLegacyNotesASkippedSymlink(t *testing.T) {
	base := t.TempDir()
	putFile(t, filepath.Join(base, "elsewhere", "token.json"), `{"refresh_token":"r"}`)
	putFile(t, filepath.Join(base, "roomctl", "config.json"), `{}`)
	if err := os.Symlink(filepath.Join(base, "elsewhere", "token.json"), filepath.Join(base, "roomctl", "token.json")); err != nil {
		t.Fatal(err)
	}
	var note strings.Builder
	if ok, err := ImportLegacy(base, &note); !ok || err != nil {
		t.Fatalf("ImportLegacy = %v, %v", ok, err)
	}
	if _, err := os.Lstat(filepath.Join(base, "maku", "token.json")); !os.IsNotExist(err) {
		t.Fatalf("the symlink was imported: %v", err)
	}
	if got := note.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "token.json") || !strings.Contains(got, "symlink") {
		t.Fatalf("note = %q, want one line naming token.json as a symlink", got)
	}
}
