package feishu

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrTableTooComplex reports that a table cannot be rendered as a native
// Feishu table, even after splitting across messages (for example because a
// single cell is too long). Callers should fall back to RenderTableAsText.
var ErrTableTooComplex = errors.New("feishu: table too complex to render natively")

// TableData is a parsed markdown (GFM) table.
type TableData struct {
	// Headers are the column titles.
	Headers []string
	// Rows are the body rows; each is normalized to len(Headers) cells.
	Rows [][]string
	// Aligns holds the per-column alignment from the delimiter row:
	// "left", "center" or "right".
	Aligns []string
}

// ColumnCount returns the number of columns.
func (t *TableData) ColumnCount() int {
	if t == nil {
		return 0
	}
	return len(t.Headers)
}

// RowCount returns the number of body rows.
func (t *TableData) RowCount() int {
	if t == nil {
		return 0
	}
	return len(t.Rows)
}

// Clone returns a deep copy.
func (t *TableData) Clone() *TableData {
	if t == nil {
		return nil
	}
	out := &TableData{
		Headers: append([]string(nil), t.Headers...),
		Aligns:  append([]string(nil), t.Aligns...),
		Rows:    make([][]string, len(t.Rows)),
	}
	for i, r := range t.Rows {
		out.Rows[i] = append([]string(nil), r...)
	}
	return out
}

// sliceRows returns a copy holding rows [start,end).
func (t *TableData) sliceRows(start, end int) *TableData {
	out := &TableData{
		Headers: append([]string(nil), t.Headers...),
		Aligns:  append([]string(nil), t.Aligns...),
	}
	out.Rows = make([][]string, 0, end-start)
	for _, r := range t.Rows[start:end] {
		out.Rows = append(out.Rows, append([]string(nil), r...))
	}
	return out
}

// sliceColumns returns a copy holding the given column indexes, in order.
func (t *TableData) sliceColumns(idx []int) *TableData {
	out := &TableData{}
	for _, c := range idx {
		out.Headers = append(out.Headers, cellAt(t.Headers, c))
		out.Aligns = append(out.Aligns, alignAt(t.Aligns, c))
	}
	out.Rows = make([][]string, 0, len(t.Rows))
	for _, r := range t.Rows {
		row := make([]string, 0, len(idx))
		for _, c := range idx {
			row = append(row, cellAt(r, c))
		}
		out.Rows = append(out.Rows, row)
	}
	return out
}

func cellAt(cells []string, i int) string {
	if i < 0 || i >= len(cells) {
		return ""
	}
	return cells[i]
}

func alignAt(aligns []string, i int) string {
	if i < 0 || i >= len(aligns) {
		return ""
	}
	return aligns[i]
}

// TableLimits bounds what can be rendered as native Feishu tables.
type TableLimits struct {
	// MaxColumns per message. Wider tables are split into column groups.
	MaxColumns int
	// MaxRows per message. Longer tables are split into several messages.
	MaxRows int
	// MaxCellRunes caps a single cell; longer cells make the table
	// unrenderable rather than split (splitting could not shorten a cell).
	MaxCellRunes int
	// MaxBytes caps the serialized content of one table.
	MaxBytes int
}

// DefaultTableLimits returns the limits used for Feishu cards. They are
// deliberately conservative: a wide table is unreadable on a phone, and a card
// that is too large is rejected by the platform.
func DefaultTableLimits() TableLimits {
	return TableLimits{
		MaxColumns:   6,
		MaxRows:      20,
		MaxCellRunes: 120,
		MaxBytes:     20000,
	}
}

// normalized fills in sane values for unset fields.
func (l TableLimits) normalized() TableLimits {
	d := DefaultTableLimits()
	if l.MaxColumns <= 0 {
		l.MaxColumns = d.MaxColumns
	}
	if l.MaxRows <= 0 {
		l.MaxRows = d.MaxRows
	}
	if l.MaxCellRunes <= 0 {
		l.MaxCellRunes = d.MaxCellRunes
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = d.MaxBytes
	}
	return l
}

