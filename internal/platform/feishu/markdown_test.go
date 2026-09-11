package feishu

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// mdDiv / mdHr build expected elements compactly.
func mdDiv(content string) CardElement {
	return CardElement{Tag: "div", Text: &CardText{Tag: "lark_md", Content: content}}
}

func mdHr() CardElement { return CardElement{Tag: "hr"} }

// mdBigText builds a single markdown paragraph of n lines, each padded so that
// the total comfortably exceeds MaxCardContentBytes.
func mdBigText(n, pad int) string {
	lines := make([]string, 0, n)
	for i := 0; i < n; i++ {
		lines = append(lines, fmt.Sprintf("line %04d %s", i, strings.Repeat("x", pad)))
	}
	return strings.Join(lines, "\n")
}

// mdDivContents returns the content of every div element, in order.
func mdDivContents(els []CardElement) []string {
	out := make([]string, 0, len(els))
	for _, el := range els {
		if el.Tag == "div" && el.Text != nil {
			out = append(out, el.Text.Content)
		}
	}
	return out
}

func TestSplitMarkdownBlocks(t *testing.T) {
	tests := []struct {
		name string
		md   string
		want []MarkdownBlock
	}{
		{
			name: "plain paragraph",
			md:   "hello world",
			want: []MarkdownBlock{{Kind: "text", Content: "hello world"}},
		},
		{
			name: "multiple paragraphs separated by blank lines",
			md:   "first\n\nsecond\n\n\nthird",
			want: []MarkdownBlock{
				{Kind: "text", Content: "first"},
				{Kind: "text", Content: "second"},
				{Kind: "text", Content: "third"},
			},
		},
		{
			name: "multiline paragraph stays whole",
			md:   "line one\nline two\n- bullet\n\npara two",
			want: []MarkdownBlock{
				{Kind: "text", Content: "line one\nline two\n- bullet"},
				{Kind: "text", Content: "para two"},
			},
		},
		{
			name: "code fence with language",
			md:   "intro\n\n```go\nfmt.Println(\"hi\")\n```\n\noutro",
			want: []MarkdownBlock{
				{Kind: "text", Content: "intro"},
				{Kind: "code", Content: "```go\nfmt.Println(\"hi\")\n```", Language: "go"},
				{Kind: "text", Content: "outro"},
			},
		},
		{
			name: "code fence without language",
			md:   "```\nplain code\n```",
			want: []MarkdownBlock{{Kind: "code", Content: "```\nplain code\n```"}},
		},
		{
			name: "blank lines inside fence do not split",
			md:   "```\na\n\nb\n```",
			want: []MarkdownBlock{{Kind: "code", Content: "```\na\n\nb\n```"}},
		},
		{
			name: "unterminated fence runs to EOF",
			md:   "text\n\n```go\nunclosed := true\nstill code",
			want: []MarkdownBlock{
				{Kind: "text", Content: "text"},
				{Kind: "code", Content: "```go\nunclosed := true\nstill code", Language: "go"},
			},
		},
		{
			name: "tilde fences",
			md:   "~~~python\nprint(1)\n~~~",
			want: []MarkdownBlock{{Kind: "code", Content: "~~~python\nprint(1)\n~~~", Language: "python"}},
		},
		{
			name: "nested fences of different characters",
			md:   "~~~~\n```go\ninner := 1\n```\n~~~~",
			want: []MarkdownBlock{{Kind: "code", Content: "~~~~\n```go\ninner := 1\n```\n~~~~"}},
		},
		{
			name: "shorter fence does not close a longer one",
			md:   "````\n```\ncode\n````",
			want: []MarkdownBlock{{Kind: "code", Content: "````\n```\ncode\n````"}},
		},
		{
			name: "adjacent fences",
			md:   "```\na\n```\n```b\nb\n```",
			want: []MarkdownBlock{
				{Kind: "code", Content: "```\na\n```"},
				{Kind: "code", Content: "```b\nb\n```", Language: "b"},
			},
		},
		{
			name: "info string with extra words",
			md:   "```go title=demo.go\nx := 1\n```",
			want: []MarkdownBlock{{Kind: "code", Content: "```go title=demo.go\nx := 1\n```", Language: "go title=demo.go"}},
		},
		{
			name: "indented fence is accepted",
			md:   "    ```go\n    x := 1\n    ```",
			want: []MarkdownBlock{{Kind: "code", Content: "    ```go\n    x := 1\n    ```", Language: "go"}},
		},
		{
			name: "crlf line endings are normalized",
			md:   "\r\n\r\nparagraph one\r\n\r\n```sh\r\nls -la\r\n```\r\n\r\n",
			want: []MarkdownBlock{
				{Kind: "text", Content: "paragraph one"},
				{Kind: "code", Content: "```sh\nls -la\n```", Language: "sh"},
			},
		},
		{
			name: "hash line outside code stays text",
			md:   "# Title\nbody",
			want: []MarkdownBlock{{Kind: "text", Content: "# Title\nbody"}},
		},
		{
			name: "two backticks are not a fence",
			md:   "``not a fence``",
			want: []MarkdownBlock{{Kind: "text", Content: "``not a fence``"}},
		},
		{
			name: "empty input",
			md:   "",
			want: []MarkdownBlock{},
		},
		{
			name: "whitespace only input",
			md:   "   \n\t\n  \n",
			want: []MarkdownBlock{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitMarkdownBlocks(tc.md)
			if got == nil {
				t.Fatal("SplitMarkdownBlocks returned a nil slice")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("blocks = %#v, want %#v", got, tc.want)
			}
			// Invariants: a code block always carries a well-formed opening fence
			// whose info string is reported as Language.
			for i, b := range got {
				if b.Kind != "code" {
					continue
				}
				first := strings.Split(b.Content, "\n")[0]
				_, info, ok := fenceRun(first)
				if !ok {
					t.Fatalf("block %d: code content does not start with a fence: %q", i, b.Content)
				}
				if info != b.Language {
					t.Errorf("block %d: Language = %q, want %q", i, b.Language, info)
				}
			}
		})
	}
}

