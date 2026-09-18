package lsp

import "testing"

// TestToLSP covers the conversion that is the easiest thing in this package to
// get quietly wrong: LSP counts UTF-16 code units, everything a person or a
// model counts is runes. A mistake here does not fail — it points at the wrong
// column, or the wrong line, and the tool looks like it is answering about
// something else.
func TestToLSP(t *testing.T) {
	tests := []struct {
		name    string
		content string
		at      LineCol
		want    Position
		wantErr bool
	}{
		{name: "start of file", content: "hello\n", at: LineCol{1, 1}, want: Position{0, 0}},
		{name: "middle of a line", content: "hello\n", at: LineCol{1, 3}, want: Position{0, 2}},
		{name: "one past the last rune is allowed", content: "hello\n", at: LineCol{1, 6}, want: Position{0, 5}},
		{name: "second line", content: "one\ntwo\n", at: LineCol{2, 2}, want: Position{1, 1}},
		{name: "empty file", content: "", at: LineCol{1, 1}, want: Position{0, 0}},

		// CJK is in the BMP, so one character is one UTF-16 unit. The point of
		// these cases is that a byte-oriented implementation would be wrong:
		// "中" is three bytes.
		{name: "CJK counts as one unit", content: "中文abc", at: LineCol{1, 3}, want: Position{0, 2}},
		{name: "past CJK", content: "中文abc", at: LineCol{1, 6}, want: Position{0, 5}},

		// Emoji is outside the BMP: one rune, two UTF-16 units. This is the case
		// that separates "counts runes" from "counts UTF-16 units".
		{name: "emoji is two units", content: "😀x", at: LineCol{1, 2}, want: Position{0, 2}},
		{name: "after emoji", content: "😀x", at: LineCol{1, 3}, want: Position{0, 3}},
		{name: "two emoji", content: "😀😀", at: LineCol{1, 3}, want: Position{0, 4}},

		// CRLF: the "\r" is a line terminator, not content, so the end of the
		// visible line is before it.
		{name: "CRLF end of line", content: "ab\r\ncd\r\n", at: LineCol{1, 3}, want: Position{0, 2}},
		{name: "CRLF second line", content: "ab\r\ncd\r\n", at: LineCol{2, 1}, want: Position{1, 0}},

		{name: "line zero", content: "x", at: LineCol{0, 1}, wantErr: true},
		{name: "negative line", content: "x", at: LineCol{-1, 1}, wantErr: true},
		{name: "line past the end", content: "one\n", at: LineCol{3, 1}, wantErr: true},
		{name: "column zero", content: "one\n", at: LineCol{1, 0}, wantErr: true},
		{name: "column past the end", content: "one\n", at: LineCol{1, 9}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ToLSP([]byte(tt.content), tt.at)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %+v, got %+v (clamping is the thing this must not do)", tt.at, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ToLSP: %v", err)
			}
			if got != tt.want {
				t.Errorf("ToLSP(%+v) = %+v, want %+v", tt.at, got, tt.want)
			}
		})
	}
}

// TestFromLSP is the inverse, including the one asymmetry: a character offset
// inside a surrogate pair rounds down rather than failing.
func TestFromLSP(t *testing.T) {
	tests := []struct {
		name    string
		content string
		at      Position
		want    LineCol
		wantErr bool
	}{
		{name: "start", content: "hello\n", at: Position{0, 0}, want: LineCol{1, 1}},
		{name: "middle", content: "hello\n", at: Position{0, 2}, want: LineCol{1, 3}},
		{name: "past the end clamps to the end", content: "hello\n", at: Position{0, 99}, want: LineCol{1, 6}},
		{name: "second line", content: "one\ntwo\n", at: Position{1, 1}, want: LineCol{2, 2}},
		{name: "CJK", content: "中文abc", at: Position{0, 2}, want: LineCol{1, 3}},
		{name: "after emoji", content: "😀x", at: Position{0, 3}, want: LineCol{1, 3}},
		// Half of a surrogate pair is not a position a person can hold; the
		// start of the pair is what the server meant.
		{name: "inside a surrogate pair rounds down", content: "😀x", at: Position{0, 1}, want: LineCol{1, 1}},
		{name: "negative character", content: "abc", at: Position{0, -3}, want: LineCol{1, 1}},
		{name: "line past the end", content: "one\n", at: Position{5, 0}, wantErr: true},
		{name: "negative line", content: "one\n", at: Position{-1, 0}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FromLSP([]byte(tt.content), tt.at)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromLSP: %v", err)
			}
			if got != tt.want {
				t.Errorf("FromLSP(%+v) = %+v, want %+v", tt.at, got, tt.want)
			}
		})
	}
}

