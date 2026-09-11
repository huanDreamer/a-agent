package feishu

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestParseTable_Basic(t *testing.T) {
	lines := []string{
		"| 名字 | 年龄 | 城市 |",
		"| --- | --- | --- |",
		"| 张三 | 30 | 北京 |",
		"| 李四 | 25 | 上海 |",
	}
	tbl := ParseTable(lines)
	if tbl == nil {
		t.Fatal("ParseTable returned nil for a valid table")
	}
	if got := tbl.ColumnCount(); got != 3 {
		t.Errorf("ColumnCount = %d, want 3", got)
	}
	if got := tbl.RowCount(); got != 2 {
		t.Errorf("RowCount = %d, want 2", got)
	}
	if tbl.Headers[0] != "名字" || tbl.Headers[2] != "城市" {
		t.Errorf("Headers = %v", tbl.Headers)
	}
	if tbl.Rows[0][0] != "张三" || tbl.Rows[1][2] != "上海" {
		t.Errorf("Rows = %v", tbl.Rows)
	}
}

func TestParseTable_Alignments(t *testing.T) {
	tbl := ParseTable([]string{
		"| a | b | c | d |",
		"| :-- | :-: | --: | --- |",
		"| 1 | 2 | 3 | 4 |",
	})
	if tbl == nil {
		t.Fatal("nil table")
	}
	want := []string{"left", "center", "right", "left"}
	for i, w := range want {
		if tbl.Aligns[i] != w {
			t.Errorf("align[%d] = %q, want %q", i, tbl.Aligns[i], w)
		}
	}
}

func TestParseTable_NotATable(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
	}{
		{"empty", nil},
		{"single line", []string{"| a | b |"}},
		{"no pipes", []string{"hello", "---"}},
		{"delimiter without dashes", []string{"| a | b |", "| x | y |"}},
		{"delimiter with text", []string{"| a | b |", "| -x- | --- |"}},
		{"blank delimiter cell", []string{"| a | b |", "|  | --- |"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseTable(tc.lines); got != nil {
				t.Errorf("ParseTable = %+v, want nil", got)
			}
		})
	}
}

func TestParseTable_LeadingAndTrailingPipesOptional(t *testing.T) {
	tbl := ParseTable([]string{
		"a | b",
		"--- | ---",
		"1 | 2",
	})
	if tbl == nil {
		t.Fatal("table without outer pipes should parse")
	}
	if tbl.ColumnCount() != 2 || tbl.Headers[0] != "a" || tbl.Rows[0][1] != "2" {
		t.Errorf("unexpected parse: %+v", tbl)
	}
}

func TestParseTable_NormalizesRowWidth(t *testing.T) {
	tbl := ParseTable([]string{
		"| a | b | c |",
		"| --- | --- | --- |",
		"| 1 |",             // short row -> padded
		"| 1 | 2 | 3 | 4 |", // long row -> truncated
	})
	if tbl == nil {
		t.Fatal("nil table")
	}
	for i, r := range tbl.Rows {
		if len(r) != 3 {
			t.Errorf("row %d has %d cells, want 3: %v", i, len(r), r)
		}
	}
	if tbl.Rows[0][1] != "" || tbl.Rows[0][2] != "" {
		t.Errorf("short row not padded: %v", tbl.Rows[0])
	}
}

func TestParseTable_StopsAtNonTableLine(t *testing.T) {
	tbl := ParseTable([]string{
		"| a | b |",
		"| --- | --- |",
		"| 1 | 2 |",
		"trailing prose without pipes",
	})
	if tbl == nil {
		t.Fatal("nil table")
	}
	if tbl.RowCount() != 1 {
		t.Errorf("RowCount = %d, want 1 (prose should end the table)", tbl.RowCount())
	}
}

func TestParseTable_HeaderOnly(t *testing.T) {
	tbl := ParseTable([]string{"| a | b |", "| --- | --- |"})
	if tbl == nil {
		t.Fatal("header-only table should parse")
	}
	if tbl.RowCount() != 0 {
		t.Errorf("RowCount = %d, want 0", tbl.RowCount())
	}
}

func TestIsTableStart(t *testing.T) {
	lines := []string{
		"prose",
		"| a | b |",
		"| --- | --- |",
		"| 1 | 2 |",
	}
	if IsTableStart(lines, 0) {
		t.Error("prose line should not start a table")
	}
	if !IsTableStart(lines, 1) {
		t.Error("line 1 starts a valid table")
	}
	if IsTableStart(lines, 3) {
		t.Error("last line has no delimiter row after it")
	}
	if IsTableStart(lines, 99) {
		t.Error("out-of-range index should be false")
	}
}

