package feishu

import (
	"strings"
	"testing"
)

// tableLines builds a markdown table with the given column count and rows.
func tableLines(cols, rows int) string {
	var b strings.Builder
	b.WriteString("|")
	for c := 0; c < cols; c++ {
		b.WriteString(" h" + itoa(c) + " |")
	}
	b.WriteString("\n|")
	for c := 0; c < cols; c++ {
		b.WriteString(" --- |")
	}
	for r := 0; r < rows; r++ {
		b.WriteString("\n|")
		for c := 0; c < cols; c++ {
			b.WriteString(" r" + itoa(r) + "c" + itoa(c) + " |")
		}
	}
	return b.String()
}

func TestMarkdownToCardElements_NativeTable(t *testing.T) {
	md := "先看数据：\n\n" +
		"| 名字 | 年龄 |\n| --- | --- |\n| 张三 | 30 |\n| 李四 | 25 |"

	els := MarkdownToCardElements(md)

	var tables []CardElement
	for _, el := range els {
		if el.Tag == "table" {
			tables = append(tables, el)
		}
	}
	if len(tables) != 1 {
		t.Fatalf("found %d table elements, want 1 (elements: %+v)", len(tables), els)
	}
	tbl := tables[0]
	if len(tbl.Columns) != 2 {
		t.Errorf("columns = %d, want 2", len(tbl.Columns))
	}
	if len(tbl.Rows) != 2 {
		t.Errorf("rows = %d, want 2", len(tbl.Rows))
	}
	// the surrounding prose must survive as a div
	foundProse := false
	for _, el := range els {
		if el.Tag == "div" && el.Text != nil && strings.Contains(el.Text.Content, "先看数据") {
			foundProse = true
		}
	}
	if !foundProse {
		t.Errorf("prose around the table was lost: %+v", els)
	}
}

func TestMarkdownToCardElements_TableFallbackWhenTooWide(t *testing.T) {
	// A table far wider than the limit cannot be a single native table in one
	// card, so it degrades to markdown text rather than being dropped.
	md := tableLines(12, 1)
	els := MarkdownToCardElements(md)

	for _, el := range els {
		if el.Tag == "table" {
			t.Fatal("an over-wide table should not become a native table in a single card")
		}
	}
	joined := renderAll(els)
	if !strings.Contains(joined, "h11") {
		t.Errorf("table data was lost in the fallback: %q", joined)
	}
}

func TestMarkdownToCardElements_CodeBlockPipeIsNotATable(t *testing.T) {
	md := "```\n| a | b |\n| --- | --- |\n| 1 | 2 |\n```"
	els := MarkdownToCardElements(md)

	for _, el := range els {
		if el.Tag == "table" {
			t.Fatal("a table inside a code fence must stay code")
		}
	}
	if !strings.Contains(renderAll(els), "| --- |") {
		t.Error("code block content was altered")
	}
}

func TestMarkdownToCardMessages_SingleMessageWhenSmall(t *testing.T) {
	md := "结论：\n\n| a | b |\n| --- | --- |\n| 1 | 2 |"
	msgs := MarkdownToCardMessages(md, "标题", DefaultTableLimits())
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	if msgs[0].Title != "标题" {
		t.Errorf("title = %q, want 标题", msgs[0].Title)
	}
	if msgs[0].Continued {
		t.Error("the first message must not be marked as continued")
	}
}

func TestMarkdownToCardMessages_SplitsLongTable(t *testing.T) {
	limits := TableLimits{MaxColumns: 6, MaxRows: 5, MaxCellRunes: 120, MaxBytes: 20000}
	md := "长时间序列：\n\n" + tableLines(3, 12)

	msgs := MarkdownToCardMessages(md, "T", limits)
	if len(msgs) < 2 {
		t.Fatalf("messages = %d, want the table split across several messages", len(msgs))
	}
	if msgs[0].Continued {
		t.Error("first message should not be a continuation")
	}
	for i := 1; i < len(msgs); i++ {
		if !msgs[i].Continued {
			t.Errorf("message %d should be marked continued", i)
		}
	}

	// No row may be lost: every rNc0 value must appear exactly once.
	seen := map[string]int{}
	for _, m := range msgs {
		for _, el := range m.Elements {
			if el.Tag != "table" {
				continue
			}
			if len(el.Rows) > limits.MaxRows {
				t.Errorf("a message carries %d rows, over the %d limit", len(el.Rows), limits.MaxRows)
			}
			for _, row := range el.Rows {
				seen[row["col_0"]]++
			}
		}
	}
	for r := 0; r < 12; r++ {
		key := "r" + itoa(r) + "c0"
		if seen[key] != 1 {
			t.Errorf("row %q appears %d times, want exactly 1", key, seen[key])
		}
	}
}

