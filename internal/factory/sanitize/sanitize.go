// SPDX-License-Identifier: Apache-2.0

// Package sanitize neutralises text written outside the platform before any agent reads it
// (external review G2, ruling R43). Every 2025–26 agent incident entered through such text:
// invisible instructions, and markdown images that exfiltrate when rendered. It is a filter,
// not a boundary: egress policy and the merge gate's paths are the controls.
//
// Ruling SH: it neutralises instead of parsing. A regex cannot follow CommonMark, so every
// trigger of an image, a reference definition or raw HTML is defused wherever it appears, code
// spans included. The cost, accepted by the ruling, is a stray backslash or entity in benign text.
package sanitize

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Report counts what Text removed or replaced. The intake logs it, so an injection attempt shows.
type Report struct {
	Invisible int // default-ignorable code points: format (Cf), variation selectors, tags, fillers
	Control   int // C0 and C1 controls other than \n and \t; U+2028 and U+2029, made \n
	Images    int // markdown and HTML images, reduced to their alt text or defused
	Markup    int // other raw HTML tags, reference definitions and the input's ⟦ ⟧, defused
}

// Changed is whether Text altered anything.
func (r Report) Changed() bool { return r.Invisible+r.Control+r.Images+r.Markup > 0 }

// String is the report as one log-safe line: counts only, never the removed text.
func (r Report) String() string {
	return fmt.Sprintf("%d invisible and %d control characters removed, %d images and %d other markup "+
		"(raw HTML, reference definitions, brackets) defused", r.Invisible, r.Control, r.Images, r.Markup)
}

var (
	// ![alt](url "title") and ![alt][ref] on one line: the URL goes, the alt text stays (R43).
	// Only a convenience: whatever it misses is still defused by its "![" below.
	mdImage = regexp.MustCompile(`!\[([^\]\n]*)\](?:\([^)\n]*\)|\[[^\]\n]*\])`)
	htmlImg = regexp.MustCompile(`(?i)<img\b[^>]*>`)
	// CommonMark raw HTML opens with "<" and a letter, "/", "!" or "?": tags, comments,
	// declarations and processing instructions. "<" before a space or a digit stays.
	htmlOpen = regexp.MustCompile(`<[A-Za-z/!?]`)
)

// invisible is a character that renders as nothing yet reaches the model: every default-ignorable
// code point. The categories cover R43's enumeration (U+200B–200F, 202A–202E, 2060–2064,
// 2066–2069, FEFF, the tags) and what it missed: U+061C, 206A–206F, 00AD, 180E, FFF9–FFFB,
// 1D173–1D17A, the variation selectors that carry the 2025 "emoji smuggling" payloads, the
// Hangul fillers, U+034F, 17B4–17B5, 2065, FFF0–FFF8 and the rest of the tags plane up to E0FFF.
func invisible(r rune) bool {
	return unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Variation_Selector, r) ||
		unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r)
}

// lookalike maps the input's own ⟦ and ⟧ away, so every ⟦…⟧ in the output is a token of ours.
func lookalike(r rune) (rune, bool) {
	switch r {
	case '⟦':
		return '〚', true
	case '⟧':
		return '〛', true
	}
	return r, false
}

// Text removes invisible and control characters, then defuses every image, reference
// definition and raw HTML tag. The order matters: an image split by a zero-width space is
// still an image. Neither step inserts what the first removes, and the second repeats until
// its output stops changing, so no replacement can complete a new image with its neighbours.
//
// Text has no length bound of its own: every step is linear (RE2), and callers bound the
// input (Snapshot truncates; RunLore's text is bounded by its caller).
func Text(s string) (string, Report) {
	var rep Report
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\u2028' || r == '\u2029': // a line break the maintainer never saw as one
			rep.Control++
			return '\n'
		case invisible(r):
			rep.Invisible++
			return -1
		case unicode.IsControl(r) && r != '\n' && r != '\t':
			rep.Control++
			return -1
		}
		if m, ok := lookalike(r); ok {
			rep.Markup++
			return m
		}
		return r
	}, s)
	for {
		next := defuse(s, &rep)
		if next == s {
			return s, rep
		}
		s = next
	}
}

// defuse is one pass of the markup step. Its tokens open with "⟦", which no markdown syntax
// joins; its escapes leave the text readable ("!\[", "]\:", "&lt;"). An entity, not a
// backslash, escapes "<": a backslash already before it would escape ours instead.
func defuse(s string, rep *Report) string {
	s = mdImage.ReplaceAllStringFunc(s, func(m string) string {
		rep.Images++
		return "⟦image: " + mdImage.FindStringSubmatch(m)[1] + "⟧"
	})
	s = htmlImg.ReplaceAllStringFunc(s, func(string) string {
		rep.Images++
		return "⟦image⟧"
	})
	// No markdown image exists without "![", however its destination or alt text is written.
	rep.Images += strings.Count(s, "![")
	s = strings.ReplaceAll(s, "![", `!\[`)
	// No reference definition exists without "]:"; a definition may span lines, sit in a quote
	// or a list, so the pair is defused wherever it appears.
	rep.Markup += strings.Count(s, "]:")
	s = strings.ReplaceAll(s, "]:", `]\:`)
	return htmlOpen.ReplaceAllStringFunc(s, func(m string) string {
		rep.Markup++
		return "&lt;" + m[1:]
	})
}