// ParseTable parses a GFM table from its lines. It returns nil when the lines
// are not a table: at least a header row and a delimiter row are required, and
// the delimiter row must consist purely of dashes with optional colons.
//
// Rows with fewer cells than the header are padded with empty cells; extra
// cells are dropped, so every returned row has exactly len(Headers) cells.
func ParseTable(lines []string) *TableData {
	if len(lines) < 2 {
		return nil
	}
	headers, ok := splitTableRow(lines[0])
	if !ok || len(headers) == 0 {
		return nil
	}
	aligns, ok := parseDelimiterRow(lines[1], len(headers))
	if !ok {
		return nil
	}

	t := &TableData{
		Headers: headers,
		Aligns:  aligns,
		Rows:    make([][]string, 0, len(lines)-2),
	}
	for _, ln := range lines[2:] {
		cells, ok := splitTableRow(ln)
		if !ok {
			// A line without pipes ends the table.
			break
		}
		t.Rows = append(t.Rows, normalizeRow(cells, len(headers)))
	}
	return t
}

// normalizeRow pads or truncates a row to exactly n cells.
func normalizeRow(cells []string, n int) []string {
	row := make([]string, n)
	for i := 0; i < n && i < len(cells); i++ {
		row[i] = cells[i]
	}
	return row
}

// splitTableRow splits a table line into cells. It reports false when the line
// contains no pipe at all.
func splitTableRow(line string) ([]string, bool) {
	s := strings.TrimSpace(line)
	if !strings.Contains(s, "|") {
		return nil, false
	}
	// The leading and trailing pipes are optional in GFM.
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")

	raw := strings.Split(s, "|")
	cells := make([]string, 0, len(raw))
	for _, c := range raw {
		cells = append(cells, strings.TrimSpace(c))
	}
	return cells, true
}

// parseDelimiterRow parses the |---|---| row into per-column alignments. Every
// cell must be dashes with optional leading/trailing colons.
func parseDelimiterRow(line string, cols int) ([]string, bool) {
	cells, ok := splitTableRow(line)
	if !ok {
		return nil, false
	}
	aligns := make([]string, 0, len(cells))
	for _, c := range cells {
		c = strings.TrimSpace(c)
		if c == "" {
			return nil, false
		}
		left := strings.HasPrefix(c, ":")
		right := strings.HasSuffix(c, ":")
		body := strings.TrimSuffix(strings.TrimPrefix(c, ":"), ":")
		if body == "" || strings.Trim(body, "-") != "" {
			return nil, false
		}
		switch {
		case left && right:
			aligns = append(aligns, "center")
		case right:
			aligns = append(aligns, "right")
		default:
			aligns = append(aligns, "left")
		}
	}
	if cols > 0 && len(aligns) < cols {
		// Tolerate a short delimiter row by defaulting the rest to left.
		for len(aligns) < cols {
			aligns = append(aligns, "left")
		}
	}
	return aligns, true
}

// IsTableStart reports whether the line at i begins a GFM table, i.e. the next
// line is a valid delimiter row.
func IsTableStart(lines []string, i int) bool {
	if i < 0 || i+1 >= len(lines) {
		return false
	}
	headers, ok := splitTableRow(lines[i])
	if !ok || len(headers) == 0 {
		return false
	}
	_, ok = parseDelimiterRow(lines[i+1], len(headers))
	return ok
}

// TableColumn is one column of a native Feishu table.
type TableColumn struct {
	// Name is the unique key used by every row object.
	Name string `json:"name"`
	// DisplayName is the visible header.
	DisplayName string `json:"display_name"`
	// DataType is "text" or "lark_md".
	DataType string `json:"data_type"`
	// Width is "auto" or a fixed width like "120px".
	Width string `json:"width,omitempty"`
	// HorizontalAlign is "left", "center" or "right".
	HorizontalAlign string `json:"horizontal_align,omitempty"`
	// VerticalAlign is "top", "center" or "bottom".
	VerticalAlign string `json:"vertical_align,omitempty"`
}

