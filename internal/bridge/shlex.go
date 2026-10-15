// SPDX-License-Identifier: Apache-2.0

package bridge

import "strings"

// A word as bash passes it: text without quotes, bare with every quoted or
// escaped byte as \x00, and whether it holds an expansion bash resolves at run time.
type word struct {
	text     string
	bare     string
	expanded bool
}

func (w word) quoted() bool { return strings.IndexByte(w.bare, 0) >= 0 }

// A segment is one simple command: its words and the heredoc bodies fed to it.
type segment struct {
	words  []word
	bodies []string
}

type heredoc struct {
	seg    int
	delim  string
	strip  bool // <<-
	quoted bool // <<'EOF': the body is literal
}

// lex splits a command line as a POSIX shell would, as far as classification
// needs: quotes and backslashes group, ; | & ( ) newline and substitutions split
// outside quotes, a # comment ends the line, and heredoc bodies are set aside.
// A word holding $ outside single quotes ($VAR, $'…', $"…"), an unquoted brace
// expansion or an unquoted glob is expanded (N1). opaque reports what it cannot
// read at all: a command substitution or an unterminated quote.
func lex(line string) (segs []segment, opaque bool) {
	var (
		cur            segment
		text, bare     strings.Builder
		inWord, expand bool
		pending        []heredoc
	)
	put := func(ch byte, quoted bool) {
		text.WriteByte(ch)
		if quoted {
			bare.WriteByte(0)
		} else {
			bare.WriteByte(ch)
		}
		inWord = true
	}
	// dollar reports a $ at i that bash expands: anything but a trailing or lone $.
	dollar := func(i int, end string) bool {
		return line[i] == '$' && i+1 < len(line) && strings.IndexByte(end, line[i+1]) < 0
	}
	endWord := func() {
		if inWord {
			b := bare.String()
			cur.words = append(cur.words, word{text: text.String(), bare: b,
				expanded: expand || glob.MatchString(b) || brace.MatchString(b)})
			text.Reset()
			bare.Reset()
			inWord, expand = false, false
		}
	}
	endSeg := func() {
		endWord()
		segs = append(segs, cur)
		cur = segment{}
	}
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case ch == '\\':
			if i+1 < len(line) {
				i++
				if line[i] != '\n' {
					put(line[i], true)
				}
			}
		case ch == '\'':
			j := strings.IndexByte(line[i+1:], '\'')
			if j < 0 {
				j, opaque = len(line)-i-1, true
			}
			for k := i + 1; k < i+1+j; k++ {
				put(line[k], true)
			}
			inWord, i = true, i+1+j
		case ch == '"':
			inWord = true
			for i++; i < len(line) && line[i] != '"'; i++ {
				if line[i] == '`' || (line[i] == '$' && i+1 < len(line) && line[i+1] == '(') {
					opaque = true
				}
				if dollar(i, "\" \t\n") {
					expand = true
				}
				if line[i] == '\\' && i+1 < len(line) {
					i++
				}
				put(line[i], true)
			}
			if i >= len(line) {
				opaque = true
			}
		case ch == '#' && !inWord:
			for i+1 < len(line) && line[i+1] != '\n' {
				i++
			}
		case ch == '`' || (ch == '$' && i+1 < len(line) && line[i+1] == '('):
			opaque = true
			endSeg() // the substituted command is still classified, as its own segment
		case ch == '&' && ((i > 0 && line[i-1] == '>') || (i+1 < len(line) && line[i+1] == '>')):
			put(ch, false) // >&2, 2>&1, &>file
		case strings.HasPrefix(line[i:], "<<") && !strings.HasPrefix(line[i:], "<<<"):
			endWord()
			h, n := readDelim(line[i+2:])
			if h.delim == "" {
				opaque = true
			}
			h.seg = len(segs)
			pending = append(pending, h)
			i += 1 + n
		case ch == '\n':
			endSeg()
			var bad bool
			i, bad = bodies(line, i+1, pending, segs)
			opaque = opaque || bad
			pending = nil
		case strings.IndexByte(";|&()", ch) >= 0:
			endSeg()
		case ch == ' ' || ch == '\t':
			endWord()
		default:
			if dollar(i, " \t\n;|&()<>") {
				expand = true // $VAR, ${VAR}, $'…', $"…"
			}
			put(ch, false)
		}
	}
	endSeg()
	return segs, opaque
}

// readDelim reads a heredoc operator's tail (after <<): -, blanks, then the
// delimiter word. It returns the heredoc and the bytes consumed.
func readDelim(s string) (heredoc, int) {
	var h heredoc
	i := 0
	if i < len(s) && s[i] == '-' {
		h.strip = true
		i++
	}
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	var d strings.Builder
	for ; i < len(s) && strings.IndexByte(" \t\n;|&()<>", s[i]) < 0; i++ {
		switch s[i] {
		case '\'', '"':
			h.quoted = true
			if j := strings.IndexByte(s[i+1:], s[i]); j >= 0 {
				d.WriteString(s[i+1 : i+1+j])
				i += 1 + j
			}
		case '\\':
			h.quoted = true
			if i+1 < len(s) {
				i++
				d.WriteByte(s[i])
			}
		default:
			d.WriteByte(s[i])
		}
	}
	h.delim = d.String()
	return h, i
}

// bodies consumes the pending heredocs' lines from pos and attaches each body to
// its segment. It returns the index of the last byte consumed, and whether an
// unquoted body holds a substitution.
func bodies(line string, pos int, pending []heredoc, segs []segment) (int, bool) {
	opaque := false
	for _, h := range pending {
		var lines []string
		for pos < len(line) {
			end := strings.IndexByte(line[pos:], '\n')
			if end < 0 {
				end = len(line) - pos
			}
			l := line[pos : pos+end]
			pos += end + 1
			check := l
			if h.strip {
				check = strings.TrimLeft(l, "\t")
			}
			if check == h.delim {
				break
			}
			lines = append(lines, l)
		}
		body := strings.Join(lines, "\n")
		if !h.quoted && (strings.Contains(body, "`") || strings.Contains(body, "$(")) {
			opaque = true
		}
		segs[h.seg].bodies = append(segs[h.seg].bodies, body)
	}
	return pos - 1, opaque
}
