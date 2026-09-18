// Package lsp speaks the Language Server Protocol to a local language server and
// exposes the answers as agent tools: diagnostics, go-to-definition, references
// and workspace symbols.
//
// It exists because string search cannot answer a whole class of questions. "Who
// implements this method", "what breaks if I change this signature" and "did I
// just introduce a type error" have exact answers that only a compiler has, and
// `grep` guesses at all three: a missed call site does not fail loudly, it fails
// as half a repository that no longer compiles.
//
// The two halves of the design that matter most are not the protocol:
//
//   - A language server is a long-lived child process that indexes a whole
//     module, so it is started once per workspace root, kept, and reaped. Doing
//     it per call would make every tool slower than the grep it replaces.
//   - It is an external binary that may simply not be installed. Everything here
//     degrades to "these tools do not exist" rather than to "the agent does not
//     start", because a capability that can take the whole agent down with it is
//     worse than a capability that is missing.
package lsp

import (
	"bytes"
	"fmt"
	"os"
	"strings"
)

// Position is a zero-based line and UTF-16 character offset, as LSP defines it.
//
// It is deliberately not the type the tools expose. LSP counts characters in
// UTF-16 code units, which is a historical choice that has nothing to do with
// how either a model or a human counts, and every conversion bug shows up as
// "jumped to the wrong line" rather than as an error.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a half-open span between two positions.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// LineCol is a 1-based line and 1-based character column, which is the
// vocabulary the tools use.
//
// The line is 1-based because that is what read_file prints in its gutter: a
// model reading line 42 must be able to pass 42, not 41. The column counts
// **runes**, not bytes and not UTF-16 units, because it is what someone counting
// along a line of text produces — and for the ASCII that makes up nearly all
// source code the three agree anyway.
type LineCol struct {
	Line   int
	Column int
}

// splitLines splits content into lines without their terminators, keeping a
// trailing "\r" out of the way so a column at the end of a CRLF line is the end
// of the visible text.
func splitLines(content []byte) [][]byte {
	if len(content) == 0 {
		return [][]byte{{}}
	}
	lines := bytes.Split(content, []byte("\n"))
	for i, l := range lines {
		lines[i] = bytes.TrimSuffix(l, []byte("\r"))
	}
	return lines
}

// ToLSP converts a 1-based rune LineCol into an LSP Position.
//
// Both coordinates are validated: a line past the end of the file, a
// non-positive line, or a column past the end of its line is an error rather
// than a silently clamped position. Clamping is the tempting option and the
// wrong one — it turns "you pointed at nothing" into "here is an answer about
// some other line", which is worse than saying no.
func ToLSP(content []byte, lc LineCol) (Position, error) {
	if lc.Line < 1 {
		return Position{}, fmt.Errorf("lsp: line %d is out of range (lines are 1-based)", lc.Line)
	}
	if lc.Column < 1 {
		return Position{}, fmt.Errorf("lsp: column %d is out of range (columns are 1-based)", lc.Column)
	}

	lines := splitLines(content)
	if lc.Line > len(lines) {
		return Position{}, fmt.Errorf("lsp: line %d is out of range (the file has %d lines)", lc.Line, len(lines))
	}
	line := lines[lc.Line-1]

	// A column one past the last rune is allowed: it is the position just after
	// the line's text, which is a legitimate place to point (the end of an
	// identifier).
	runeCount := len([]rune(string(line)))
	if lc.Column > runeCount+1 {
		return Position{}, fmt.Errorf("lsp: column %d is out of range (line %d has %d characters)",
			lc.Column, lc.Line, runeCount)
	}

	// Walk the runes up to the target column, accumulating UTF-16 width:
	// everything outside the BMP is a surrogate pair and therefore two units.
	// For ASCII — nearly all source code — one rune is one unit and the
	// conversion is the identity.
	runes := []rune(string(line))
	u16 := 0
	for i := 0; i < lc.Column-1 && i < len(runes); i++ {
		if runes[i] > 0xFFFF {
			u16 += 2
		} else {
			u16++
		}
	}
	return Position{Line: lc.Line - 1, Character: u16}, nil
}

// FromLSP converts an LSP Position back into a 1-based rune LineCol.
//
// A character offset that lands in the middle of a surrogate pair rounds down to
// the start of the pair: that is the position the server meant, and refusing the
// whole answer over half an emoji would help nobody.
func FromLSP(content []byte, p Position) (LineCol, error) {
	lines := splitLines(content)
	if p.Line < 0 || p.Line >= len(lines) {
		return LineCol{}, fmt.Errorf("lsp: position line %d is out of range (the file has %d lines)", p.Line, len(lines))
	}

	target := max(p.Character, 0)
	runes := []rune(string(lines[p.Line]))

	// Walk the rune boundaries, keeping the last one that is not past the
	// target. The boundaries are what a position can name: rune i starts at
	// some offset and ends at the next one, and a target inside a rune belongs
	// to the boundary before it.
	u16 := 0
	column := 1
	for i := 0; i < len(runes); i++ {
		next := u16
		if runes[i] > 0xFFFF {
			next += 2
		} else {
			next++
		}
		if next > target {
			break
		}
		u16 = next
		column = i + 2 // the boundary after rune i, in 1-based columns
	}
	return LineCol{Line: p.Line + 1, Column: column}, nil
}

// lineText returns one line's text (1-based), for showing context next to a
// position.
func lineText(content []byte, line int) string {
	lines := splitLines(content)
	if line < 1 || line > len(lines) {
		return ""
	}
	return string(lines[line-1])
}

// snippet renders the line a position points at, trimmed, so a tool result can
// show what it is talking about instead of only a coordinate.
func snippet(content []byte, lc LineCol) string {
	return strings.TrimSpace(lineText(content, lc.Line))
}

// SnippetAt reads a file and returns the trimmed text of one line in it.
//
// A tool that answers with a coordinate alone makes the model open the file to
// find out whether the answer is even relevant; the line costs one read here and
// saves a round trip there. A file that cannot be read yields an empty snippet
// rather than an error: the position is still the answer.
func SnippetAt(file string, at LineCol) string {
	content, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	return snippet(content, at)
}
