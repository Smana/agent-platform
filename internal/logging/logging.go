// SPDX-License-Identifier: Apache-2.0

// Package logging builds a binary's *slog.Logger: JSON in the cluster, text
// locally, each overridable by the environment the binary reads.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// New returns a logger writing to w. format is "json" (the default) or "text";
// level is "debug", "info" (the default), "warn" or "error". Anything else is
// an error, so a typo fails startup.
func New(w io.Writer, format, level string) (*slog.Logger, error) {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "", "info":
		lv = slog.LevelInfo
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		return nil, fmt.Errorf("logging: unknown level %q (debug, info, warn, error)", level)
	}
	opts := &slog.HandlerOptions{Level: lv}
	switch strings.ToLower(format) {
	case "", "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("logging: unknown format %q (json, text)", format)
}