func TestNativeTableElement_Shape(t *testing.T) {
	tbl := ParseTable([]string{
		"| 名字 | 年龄 |",
		"| :-- | --: |",
		"| 张三 | 30 |",
	})
	el, err := NativeTableElement(tbl, DefaultTableLimits())
	if err != nil {
		t.Fatalf("NativeTableElement: %v", err)
	}
	if el.Tag != "table" {
		t.Errorf("Tag = %q, want table", el.Tag)
	}
	if len(el.Columns) != 2 {
		t.Fatalf("Columns = %d, want 2", len(el.Columns))
	}
	if el.Columns[0].Name != "col_0" || el.Columns[1].Name != "col_1" {
		t.Errorf("column names = %q/%q, want col_0/col_1", el.Columns[0].Name, el.Columns[1].Name)
	}
	if el.Columns[0].DisplayName != "名字" {
		t.Errorf("DisplayName = %q, want 名字", el.Columns[0].DisplayName)
	}
	if el.Columns[0].HorizontalAlign != "left" || el.Columns[1].HorizontalAlign != "right" {
		t.Errorf("aligns = %q/%q, want left/right", el.Columns[0].HorizontalAlign, el.Columns[1].HorizontalAlign)
	}
	if len(el.Rows) != 1 {
		t.Fatalf("Rows = %d, want 1", len(el.Rows))
	}
	if el.Rows[0]["col_0"] != "张三" || el.Rows[0]["col_1"] != "30" {
		t.Errorf("row = %v", el.Rows[0])
	}
	if el.HeaderStyle == nil || !el.HeaderStyle.Bold {
		t.Error("header style should be bold")
	}
	if el.RowHeight != "low" {
		t.Errorf("RowHeight = %q, want low", el.RowHeight)
	}
}

func TestNativeTableElement_JSONRoundTrip(t *testing.T) {
	tbl := ParseTable([]string{"| a |", "| --- |", "| 1 |"})
	el, err := NativeTableElement(tbl, DefaultTableLimits())
	if err != nil {
		t.Fatalf("NativeTableElement: %v", err)
	}
	b, err := json.Marshal(el)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The platform needs a rows array of objects keyed by column name.
	var decoded struct {
		Tag     string              `json:"tag"`
		Columns []map[string]any    `json:"columns"`
		Rows    []map[string]string `json:"rows"`
	}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Tag != "table" {
		t.Errorf("tag = %q", decoded.Tag)
	}
	if len(decoded.Columns) != 1 {
		t.Fatalf("columns = %d", len(decoded.Columns))
	}
	if _, ok := decoded.Columns[0]["name"]; !ok {
		t.Error("column is missing the name key required by the platform")
	}
	if len(decoded.Rows) != 1 || decoded.Rows[0]["col_0"] != "1" {
		t.Errorf("rows = %v", decoded.Rows)
	}
}

func TestNativeTableElement_TooManyColumns(t *testing.T) {
	headers := "| a | b | c | d | e | f | g | h |"
	sep := "| --- | --- | --- | --- | --- | --- | --- | --- |"
	row := "| 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 |"
	tbl := ParseTable([]string{headers, sep, row})

	_, err := NativeTableElement(tbl, DefaultTableLimits())
	if !errors.Is(err, ErrTableTooComplex) {
		t.Errorf("err = %v, want ErrTableTooComplex", err)
	}
}

func TestNativeTableElement_TooManyRows(t *testing.T) {
	lines := []string{"| a |", "| --- |"}
	for i := 0; i < 30; i++ {
		lines = append(lines, "| x |")
	}
	tbl := ParseTable(lines)

	_, err := NativeTableElement(tbl, DefaultTableLimits())
	if !errors.Is(err, ErrTableTooComplex) {
		t.Errorf("err = %v, want ErrTableTooComplex", err)
	}
}

func TestNativeTableElement_LongCell(t *testing.T) {
	long := strings.Repeat("字", 200)
	tbl := &TableData{Headers: []string{"h"}, Rows: [][]string{{long}}}

	_, err := NativeTableElement(tbl, DefaultTableLimits())
	if !errors.Is(err, ErrTableTooComplex) {
		t.Errorf("err = %v, want ErrTableTooComplex for an over-long cell", err)
	}
}

func TestNativeTableElement_LongHeader(t *testing.T) {
	tbl := &TableData{Headers: []string{strings.Repeat("h", 200)}, Rows: [][]string{{"x"}}}
	if _, err := NativeTableElement(tbl, DefaultTableLimits()); !errors.Is(err, ErrTableTooComplex) {
		t.Errorf("err = %v, want ErrTableTooComplex for an over-long header", err)
	}
}