func TestMarkdownToCardElements(t *testing.T) {
	tests := []struct {
		name string
		md   string
		want []CardElement
	}{
		{
			name: "single paragraph",
			md:   "hello world",
			want: []CardElement{mdDiv("hello world")},
		},
		{
			name: "two paragraphs get an hr between them",
			md:   "first\n\nsecond",
			want: []CardElement{mdDiv("first"), mdHr(), mdDiv("second")},
		},
		{
			name: "three paragraphs get two hrs",
			md:   "a\n\nb\n\nc",
			want: []CardElement{mdDiv("a"), mdHr(), mdDiv("b"), mdHr(), mdDiv("c")},
		},
		{
			name: "trailing blank lines do not add blocks",
			md:   "only\n\n\n",
			want: []CardElement{mdDiv("only")},
		},
		{
			name: "code block keeps its fences verbatim",
			md:   "```go\nfunc main() {}\n```",
			want: []CardElement{mdDiv("```go\nfunc main() {}\n```")},
		},
		{
			name: "code block without language",
			md:   "```\nraw text\n```",
			want: []CardElement{mdDiv("```\nraw text\n```")},
		},
		{
			name: "paragraph and code block",
			md:   "看下这个函数：\n\n```go\nfmt.Println(\"hi\")\n```",
			want: []CardElement{
				mdDiv("看下这个函数："),
				mdHr(),
				mdDiv("```go\nfmt.Println(\"hi\")\n```"),
			},
		},
		{
			name: "blank line inside fence does not split the element",
			md:   "```\na\n\nb\n```",
			want: []CardElement{mdDiv("```\na\n\nb\n```")},
		},
		{
			name: "unterminated fence renders as one code element",
			md:   "```go\nx := 1",
			want: []CardElement{mdDiv("```go\nx := 1")},
		},
		{
			name: "heading and list stay in one text element",
			md:   "# 标题\n\n- one\n- two",
			want: []CardElement{mdDiv("# 标题"), mdHr(), mdDiv("- one\n- two")},
		},
		{
			name: "empty input yields one empty div",
			md:   "",
			want: []CardElement{mdDiv("")},
		},
		{
			name: "whitespace only input yields one empty div",
			md:   "  \n\t\n   ",
			want: []CardElement{mdDiv("")},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MarkdownToCardElements(tc.md)
			if got == nil {
				t.Fatal("MarkdownToCardElements returned a nil slice")
			}
			if len(got) == 0 {
				t.Fatal("MarkdownToCardElements returned an empty slice")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("elements = %#v, want %#v", got, tc.want)
			}
			mdAssertShape(t, got)
		})
	}
}