// TableTextStyle styles the header row of a native table.
type TableTextStyle struct {
	TextAlign       string `json:"text_align,omitempty"`
	TextSize        string `json:"text_size,omitempty"`
	BackgroundColor string `json:"background_color,omitempty"`
	TextColor       string `json:"text_color,omitempty"`
	Bold            bool   `json:"bold,omitempty"`
	Lines           int    `json:"lines,omitempty"`
}

// columnKey builds the row-object key for column i. Column names must be
// unique and stable, so they are derived from the index rather than the header
// text (headers may repeat or contain characters that are awkward as keys).
func columnKey(i int) string { return fmt.Sprintf("col_%d", i) }

// NativeTableElement builds a native Feishu `table` card element.
//
// It returns ErrTableTooComplex when the table cannot be rendered natively at
// all — currently when a single cell exceeds limits.MaxCellRunes, because
// splitting messages cannot shorten a cell. Otherwise the returned element
// always fits the limits (the table must already have been split, see
// SplitTableForMessages).
func NativeTableElement(t *TableData, limits TableLimits) (CardElement, error) {
	if t == nil || t.ColumnCount() == 0 {
		return CardElement{}, ErrTableTooComplex
	}
	lim := limits.normalized()

	for _, r := range t.Rows {
		for _, c := range r {
			if utf8.RuneCountInString(c) > lim.MaxCellRunes {
				return CardElement{}, fmt.Errorf("%w: a cell exceeds %d characters",
					ErrTableTooComplex, lim.MaxCellRunes)
			}
		}
	}
	for _, h := range t.Headers {
		if utf8.RuneCountInString(h) > lim.MaxCellRunes {
			return CardElement{}, fmt.Errorf("%w: a header exceeds %d characters",
				ErrTableTooComplex, lim.MaxCellRunes)
		}
	}
	if t.ColumnCount() > lim.MaxColumns || t.RowCount() > lim.MaxRows {
		return CardElement{}, fmt.Errorf("%w: %d columns x %d rows exceeds the %d x %d limit; split it first",
			ErrTableTooComplex, t.ColumnCount(), t.RowCount(), lim.MaxColumns, lim.MaxRows)
	}

	el := CardElement{
		Tag:       "table",
		PageSize:  t.RowCount(),
		RowHeight: "low",
		HeaderStyle: &TableTextStyle{
			TextAlign:       "left",
			TextSize:        "normal",
			BackgroundColor: "grey",
			TextColor:       "default",
			Bold:            true,
			Lines:           1,
		},
	}
	if el.PageSize <= 0 {
		el.PageSize = 1
	}
	for i, h := range t.Headers {
		el.Columns = append(el.Columns, TableColumn{
			Name:            columnKey(i),
			DisplayName:     h,
			DataType:        "lark_md",
			Width:           "auto",
			HorizontalAlign: alignmentOrDefault(alignAt(t.Aligns, i)),
			VerticalAlign:   "top",
		})
	}
	for _, r := range t.Rows {
		obj := make(map[string]string, len(r))
		for i := range t.Headers {
			obj[columnKey(i)] = cellAt(r, i)
		}
		el.Rows = append(el.Rows, obj)
	}

	if size := estimateTableBytes(t); size > lim.MaxBytes {
		return CardElement{}, fmt.Errorf("%w: serialized table is %d bytes, over the %d limit; split it first",
			ErrTableTooComplex, size, lim.MaxBytes)
	}
	return el, nil
}

// alignmentOrDefault maps a markdown alignment to a Feishu one.
func alignmentOrDefault(a string) string {
	switch a {
	case "center", "right":
		return a
	default:
		return "left"
	}
}