// TestPositionRoundTrip is the property that matters in practice: a position a
// server reports must convert back to the coordinates a tool printed, or
// "the answer is at line 42 column 8" is a lie.
func TestPositionRoundTrip(t *testing.T) {
	contents := []string{
		"package main\n\nfunc main() {}\n",
		"中文注释\nfunc 中文() {}\n",
		"emoji 😀 in a string\nnext line\n",
		"tabs\there\nand\tmore\n",
		"a\r\nb\r\n",
		"single line no newline",
	}
	for _, content := range contents {
		lines := splitLines([]byte(content))
		for line := 1; line <= len(lines); line++ {
			runeCount := len([]rune(string(lines[line-1])))
			for col := 1; col <= runeCount+1; col++ {
				pos, err := ToLSP([]byte(content), LineCol{line, col})
				if err != nil {
					t.Fatalf("ToLSP(%q, %d:%d): %v", content, line, col, err)
				}
				back, err := FromLSP([]byte(content), pos)
				if err != nil {
					t.Fatalf("FromLSP(%q, %+v): %v", content, pos, err)
				}
				if back != (LineCol{line, col}) {
					t.Errorf("round trip of %d:%d through %+v gave %d:%d (content %q)",
						line, col, pos, back.Line, back.Column, content)
				}
			}
		}
	}
}

// TestSnippet: a tool result shows the line it is talking about, so a wrong
// line number is visible to the model instead of silently plausible.
func TestSnippet(t *testing.T) {
	content := []byte("first\n  second line  \nthird\n")
	if got := snippet(content, LineCol{2, 3}); got != "second line" {
		t.Errorf("snippet = %q, want the trimmed second line", got)
	}
	if got := snippet(content, LineCol{99, 1}); got != "" {
		t.Errorf("snippet out of range = %q, want empty", got)
	}
}

// TestPathURIRoundTrip: the URI is how a server names a file, and a mangled one
// means an answer about a document that does not exist.
func TestPathURI(t *testing.T) {
	for _, p := range []string{"/tmp/a.go", "/tmp/with space/b.go", "/tmp/中文/c.go"} {
		uri := PathToURI(p)
		got, err := URIToPath(uri)
		if err != nil {
			t.Fatalf("URIToPath(%q): %v", uri, err)
		}
		if got != p {
			t.Errorf("round trip of %q gave %q (uri %q)", p, got, uri)
		}
	}
	if _, err := URIToPath("http://example.com/x.go"); err == nil {
		t.Error("a non-file URI must be refused")
	}
}

// TestLanguageIDFor: the label a server receives decides whether it treats the
// document as source at all.
func TestLanguageIDFor(t *testing.T) {
	cases := map[string]string{
		"/a/b.go":  "go",
		"/a/b.GO":  "go",
		"/a/b.py":  "python",
		"/a/b.tsx": "typescriptreact",
		"/a/b.zzz": "plaintext",
		"/a/noext": "plaintext",
	}
	for path, want := range cases {
		if got := LanguageIDFor(path); got != want {
			t.Errorf("LanguageIDFor(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestSortDiagnostics: stable output is what makes a tool result readable and
// testable. Errors come first, and position breaks ties.
func TestSortDiagnostics(t *testing.T) {
	items := []Diagnostic{
		{Severity: SeverityWarning, Range: Range{Start: Position{Line: 1}}},
		{Severity: SeverityError, Range: Range{Start: Position{Line: 9}}},
		{Severity: SeverityError, Range: Range{Start: Position{Line: 2}}},
		{Severity: 0, Range: Range{Start: Position{Line: 0}}},
		{Severity: SeverityInformation, Range: Range{Start: Position{Line: 0}}},
	}
	SortDiagnostics(items)

	wantOrder := []int{0, 2, 9, 1, 0} // unknown(0) , 2, 9, warning@1, info@0
	gotSeverities := []int{}
	gotLines := []int{}
	for _, d := range items {
		gotSeverities = append(gotSeverities, d.Severity)
		gotLines = append(gotLines, d.Range.Start.Line)
	}
	if gotLines[0] != 0 || gotSeverities[0] != 0 {
		t.Errorf("an unlabelled severity should sort with errors, got severity %d at line %d", gotSeverities[0], gotLines[0])
	}
	if gotLines[1] != 2 || gotLines[2] != 9 {
		t.Errorf("errors are not in line order: %v", gotLines)
	}
	if gotSeverities[len(gotSeverities)-1] != SeverityInformation {
		t.Errorf("severity order wrong: %v (want %v)", gotSeverities, wantOrder)
	}
}
