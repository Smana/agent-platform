// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DashboardConfig is one link the room page shows in its title strip, such as a Grafana
// dashboard. URL is a template: the page fills {room}, {run} and {task} with the room's ids,
// each URL-encoded, and hides a link whose placeholder the room has no value for.
type DashboardConfig struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// The bounds of human.dashboards: a strip of a few links, each label a chip's width.
const (
	MaxDashboards     = 8
	maxDashboardLabel = 40
)

var (
	placeholderRE = regexp.MustCompile(`\{[^{}]*\}`)
	placeholders  = map[string]bool{"{room}": true, "{run}": true, "{task}": true}
)

// validateDashboards reports every bad entry of human.dashboards.
func validateDashboards(ds []DashboardConfig) []error {
	var errs []error
	if len(ds) > MaxDashboards {
		errs = append(errs, fmt.Errorf("human.dashboards: at most %d links, got %d", MaxDashboards, len(ds)))
	}
	for i, d := range ds {
		at := fmt.Sprintf("human.dashboards[%d]", i)
		switch n := utf8.RuneCountInString(d.Label); {
		case strings.TrimSpace(d.Label) == "":
			errs = append(errs, fmt.Errorf("%s: label is required", at))
		case n > maxDashboardLabel:
			errs = append(errs, fmt.Errorf("%s: label is at most %d characters", at, maxDashboardLabel))
		case !utf8.ValidString(d.Label) || strings.IndexFunc(d.Label, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0:
			errs = append(errs, fmt.Errorf("%s: label is printable text", at))
		}
		if err := dashboardURL(d.URL); err != nil {
			errs = append(errs, fmt.Errorf("%s %q: %w", at, d.Label, err))
		}
	}
	return errs
}

// dashboardURL checks a template: known placeholders only, then, with each filled, an absolute
// https URL whose host is fixed, so no room id can choose where the link goes.
func dashboardURL(tmpl string) error {
	const sample = "placeholder0"
	for _, p := range placeholderRE.FindAllString(tmpl, -1) {
		if !placeholders[p] {
			return fmt.Errorf("unknown placeholder %q: only {room}, {run} and {task}", p)
		}
	}
	filled := placeholderRE.ReplaceAllString(tmpl, sample)
	if strings.ContainsAny(filled, "{}") {
		return errors.New("unmatched { or }")
	}
	u, err := url.Parse(filled)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return errors.New("url must be an absolute https:// URL")
	}
	if strings.Contains(u.Host, sample) {
		return errors.New("a placeholder only in the path, query or fragment, never the host")
	}
	return nil
}