func TestNativeTableElement_NilAndEmpty(t *testing.T) {
	if _, err := NativeTableElement(nil, DefaultTableLimits()); !errors.Is(err, ErrTableTooComplex) {
		t.Error("nil table should be reported as too complex")
	}
	if _, err := NativeTableElement(&TableData{}, DefaultTableLimits()); !errors.Is(err, ErrTableTooComplex) {
		t.Error("a table with no columns should be reported as too complex")
	}
}

func TestSplitTableForMessages_RowsOnly(t *testing.T) {
	lines := []string{"| n | v |", "| --- | --- |"}
	for i := 0; i < 45; i++ {
		lines = append(lines, "| "+itoa(i)+" | x |")
	}
	tbl := ParseTable(lines)

	limits := TableLimits{MaxRows: 20, MaxColumns: 6, MaxCellRunes: 120, MaxBytes: 20000}
	chunks, err := SplitTableForMessages(tbl, limits)
	if err != nil {
		t.Fatalf("SplitTableForMessages: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3 (20+20+5)", len(chunks))
	}
	total := 0
	for i, c := range chunks {
		total += c.RowCount()
		if c.RowCount() > 20 {
			t.Errorf("chunk %d has %d rows, over the limit", i, c.RowCount())
		}
		if c.ColumnCount() != 2 {
			t.Errorf("chunk %d lost columns: %d", i, c.ColumnCount())
		}
		// every chunk keeps the header
		if c.Headers[0] != "n" {
			t.Errorf("chunk %d lost its header: %v", i, c.Headers)
		}
	}
	if total != 45 {
		t.Errorf("total rows across chunks = %d, want 45", total)
	}
}

func TestSplitTableForMessages_ColumnsOnly(t *testing.T) {
	tbl := &TableData{
		Headers: []string{"id", "c1", "c2", "c3", "c4", "c5", "c6", "c7"},
		Rows:    [][]string{{"k", "1", "2", "3", "4", "5", "6", "7"}},
	}
	limits := TableLimits{MaxColumns: 3, MaxRows: 20, MaxCellRunes: 120, MaxBytes: 20000}
	chunks, err := SplitTableForMessages(tbl, limits)
	if err != nil {
		t.Fatalf("SplitTableForMessages: %v", err)
	}
	if len(chunks) != 4 {
		t.Fatalf("chunks = %d, want 4 column groups", len(chunks))
	}
	for i, c := range chunks {
		if c.ColumnCount() > 3 {
			t.Errorf("chunk %d has %d columns, over the limit", i, c.ColumnCount())
		}
		// the key column is repeated in every group
		if c.Headers[0] != "id" {
			t.Errorf("chunk %d lost the key column: %v", i, c.Headers)
		}
		if len(c.Rows) != 1 || c.Rows[0][0] != "k" {
			t.Errorf("chunk %d lost the key value: %v", i, c.Rows)
		}
	}
	// first group holds the key plus the first two data columns
	if got := chunks[0].Headers; len(got) != 3 || got[1] != "c1" || got[2] != "c2" {
		t.Errorf("first group headers = %v, want [id c1 c2]", got)
	}
	if got := chunks[1].Headers; len(got) != 3 || got[1] != "c3" || got[2] != "c4" {
		t.Errorf("second group headers = %v, want [id c3 c4]", got)
	}
}

func TestSplitTableForMessages_ColumnsAndRows(t *testing.T) {
	tbl := &TableData{Headers: []string{"id", "a", "b", "c", "d"}}
	for i := 0; i < 5; i++ {
		tbl.Rows = append(tbl.Rows, []string{itoa(i), "1", "2", "3", "4"})
	}
	limits := TableLimits{MaxColumns: 3, MaxRows: 2, MaxCellRunes: 120, MaxBytes: 20000}
	chunks, err := SplitTableForMessages(tbl, limits)
	if err != nil {
		t.Fatalf("SplitTableForMessages: %v", err)
	}
	// 2 column groups x ceil(5/2)=3 row chunks = 6
	if len(chunks) != 6 {
		t.Fatalf("chunks = %d, want 6", len(chunks))
	}
	for i, c := range chunks {
		if c.ColumnCount() > 3 || c.RowCount() > 2 {
			t.Errorf("chunk %d = %dx%d, over the limits", i, c.ColumnCount(), c.RowCount())
		}
	}
	// no rows may be lost or duplicated
	seen := map[string]int{}
	for _, c := range chunks {
		for _, r := range c.Rows {
			seen[r[0]]++
		}
	}
	for i := 0; i < 5; i++ {
		if seen[itoa(i)] != 2 {
			t.Errorf("row %d appears %d times across column groups, want 2", i, seen[itoa(i)])
		}
	}
}