// estimateTableBytes approximates the serialized size of a table.
func estimateTableBytes(t *TableData) int {
	n := 0
	for _, h := range t.Headers {
		n += len(h)
	}
	for _, r := range t.Rows {
		for _, c := range r {
			n += len(c) + len(`"col_0":,`) + 4
		}
	}
	return n
}

// SplitTableForMessages splits a table into pieces that each fit the limits, so
// it can be delivered as several native tables in several messages.
//
// Columns are grouped first (each group repeats column 0 as a key when the
// table is wide), then rows are chunked inside each group so no piece exceeds
// MaxRows. The order of the returned pieces follows the original table.
//
// It returns ErrTableTooComplex when a cell is too long to render at all, since
// no amount of splitting can shorten a cell.
func SplitTableForMessages(t *TableData, limits TableLimits) ([]*TableData, error) {
	if t == nil || t.ColumnCount() == 0 {
		return nil, ErrTableTooComplex
	}
	lim := limits.normalized()

	for _, r := range t.Rows {
		for _, c := range r {
			if utf8.RuneCountInString(c) > lim.MaxCellRunes {
				return nil, fmt.Errorf("%w: a cell exceeds %d characters",
					ErrTableTooComplex, lim.MaxCellRunes)
			}
		}
	}

	// A table with no body rows still renders (header only).
	if t.RowCount() == 0 {
		return []*TableData{t.Clone()}, nil
	}

	groups := columnGroups(t.ColumnCount(), lim.MaxColumns)
	out := make([]*TableData, 0, len(groups))
	for _, g := range groups {
		sub := t.sliceColumns(g)
		for start := 0; start < sub.RowCount(); start += lim.MaxRows {
			end := start + lim.MaxRows
			if end > sub.RowCount() {
				end = sub.RowCount()
			}
			out = append(out, sub.sliceRows(start, end))
		}
	}
	if len(out) == 0 {
		return nil, ErrTableTooComplex
	}
	return out, nil
}

// columnGroups partitions columnCount columns into groups of at most max,
// repeating column 0 at the start of every group after the first so each piece
// keeps an identifying column.
func columnGroups(columnCount, max int) [][]int {
	if columnCount <= max {
		all := make([]int, columnCount)
		for i := range all {
			all[i] = i
		}
		return [][]int{all}
	}
	if max < 2 {
		max = 2 // need room for the repeated key column plus one
	}
	groups := make([][]int, 0, 2)
	// First group: columns [0,max).
	first := make([]int, 0, max)
	for i := 0; i < max && i < columnCount; i++ {
		first = append(first, i)
	}
	groups = append(groups, first)

	// Remaining groups: key column 0 + up to max-1 new columns.
	next := max
	for next < columnCount {
		g := make([]int, 0, max)
		g = append(g, 0)
		for len(g) < max && next < columnCount {
			g = append(g, next)
			next++
		}
		groups = append(groups, g)
	}
	return groups
}

// RenderTableAsText renders a table as a GitHub-style markdown table, used as
// the fallback when a native table cannot be rendered. Every cell is escaped so
// a stray pipe cannot break the row structure.
func RenderTableAsText(t *TableData) string {
	if t == nil || t.ColumnCount() == 0 {
		return ""
	}
	var b strings.Builder
	writeRow := func(cells []string) {
		b.WriteString("|")
		for i := range t.Headers {
			b.WriteString(" ")
			b.WriteString(escapeCell(cellAt(cells, i)))
			b.WriteString(" |")
		}
		b.WriteString("\n")
	}
	writeRow(t.Headers)
	b.WriteString("|")
	for range t.Headers {
		b.WriteString(" --- |")
	}
	b.WriteString("\n")
	for _, r := range t.Rows {
		writeRow(r)
	}
	return strings.TrimRight(b.String(), "\n")
}

// escapeCell neutralises characters that would break a markdown table cell.
func escapeCell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}
