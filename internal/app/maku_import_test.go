// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A roomctl directory maku cannot read costs a warning, never the command: the developer can
// still configure and log in again.
func TestMakuImportFailureOnlyWarns(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 directory")
	}
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	legacy := filepath.Join(base, "roomctl")
	if err := os.Mkdir(legacy, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(legacy, 0o700) }) //nolint:gosec // a test's own temp directory
	var out, errb strings.Builder
	if err := runMaku(t.Context(), []string{"help"}, &out, &errb); err != nil {
		t.Fatalf("help failed on an unreadable roomctl directory: %v", err)
	}
	if !strings.Contains(out.String(), "configure") {
		t.Fatalf("no usage on stdout: %q", out.String())
	}
	if w := errb.String(); strings.Count(w, "\n") != 1 || !strings.Contains(w, "roomctl") {
		t.Fatalf("stderr = %q, want one warning line naming roomctl", w)
	}
}
