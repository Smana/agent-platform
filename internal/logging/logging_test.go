// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"bytes"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	cases := []struct {
		name, format, level string
		ok                  bool
		debug               bool   // a Debug line is written
		prefix              string // how the Info line starts
	}{
		{"defaults are JSON at info", "", "", true, false, `{"time"`},
		{"text at debug", "text", "debug", true, true, "time="},
		{"case does not matter", "JSON", "WARN", true, false, ""},
		{"an unknown format fails startup", "yaml", "", false, false, ""},
		{"an unknown level fails startup", "", "verbose", false, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			log, err := New(&buf, c.format, c.level)
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok = %v", err, c.ok)
			}
			if !c.ok {
				return
			}
			log.Debug("d")
			if got := strings.Contains(buf.String(), "d"); got != c.debug {
				t.Fatalf("debug written = %v, want %v: %q", got, c.debug, buf.String())
			}
			buf.Reset()
			log.Info("i")
			if !strings.HasPrefix(buf.String(), c.prefix) {
				t.Fatalf("info line %q does not start with %q", buf.String(), c.prefix)
			}
		})
	}
}