func TestSplitTableForMessages_HeaderOnly(t *testing.T) {
	tbl := &TableData{Headers: []string{"a", "b"}}
	chunks, err := SplitTableForMessages(tbl, DefaultTableLimits())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 || chunks[0].ColumnCount() != 2 {
		t.Errorf("chunks = %+v, want a single header-only table", chunks)
	}
}

func TestSplitTableForMessages_RejectsLongCell(t *testing.T) {
	tbl := &TableData{Headers: []string{"h"}, Rows: [][]string{{strings.Repeat("x", 500)}}}
	_, err := SplitTableForMessages(tbl, DefaultTableLimits())
	if !errors.Is(err, ErrTableTooComplex) {
		t.Errorf("err = %v, want ErrTableTooComplex", err)
	}
}

func TestSplitTableForMessages_NilAndEmpty(t *testing.T) {
	if _, err := SplitTableForMessages(nil, DefaultTableLimits()); !errors.Is(err, ErrTableTooComplex) {
		t.Error("nil table should be too complex")
	}
	if _, err := SplitTableForMessages(&TableData{}, DefaultTableLimits()); !errors.Is(err, ErrTableTooComplex) {
		t.Error("empty table should be too complex")
	}
}

func TestColumnGroups(t *testing.T) {
	tests := []struct {
		name string
		cols int
		max  int
		want [][]int
	}{
		{"fits", 3, 6, [][]int{{0, 1, 2}}},
		{"exact", 6, 6, [][]int{{0, 1, 2, 3, 4, 5}}},
		{"one over", 7, 6, [][]int{{0, 1, 2, 3, 4, 5}, {0, 6}}},
		{"two groups", 12, 6, [][]int{{0, 1, 2, 3, 4, 5}, {0, 6, 7, 8, 9, 10}, {0, 11}}},
		{"max of one widens to two", 4, 1, [][]int{{0, 1}, {0, 2}, {0, 3}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := columnGroups(tc.cols, tc.max)
			if len(got) != len(tc.want) {
				t.Fatalf("groups = %v, want %v", got, tc.want)
			}
			for i := range got {
				if len(got[i]) != len(tc.want[i]) {
					t.Fatalf("group %d = %v, want %v", i, got[i], tc.want[i])
				}
				for j := range got[i] {
					if got[i][j] != tc.want[i][j] {
						t.Fatalf("group %d = %v, want %v", i, got, tc.want)
					}
				}
			}
		})
	}
}

func TestRenderTableAsText(t *testing.T) {
	tbl := &TableData{
		Headers: []string{"a", "b"},
		Rows:    [][]string{{"1", "2"}, {"x|y", "multi\nline"}},
	}
	got := RenderTableAsText(tbl)

	lines := strings.Split(got, "\n")
	if len(lines) != 4 {
		t.Fatalf("rendered %d lines, want 4 (header, separator, 2 rows):\n%s", len(lines), got)
	}
	if !strings.Contains(lines[1], "---") {
		t.Errorf("separator row missing: %q", lines[1])
	}
	if !strings.Contains(got, `x\|y`) {
		t.Errorf("a pipe in a cell was not escaped:\n%s", got)
	}
	if strings.Contains(got, "multi\nline") {
		t.Errorf("a newline in a cell was not flattened:\n%s", got)
	}
}

func TestRenderTableAsText_Nil(t *testing.T) {
	if got := RenderTableAsText(nil); got != "" {
		t.Errorf("RenderTableAsText(nil) = %q, want empty", got)
	}
	if got := RenderTableAsText(&TableData{}); got != "" {
		t.Errorf("empty table = %q, want empty", got)
	}
}

func TestTableLimits_Normalized(t *testing.T) {
	got := TableLimits{}.normalized()
	want := DefaultTableLimits()
	if got != want {
		t.Errorf("zero limits normalized to %+v, want %+v", got, want)
	}
	custom := TableLimits{MaxColumns: 2}.normalized()
	if custom.MaxColumns != 2 {
		t.Errorf("MaxColumns = %d, want 2 (explicit value kept)", custom.MaxColumns)
	}
	if custom.MaxRows != want.MaxRows || custom.MaxCellRunes != want.MaxCellRunes || custom.MaxBytes != want.MaxBytes {
		t.Errorf("other fields should fall back to defaults: %+v", custom)
	}
}

// itoa avoids importing strconv for a single use.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