// mdAssertShape checks the structural invariants every result must satisfy:
// divs and hrs only, lark_md text on every div, no leading/trailing/doubled hr.
func mdAssertShape(t *testing.T, els []CardElement) {
	t.Helper()
	for i, el := range els {
		switch el.Tag {
		case "div":
			if el.Text == nil {
				t.Errorf("element %d: div has nil text", i)
				continue
			}
			if el.Text.Tag != "lark_md" {
				t.Errorf("element %d: text tag = %q, want lark_md", i, el.Text.Tag)
			}
		case "hr":
			if el.Text != nil {
				t.Errorf("element %d: hr should not carry text", i)
			}
			if i == 0 || i == len(els)-1 {
				t.Errorf("element %d: hr must not be first or last", i)
			}
			if els[i-1].Tag == "hr" {
				t.Errorf("element %d: consecutive hr elements", i)
			}
		default:
			t.Errorf("element %d: unexpected tag %q", i, el.Tag)
		}
	}
}

func TestMarkdownToCardElementsOversizedText(t *testing.T) {
	text := mdBigText(800, 40)
	if len(text) <= MaxCardContentBytes {
		t.Fatalf("test fixture too small: %d bytes", len(text))
	}

	els := MarkdownToCardElements(text)
	if len(els) < 2 {
		t.Fatalf("expected the oversized paragraph to be split, got %d element(s)", len(els))
	}
	mdAssertShape(t, els)
	mdAssertWithinBudget(t, els)

	// Line-boundary splitting is lossless: joining the parts rebuilds the text.
	if got := strings.Join(mdDivContents(els), "\n"); got != text {
		t.Errorf("reassembled content differs from the input (%d vs %d bytes)", len(got), len(text))
	}
	// No element may end mid-line: every part is a whole number of lines.
	for i, part := range mdDivContents(els) {
		if strings.HasSuffix(part, "\n") || strings.HasPrefix(part, "\n") {
			t.Errorf("part %d has a stray newline edge: %q", i, part[:min(len(part), 20)])
		}
	}
}

func TestMarkdownToCardElementsOversizedCodeBlock(t *testing.T) {
	body := mdBigText(800, 40)
	md := "```go\n" + body + "\n```"
	if len(md) <= MaxCardContentBytes {
		t.Fatalf("test fixture too small: %d bytes", len(md))
	}

	els := MarkdownToCardElements(md)
	if len(els) < 2 {
		t.Fatalf("expected the oversized code block to be split, got %d element(s)", len(els))
	}
	mdAssertShape(t, els)
	mdAssertWithinBudget(t, els)

	rebuilt := make([]string, 0, len(els))
	for i, part := range mdDivContents(els) {
		if !strings.HasPrefix(part, "```go\n") {
			t.Fatalf("part %d does not re-open the fence: %q", i, part[:min(len(part), 20)])
		}
		if !strings.HasSuffix(part, "\n```") {
			t.Fatalf("part %d does not re-close the fence: %q", i, part[len(part)-20:])
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(part, "```go\n"), "\n```")
		if _, _, ok := fenceRun(strings.Split(part, "\n")[0]); !ok {
			t.Fatalf("part %d opening fence is malformed", i)
		}
		rebuilt = append(rebuilt, inner)
	}
	if got := strings.Join(rebuilt, "\n"); got != body {
		t.Errorf("reassembled code differs from the original body (%d vs %d bytes)", len(got), len(body))
	}
}