func TestMarkdownToCardMessages_SplitsWideTable(t *testing.T) {
	limits := TableLimits{MaxColumns: 3, MaxRows: 50, MaxCellRunes: 120, MaxBytes: 20000}
	md := tableLines(8, 1)

	msgs := MarkdownToCardMessages(md, "T", limits)
	if len(msgs) < 2 {
		t.Fatalf("messages = %d, want the wide table split", len(msgs))
	}
	for i, m := range msgs {
		for _, el := range m.Elements {
			if el.Tag == "table" && len(el.Columns) > limits.MaxColumns {
				t.Errorf("message %d has %d columns, over the limit", i, len(el.Columns))
			}
		}
	}
	// every column's data must appear somewhere
	all := ""
	for _, m := range msgs {
		all += renderAll(m.Elements)
	}
	for c := 0; c < 8; c++ {
		want := "h" + itoa(c)
		if !strings.Contains(all, want) {
			t.Errorf("column header %q is missing from the output", want)
		}
	}
}

func TestMarkdownToCardMessages_UnrenderableTableFallsBackToText(t *testing.T) {
	// A single cell longer than MaxCellRunes cannot be fixed by splitting.
	limits := TableLimits{MaxColumns: 6, MaxRows: 20, MaxCellRunes: 10, MaxBytes: 20000}
	long := strings.Repeat("字", 50)
	md := "| a | b |\n| --- | --- |\n| " + long + " | x |"

	msgs := MarkdownToCardMessages(md, "T", limits)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1 (nothing to split)", len(msgs))
	}
	for _, el := range msgs[0].Elements {
		if el.Tag == "table" {
			t.Error("an unrenderable table must not be emitted as a native table")
		}
	}
	if !strings.Contains(renderAll(msgs[0].Elements), long[:9]) {
		t.Error("the cell content was lost in the fallback")
	}
}

func TestMarkdownToCardMessages_KeepsProseWithFirstMessage(t *testing.T) {
	limits := TableLimits{MaxColumns: 6, MaxRows: 3, MaxCellRunes: 120, MaxBytes: 20000}
	md := "说明文字在此。\n\n" + tableLines(2, 8) + "\n\n结尾段落。"

	msgs := MarkdownToCardMessages(md, "T", limits)
	if len(msgs) < 2 {
		t.Fatalf("messages = %d, want a split", len(msgs))
	}
	first := renderAll(msgs[0].Elements)
	if !strings.Contains(first, "说明文字在此") {
		t.Errorf("prose before the table should stay in the first message: %q", first)
	}
	// The trailing prose follows the table, so it may land in a later message;
	// what matters is that it is not lost.
	all := ""
	for _, m := range msgs {
		all += renderAll(m.Elements)
	}
	if !strings.Contains(all, "结尾段落") {
		t.Error("prose after the table was lost")
	}
}

func TestMarkdownToCardMessages_Empty(t *testing.T) {
	msgs := MarkdownToCardMessages("   ", "T", DefaultTableLimits())
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	if len(msgs[0].Elements) == 0 {
		t.Error("an empty answer must still yield one renderable element")
	}
}

func TestCardElement_JSONShapeForTable(t *testing.T) {
	md := "| a | b |\n| --- | --- |\n| 1 | 2 |"
	msgs := MarkdownToCardMessages(md, "T", DefaultTableLimits())

	var tableEl *CardElement
	for i := range msgs[0].Elements {
		if msgs[0].Elements[i].Tag == "table" {
			tableEl = &msgs[0].Elements[i]
		}
	}
	if tableEl == nil {
		t.Fatal("no table element produced")
	}
	if tableEl.PageSize <= 0 {
		t.Errorf("PageSize = %d, want a positive page size", tableEl.PageSize)
	}
	if tableEl.Columns[0].DataType != "lark_md" {
		t.Errorf("DataType = %q, want lark_md", tableEl.Columns[0].DataType)
	}
	if tableEl.Columns[0].VerticalAlign != "top" {
		t.Errorf("VerticalAlign = %q, want top", tableEl.Columns[0].VerticalAlign)
	}
}

// renderAll flattens card elements into a searchable string.
func renderAll(els []CardElement) string {
	var b strings.Builder
	for _, el := range els {
		if el.Text != nil {
			b.WriteString(el.Text.Content)
			b.WriteString("\n")
		}
		for _, c := range el.Columns {
			b.WriteString(c.DisplayName)
			b.WriteString(" ")
		}
		for _, row := range el.Rows {
			for _, v := range row {
				b.WriteString(v)
				b.WriteString(" ")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
