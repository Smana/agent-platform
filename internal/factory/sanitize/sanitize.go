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

	"golang.org/x/text/unicode/norm"
)

// Report counts what Text removed or replaced. The intake logs it, so an injection attempt shows.
// The NFKC fold is not counted: it rewrites ordinary text too (CJK punctuation, ligatures).
type Report struct {
	Invisible int  // default-ignorable code points: format (Cf), variation selectors, tags, fillers
	Control   int  // C0 and C1 controls other than \n and \t; U+2028 and U+2029, made \n
	Images    int  // markdown and HTML images, reduced to their alt text or defused
	Markup    int  // other raw HTML tags, reference definitions and the input's ⟦ ⟧, defused
	Fences    int  // look-alikes of the briefs' TASK-, ROOM- and QUEUED-DATA fences, replaced (Batch A I1)
	Withheld  bool // the markup step did not settle: the whole text was replaced by Withheld
}

// Changed is whether Text altered anything.
func (r Report) Changed() bool {
	return r.Withheld || r.Invisible+r.Control+r.Images+r.Markup+r.Fences > 0
}

// String is the report as one log-safe line: counts only, never the removed text.
func (r Report) String() string {
	if r.Withheld {
		return "text withheld: the markup step did not settle"
	}
	return fmt.Sprintf("%d invisible and %d control characters removed, %d images, %d other markup "+
		"(raw HTML, reference definitions, brackets) and %d fence look-alikes defused",
		r.Invisible, r.Control, r.Images, r.Markup, r.Fences)
}

// FenceLookalike replaces text that could pass for the brief's fence.
const FenceLookalike = "⟦fence lookalike⟧"

// Withheld replaces a text whose markup step did not settle: Text fails closed.
const Withheld = "⟦text withheld by the factory's sanitiser⟧"

const (
	// maxPasses caps the markup step's fixpoint. One pass defuses every trigger. Nothing it
	// writes is a trigger, except an image token's "⟧" between fence halves ("![TASK](x)DATA"),
	// which the second pass replaces; the third confirms. The cap leaves room for one more.
	maxPasses = 4
	// maxGrowth bounds the markup step's output against its input: its largest replacement
	// ratio is "TASKDATA" or "ROOMDATA" (8 bytes) to FenceLookalike (21), 2.625.
	maxGrowth = 3
)

var (
	// ![alt](url "title") and ![alt][ref] on one line: the URL goes, the alt text stays (R43).
	// Only a convenience: whatever it misses is still defused by its "![" below.
	mdImage = regexp.MustCompile(`!\[([^\]\n]*)\](?:\([^)\n]*\)|\[[^\]\n]*\])`)
	htmlImg = regexp.MustCompile(`(?i)<img\b[^>]*>`)
	// CommonMark raw HTML opens with "<" and a letter, "/", "!" or "?": tags, comments,
	// declarations and processing instructions. "<" before a space or a digit stays.
	htmlOpen = regexp.MustCompile(`<[A-Za-z/!?]`)
	// TASK, ROOM or QUEUED, then DATA, in any case, with up to three separators that are neither
	// letters, digits nor a line break between them: the briefs' fences are TASK-DATA-<nonce>
	// (FirstBrief, the snapshot), ROOM-DATA-<nonce> (SP2's brief) and QUEUED-DATA-<nonce> (the
	// revise brief), and a model does not compare nonces character by character. Each letter also
	// admits its common Cyrillic and Greek homoglyphs, which NFKC does not fold (a full UTS #39
	// skeleton would; this covers the fences).
	fenceLike = regexp.MustCompile(`(?i)(?:[tтτ][aаα][sѕ][kкκ]|[rг][oоο][oоο][mмμ]|[qԛ][uυ][eеε][uυ][eеε][dԁ])` +
		`[^\p{L}\p{N}\n]{0,3}[dԁ][aаα][tтτ][aаα]`)
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

// Text removes invisible and control characters, folds compatibility forms (NFKC), then defuses
// every fence look-alike, image, reference definition and raw HTML tag. The order matters: an
// image split by a zero-width space is still an image, and a fullwidth one is an image once
// folded; folding after the strip also composes what the strip left apart, so a second pass finds
// nothing to fold. No step inserts what an earlier one removes, and the last repeats until its
// output stops changing, so no replacement can complete a new image with its neighbours.
// A markup step that does not settle within maxPasses and maxGrowth fails closed (Withheld).
//
// Every step is linear (RE2), NFKC grows a string at most 11-fold (UAX #15) and the passes are
// capped; callers bound the input (Snapshot
// truncates; RunLore's text is bounded by its caller).
func Text(s string) (string, Report) { return text(s, defuse) }

// text is Text with its markup step as a parameter, so a step that never settles is testable.
func text(s string, markup func(string, *Report) string) (string, Report) {
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
	s = norm.NFKC.String(s)
	limit := maxGrowth * len(s)
	for range maxPasses {
		next := markup(s, &rep)
		if next == s {
			return s, rep
		}
		if len(next) > limit {
			break
		}
		s = next
	}
	return Withheld, Report{Withheld: true}
}

// Fences replaces every look-alike of the briefs' fences with FenceLookalike, and counts them. It
// is Text's fence step alone, for text that is quoted without the rest: a message queued in a room.
func Fences(s string) (string, int) {
	n := 0
	s = fenceLike.ReplaceAllStringFunc(s, func(string) string {
		n++
		return FenceLookalike
	})
	return s, n
}

// defuse is one pass of the markup step. Its tokens open with "⟦", which no markdown syntax
// joins; its escapes leave the text readable ("!\[", "]\:", "&lt;"). An entity, not a
// backslash, escapes "<": a backslash already before it would escape ours instead.
func defuse(s string, rep *Report) string {
	// First, while the separators are still the input's: the escapes below only lengthen them.
	s, n := Fences(s)
	rep.Fences += n
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