func TestMarkdownToCardElementsOversizedMixedDocument(t *testing.T) {
	text := mdBigText(800, 40)
	md := text + "\n\n```go\n" + mdBigText(800, 40) + "\n```\n\ntail"

	els := MarkdownToCardElements(md)
	mdAssertShape(t, els)
	mdAssertWithinBudget(t, els)

	hrs := 0
	for _, el := range els {
		if el.Tag == "hr" {
			hrs++
		}
	}
	if hrs != 2 {
		t.Errorf("hr count = %d, want 2 (one between each of the three blocks)", hrs)
	}
	contents := mdDivContents(els)
	if last := contents[len(contents)-1]; last != "tail" {
		t.Errorf("last div = %q, want tail", last)
	}
	if els[0].Tag != "div" {
		t.Errorf("first element tag = %q, want div", els[0].Tag)
	}
}

func TestMarkdownToCardElementsHugeSingleLine(t *testing.T) {
	// A single line of 3-byte runes far larger than the budget: it cannot be
	// split on a line boundary, so it must be broken on rune boundaries only.
	line := strings.Repeat("中", 20000)
	if len(line) != 60000 {
		t.Fatalf("fixture = %d bytes, want 60000", len(line))
	}

	els := MarkdownToCardElements(line)
	if len(els) < 2 {
		t.Fatalf("expected the line to be split, got %d element(s)", len(els))
	}
	mdAssertWithinBudget(t, els)

	var rebuilt strings.Builder
	for i, part := range mdDivContents(els) {
		if !utf8.ValidString(part) {
			t.Errorf("part %d is not valid UTF-8", i)
		}
		if len(part)%3 != 0 {
			t.Errorf("part %d = %d bytes, not a whole number of 3-byte runes", i, len(part))
		}
		rebuilt.WriteString(part)
	}
	if rebuilt.String() != line {
		t.Error("concatenated parts do not reproduce the original line")
	}
}

// mdAssertWithinBudget fails when an element carries more than the budget.
func mdAssertWithinBudget(t *testing.T, els []CardElement) {
	t.Helper()
	for i, el := range els {
		if el.Text == nil {
			continue
		}
		if n := len(el.Text.Content); n > MaxCardContentBytes {
			t.Errorf("element %d content = %d bytes, want <= %d", i, n, MaxCardContentBytes)
		}
	}
}

