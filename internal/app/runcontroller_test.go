// SPDX-License-Identifier: Apache-2.0

package app

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunControllerRefusesABadConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("namespace: agents\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := RunController(t.Context(), slog.New(slog.DiscardHandler), func(k string) string {
		return map[string]string{"RUN_CONTROLLER_CONFIG": p}[k]
	})
	if err == nil || !strings.Contains(err.Error(), "images.harness is required") {
		t.Fatalf("want the config error, got %v", err)
	}
}
