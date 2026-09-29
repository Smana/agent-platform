// SPDX-License-Identifier: Apache-2.0

// Package sanitize neutralises text written outside the platform before any agent reads it
// (external review G2, ruling R43). Every 2025–26 agent incident entered through such text:
// invisible instructions, and markdown images that exfiltrate when rendered. It is a filter,
// not a boundary: egress policy and the merge gate's paths are the controls.
package sanitize

import (
	"fmt"
	"regexp"
	"strings"
)

// Report counts what Text removed or replaced. The intake logs it, so an injection attempt shows.
type Report struct {
	Invisible int // zero-width, bidi, word-joiner and Unicode tag characters
	Control   int // C0 and C1 controls other than \n and \t
	Images    int // markdown and HTML images
}

// Changed is whether Text altered anything.
func (r Report) Changed() bool { return r.Invisible+r.Control+r.Images > 0 }

// String is the report as one log-safe line: counts only, never the removed text.
func (r Report) String() string {
	return fmt.Sprintf("%d invisible and %d control characters removed, %d images reduced to their alt text",
		r.Invisible, r.Control, r.Images)
}

var (
	// ![alt](url "title") and ![alt][ref]: the URL goes, the alt text stays (R43).
	mdImage = regexp.MustCompile(`!\[([^\]\n]*)\](?:\([^)\n]*\)|\[[^\]\n]*\])`)
	htmlImg = regexp.MustCompile(`(?i)<img\b[^>]*>`)
)

func invisible(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F, // zero-width space and joiners, LRM, RLM
		r >= 0x202A && r <= 0x202E,   // bidi embeddings and overrides
		r >= 0x2060 && r <= 0x2064,   // word joiner, invisible operators
		r >= 0x2066 && r <= 0x2069,   // bidi isolates
		r == 0xFEFF,                  // zero-width no-break space
		r >= 0xE0000 && r <= 0xE007F: // Unicode tags: invisible ASCII, the "ASCII smuggling" vector
		return true
	}
	return false
}

func control(r rune) bool {
	return (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7F && r <= 0x9F)
}

// Text removes invisible and control characters, then reduces every image to its alt text:
// in that order, so an image split by a zero-width space is still an image.
func Text(s string) (string, Report) {
	var rep Report
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.Map(func(r rune) rune {
		switch {
		case invisible(r):
			rep.Invisible++
			return -1
		case control(r):
			rep.Control++
			return -1
		}
		return r
	}, s)
	s = mdImage.ReplaceAllStringFunc(s, func(m string) string {
		rep.Images++
		return "[image: " + mdImage.FindStringSubmatch(m)[1] + "]"
	})
	s = htmlImg.ReplaceAllStringFunc(s, func(string) string {
		rep.Images++
		return "[image]"
	})
	return s, rep
}