func TestCardTitleFromMarkdown(t *testing.T) {
	tests := []struct {
		name   string
		md     string
		maxLen int
		want   string
	}{
		{name: "plain paragraph", md: "hello world", maxLen: 40, want: "hello world"},
		{name: "chinese paragraph", md: "帮我看看这段代码", maxLen: 40, want: "帮我看看这段代码"},
		{name: "h1", md: "# Title\nbody", maxLen: 40, want: "Title"},
		{name: "h2", md: "## Title", maxLen: 40, want: "Title"},
		{name: "h3", md: "### Title", maxLen: 40, want: "Title"},
		{name: "h4", md: "#### Title", maxLen: 40, want: "Title"},
		{name: "h5", md: "##### Title", maxLen: 40, want: "Title"},
		{name: "h6", md: "###### Title", maxLen: 40, want: "Title"},
		{name: "heading wins over earlier paragraph", md: "intro text\n\n# Real Title\nmore", maxLen: 40, want: "Real Title"},
		{name: "heading after leading blank lines", md: "\n\n   \n# Spaced\n", maxLen: 40, want: "Spaced"},
		{name: "closing hashes are stripped", md: "## Title ##", maxLen: 40, want: "Title"},
		{name: "bold noise is stripped", md: "## **Bold Title**", maxLen: 40, want: "Bold Title"},
		{name: "italic noise is stripped", md: "# _Italic_", maxLen: 40, want: "Italic"},
		{name: "code noise is stripped", md: "# `config.yaml`", maxLen: 40, want: "config.yaml"},
		{name: "noise on plain first line", md: "**Bold first line**\n\nbody", maxLen: 40, want: "Bold first line"},
		{name: "seven hashes are not a heading", md: "####### seven", maxLen: 40, want: "####### seven"},
		{name: "hash without space is not a heading", md: "#hashtag", maxLen: 40, want: "#hashtag"},
		{name: "heading inside a fence is ignored", md: "```go\n# not a title\n```\n\n# Real Title", maxLen: 40, want: "Real Title"},
		{name: "unterminated fence heading is ignored", md: "```go\n# not a title", maxLen: 40, want: "# not a title"},
		{name: "code only document uses the first code line", md: "```go\nx := 1\n```", maxLen: 40, want: "x := 1"},
		{name: "empty fences give the default", md: "```\n```", maxLen: 40, want: defaultCardTitle},
		{name: "empty input gives the default", md: "", maxLen: 40, want: defaultCardTitle},
		{name: "whitespace input gives the default", md: "   \n\t\n", maxLen: 40, want: defaultCardTitle},
		{name: "empty heading gives the default", md: "#\n", maxLen: 40, want: defaultCardTitle},
		{name: "exactly maxLen is not truncated", md: "# abcdefghij", maxLen: 10, want: "abcdefghij"},
		{name: "ascii truncation adds an ellipsis", md: "# abcdefghijklmno", maxLen: 10, want: "abcdefghij…"},
		{name: "single rune limit", md: "# abcdef", maxLen: 1, want: "a…"},
		{name: "negative maxLen falls back to the default", md: "# " + strings.Repeat("a", 50), maxLen: -3, want: strings.Repeat("a", 40) + "…"},
		{name: "zero maxLen falls back to the default", md: "# " + strings.Repeat("a", 50), maxLen: 0, want: strings.Repeat("a", 40) + "…"},
		{
			name:   "chinese truncation is rune safe",
			md:     "# " + strings.Repeat("中", 30),
			maxLen: 10,
			want:   strings.Repeat("中", 10) + "…",
		},
		{
			name:   "emoji truncation is rune safe",
			md:     "# " + strings.Repeat("🙂", 6),
			maxLen: 3,
			want:   strings.Repeat("🙂", 3) + "…",
		},
		{
			name:   "mixed ascii and chinese truncation",
			md:     "标题 " + strings.Repeat("内容", 30),
			maxLen: 5,
			want:   "标题 内容…",
		},
		{
			name:   "no ellipsis when the title fits",
			md:     "# " + strings.Repeat("中", 5),
			maxLen: 5,
			want:   strings.Repeat("中", 5),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CardTitleFromMarkdown(tc.md, tc.maxLen)
			if got != tc.want {
				t.Fatalf("title = %q, want %q", got, tc.want)
			}
			if got == "" {
				t.Fatal("title must never be empty")
			}
			if !utf8.ValidString(got) {
				t.Fatalf("title %q is not valid UTF-8", got)
			}
			if n := utf8.RuneCountInString(got); tc.maxLen > 0 && n > tc.maxLen+1 {
				t.Errorf("title is %d runes, want <= %d", n, tc.maxLen+1)
			}
		})
	}
}

func TestCardTitleFromMarkdownRuneBoundaries(t *testing.T) {
	// Every truncation point of a multi-byte title must produce valid UTF-8.
	const repeated = "中文标题测试"
	title := strings.Repeat(repeated, 20)
	runes := []rune(title)
	for maxLen := 1; maxLen <= 20; maxLen++ {
		got := CardTitleFromMarkdown("# "+title, maxLen)
		if !utf8.ValidString(got) {
			t.Fatalf("maxLen %d: title %q is not valid UTF-8", maxLen, got)
		}
		want := string(runes[:maxLen]) + "…"
		if got != want {
			t.Fatalf("maxLen %d: title = %q, want %q", maxLen, got, want)
		}
	}
}

func TestSplitLongLine(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		maxBytes int
		want     int // number of pieces
	}{
		{name: "short line is untouched", in: "abc", maxBytes: 10, want: 1},
		{name: "exact fit", in: "abcdefghij", maxBytes: 10, want: 1},
		{name: "two ascii pieces", in: "abcdefghijklmno", maxBytes: 10, want: 2},
		{name: "empty line", in: "", maxBytes: 10, want: 1},
		{name: "tiny budget is raised to one rune", in: "中文", maxBytes: 1, want: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitLongLine(tc.in, tc.maxBytes)
			if len(got) != tc.want {
				t.Fatalf("pieces = %d, want %d (%q)", len(got), tc.want, got)
			}
			if strings.Join(got, "") != tc.in {
				t.Errorf("pieces do not rebuild the input: %q", got)
			}
			for _, p := range got {
				if !utf8.ValidString(p) {
					t.Errorf("piece %q is not valid UTF-8", p)
				}
			}
		})
	}
}

func TestSplitContentByLines(t *testing.T) {
	text := mdBigText(800, 40)
	parts := splitContentByLines(text, MaxCardContentBytes)
	if len(parts) < 2 {
		t.Fatalf("parts = %d, want >= 2", len(parts))
	}
	for i, p := range parts {
		if len(p) > MaxCardContentBytes {
			t.Errorf("part %d = %d bytes, want <= %d", i, len(p), MaxCardContentBytes)
		}
	}
	if got := strings.Join(parts, "\n"); got != text {
		t.Errorf("reassembled %d bytes, want %d", len(got), len(text))
	}
	// A non-positive budget must not panic or lose content.
	if got := splitContentByLines("hello", 0); len(got) != 1 || got[0] != "hello" {
		t.Errorf("splitContentByLines with budget 0 = %q", got)
	}
	if got := splitContentByLines("hello", 100); len(got) != 1 || got[0] != "hello" {
		t.Errorf("splitContentByLines with a large budget = %q", got)
	}
}

func TestSplitCodeBlockFallbacks(t *testing.T) {
	t.Run("content that is not fenced is split as plain text", func(t *testing.T) {
		b := MarkdownBlock{Kind: "code", Content: mdBigText(800, 40)}
		parts := splitCodeBlock(b)
		if len(parts) < 2 {
			t.Fatalf("parts = %d, want >= 2", len(parts))
		}
		for i, p := range parts {
			if len(p) > MaxCardContentBytes {
				t.Errorf("part %d = %d bytes, want <= %d", i, len(p), MaxCardContentBytes)
			}
		}
		if got := strings.Join(parts, "\n"); got != b.Content {
			t.Errorf("reassembled %d bytes, want %d", len(got), len(b.Content))
		}
	})

	t.Run("a fence that alone exceeds the budget falls back", func(t *testing.T) {
		b := MarkdownBlock{Kind: "code", Content: "```" + strings.Repeat("x", MaxCardContentBytes+100)}
		parts := splitCodeBlock(b)
		if len(parts) < 2 {
			t.Fatalf("parts = %d, want >= 2", len(parts))
		}
		for i, p := range parts {
			if len(p) > MaxCardContentBytes {
				t.Errorf("part %d = %d bytes, want <= %d", i, len(p), MaxCardContentBytes)
			}
		}
	})
}

func TestCardElementJSONShapes(t *testing.T) {
	tests := []struct {
		name string
		el   CardElement
		want string
	}{
		{
			name: "div with lark_md",
			el:   CardElement{Tag: "div", Text: &CardText{Tag: "lark_md", Content: "**hi**"}},
			want: `{"tag":"div","text":{"tag":"lark_md","content":"**hi**"}}`,
		},
		{
			name: "hr has no text field",
			el:   CardElement{Tag: "hr"},
			want: `{"tag":"hr"}`,
		},
		{
			name: "note footnote",
			el: CardElement{
				Tag:      "note",
				Elements: []CardText{{Tag: "plain_text", Content: "Generated by huan-agent"}},
			},
			want: `{"tag":"note","elements":[{"tag":"plain_text","content":"Generated by huan-agent"}]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.el)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(b) != tc.want {
				t.Fatalf("json = %s, want %s", b, tc.want)
			}
		})
	}
}

func TestMarkdownToCardElementsMarshals(t *testing.T) {
	els := MarkdownToCardElements("# 标题\n\n正文\n\n```go\nx := 1\n```")
	b, err := json.Marshal(els)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(decoded) != len(els) {
		t.Fatalf("decoded %d elements, want %d", len(decoded), len(els))
	}
	if !strings.Contains(string(b), "```go") {
		t.Errorf("fences were not preserved: %s", b)
	}
	if len(b) > MaxCardContentBytes {
		t.Errorf("card body = %d bytes, want <= %d", len(b), MaxCardContentBytes)
	}
}

func TestFenceHelpers(t *testing.T) {
	tests := []struct {
		line      string
		wantFence bool
		wantInfo  string
	}{
		{"```go", true, "go"},
		{"``` go ", true, "go"},
		{"~~~", true, ""},
		{"``", false, ""},
		{"`x`", false, ""},
		{"", false, ""},
		{"plain text", false, ""},
		{"```has`tick", false, ""},
	}
	for _, tc := range tests {
		fence, info, ok := fenceRun(tc.line)
		if ok != tc.wantFence {
			t.Errorf("fenceRun(%q) ok = %v, want %v", tc.line, ok, tc.wantFence)
			continue
		}
		if !ok {
			continue
		}
		if info != tc.wantInfo {
			t.Errorf("fenceRun(%q) info = %q, want %q", tc.line, info, tc.wantInfo)
		}
		if !closingFence(fence, fence) {
			t.Errorf("closingFence(%q, %q) = false, want true", fence, fence)
		}
	}
	if closingFence("```", "") {
		t.Error("closingFence with an empty fence must be false")
	}
	if closingFence("``` extra", "```") {
		t.Error("a closing fence must not carry an info string")
	}
}

func TestAtxHeading(t *testing.T) {
	tests := []struct {
		line      string
		wantLevel int
		wantText  string
		wantOK    bool
	}{
		{"# H", 1, "H", true},
		{"  ## H2 ", 2, "H2", true},
		{"###### H6", 6, "H6", true},
		{"####### H7", 0, "", false},
		{"#no-space", 0, "", false},
		{"plain", 0, "", false},
		{"", 0, "", false},
		{"# Trailing ###", 1, "Trailing", true},
	}
	for _, tc := range tests {
		level, text, ok := atxHeading(tc.line)
		if ok != tc.wantOK || level != tc.wantLevel || text != tc.wantText {
			t.Errorf("atxHeading(%q) = (%d, %q, %v), want (%d, %q, %v)",
				tc.line, level, text, ok, tc.wantLevel, tc.wantText, tc.wantOK)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		in     string
		maxLen int
		want   string
	}{
		{"abcdef", 3, "abc…"},
		{"abc", 3, "abc"},
		{"abc", 10, "abc"},
		{"中文标题", 2, "中文…"},
		{"", 5, ""},
		{"abc", 0, "abc"},
	}
	for _, tc := range tests {
		if got := truncateRunes(tc.in, tc.maxLen); got != tc.want {
			t.Errorf("truncateRunes(%q, %d) = %q, want %q", tc.in, tc.maxLen, got, tc.want)
		}
	}
}

func TestGroupLines(t *testing.T) {
	lines := []string{"aa", "bb", "cc"}
	groups := groupLines(lines, 5) // "aa\nbb" fits, adding "cc" would not
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2 (%q)", len(groups), groups)
	}
	if got := strings.Join(groups[0], "\n"); got != "aa\nbb" {
		t.Errorf("first group = %q, want %q", got, "aa\nbb")
	}
	if groups[1][0] != "cc" {
		t.Errorf("second group = %q, want [cc]", groups[1])
	}
	for _, g := range groups {
		if n := len(strings.Join(g, "\n")); n > 5 {
			t.Errorf("group %q = %d bytes, want <= 5", g, n)
		}
	}

	// A non-positive budget must still produce usable groups.
	if got := groupLines([]string{"x"}, 0); len(got) != 1 {
		t.Errorf("groupLines with budget 0 = %q", got)
	}
	if got := groupLines(nil, 10); len(got) != 0 {
		t.Errorf("groupLines(nil) = %q, want no groups", got)
	}
}

func TestNormalizeNewlines(t *testing.T) {
	tests := []struct{ in, want string }{
		{"a\r\nb", "a\nb"},
		{"a\rb", "a\nb"},
		{"a\nb", "a\nb"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := normalizeNewlines(tc.in); got != tc.want {
			t.Errorf("normalizeNewlines(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStripInlineMarkdown(t *testing.T) {
	tests := []struct{ in, want string }{
		{"**bold**", "bold"},
		{"`code`", "code"},
		{"_italic_", "italic"},
		{"  spaced  ", "spaced"},
		{"plain", "plain"},
	}
	for _, tc := range tests {
		if got := stripInlineMarkdown(tc.in); got != tc.want {
			t.Errorf("stripInlineMarkdown(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
