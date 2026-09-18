package builtin

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/huan/huan-agent/internal/lsp"
	"github.com/huan/huan-agent/internal/workspace"
)

// The four read-only code-intelligence tools.
//
// They answer what string search cannot: where something is defined, who
// references it, what symbols exist, and what is actually broken. Each is only
// registered when a language server covers the workspace (see
// CodeIntelligenceTools), because a tool whose every call can only fail is worse
// than a tool that is not on the menu.
//
// All four are tagged read-only: they observe and never write, which is why they
// are exactly as useful on a read-only workspace as on a writable one. Reading is
// not what a read-only workspace forbids.
//
// Every one returns a `summary` field holding the answer already rendered — the
// same shape the plan tools use — so the model can act on the text directly while
// a caller that wants structure still has it.

// Names of the four tools. They are constants so the registration site, the
// allow-list handling and the tests cannot disagree about them.
const (
	DiagnosticsToolName      = "diagnostics"
	GotoDefinitionToolName   = "goto_definition"
	FindReferencesToolName   = "find_references"
	WorkspaceSymbolsToolName = "workspace_symbols"
)

// DefaultCodeIntelMaxResults caps a query's answer.
//
// A reference query on a widely used function returns hundreds of sites; a model
// cannot act on hundreds, and pasting them in would crowd out the file it is
// trying to edit. The total is always reported, so the cap is never silent.
const DefaultCodeIntelMaxResults = 100

// CodeIntelSource is what the tools need from the language-server layer.
//
// It is an interface rather than *lsp.Manager so the tools can be tested without
// a language server — which is the only way to test them on a machine that does
// not have one installed.
type CodeIntelSource interface {
	// ClientFor returns the client for a file, or (nil, nil) when no server
	// covers it.
	ClientFor(ctx context.Context, path string) (CodeIntelClient, error)
	// ClientForRoot returns the client for a workspace root.
	ClientForRoot(ctx context.Context, root string) (CodeIntelClient, error)
	// Covers reports whether any configured server handles this path. The
	// workspace-wide sweep uses it to decide which files are worth asking about:
	// a Go server has nothing to say about a Markdown file, and opening one to
	// find that out costs a round trip.
	Covers(path string) bool
}

// CodeIntelClient is the part of a language-server session these tools use.
//
// It is an interface so the tools can be tested against a scripted client — the
// answers they have to handle (no definition, an empty reference list, a server
// that has not caught up) are all about what the session returns, and a test that
// needs a real language server is a test that gets skipped on most machines.
// *lsp.Client satisfies it.
type CodeIntelClient interface {
	Open(ctx context.Context, path string) error
	Definition(ctx context.Context, path string, at lsp.LineCol) ([]lsp.Location, error)
	References(ctx context.Context, path string, at lsp.LineCol, includeDeclaration bool) ([]lsp.Location, error)
	WorkspaceSymbols(ctx context.Context, query string) ([]lsp.Symbol, error)
	WaitDiagnostics(ctx context.Context, path string, maxWait time.Duration) ([]lsp.Diagnostic, bool)
	Root() string
}

// CodeIntelOptions configures the tool set.
type CodeIntelOptions struct {
	// Workspace resolves and displays paths inside the sandbox.
	Workspace *workspace.Workspace
	// Source is the language-server layer.
	Source CodeIntelSource
	// MaxResults caps how many locations a query returns. Zero uses the default.
	MaxResults int
}

func (o CodeIntelOptions) maxResults() int {
	if o.MaxResults > 0 {
		return o.MaxResults
	}
	return DefaultCodeIntelMaxResults
}

// --- diagnostics ---

// DiagnosticsInput is the parameter schema of the "diagnostics" tool.
type DiagnosticsInput struct {
	Path string `json:"path,omitempty" jsonschema:"description=File to check; a path inside the workspace. Omit to check the whole workspace"`
	// The commas inside these descriptions would be read as tag separators, so
	// the wording uses semicolons instead.
	Severity string `json:"severity,omitempty" jsonschema:"description=Lowest severity to report: error; warning (the default); info; hint"`
}

// DiagItem is one diagnostic in the answer.
type DiagItem struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Source   string `json:"source,omitempty"`
	Code     string `json:"code,omitempty"`
}

// DiagnosticsOutput is what the "diagnostics" tool returns.
type DiagnosticsOutput struct {
	// Summary is the rendered answer, ready to read.
	Summary string `json:"summary"`
	// Count is how many diagnostics matched; Items may be shorter than this.
	Count int `json:"count"`
	// Files is how many files were checked.
	Files int `json:"files"`
	// Ready reports whether the language server had caught up with the files on
	// disk. False means the list may be partial, and says so rather than implying
	// the code is clean.
	Ready bool       `json:"ready"`
	Items []DiagItem `json:"items"`
}

// NewDiagnosticsTool reports errors and warnings for a file or the workspace.
func NewDiagnosticsTool(opts CodeIntelOptions) (tool.InvokableTool, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	return utils.InferTool(DiagnosticsToolName,
		"Report compiler and analyzer diagnostics (errors and warnings) for one file, or for the whole workspace when path is omitted. "+
			"After changing code, use this to find out whether the change compiles instead of assuming it does. "+
			"The language server is asked about files on disk, so call it after a write rather than before.",
		func(ctx context.Context, in DiagnosticsInput) (DiagnosticsOutput, error) {
			return diagnostics(ctx, opts, in)
		})
}

func diagnostics(ctx context.Context, opts CodeIntelOptions, in DiagnosticsInput) (DiagnosticsOutput, error) {
	floor, err := severityFloor(in.Severity)
	if err != nil {
		return DiagnosticsOutput{}, err
	}

	client, files, err := clientAndFiles(ctx, opts, in.Path)
	if err != nil {
		return DiagnosticsOutput{}, err
	}
	if client == nil {
		out := DiagnosticsOutput{Ready: false, Items: []DiagItem{}}
		out.Summary = "该文件类型没有可用的语言服务器，因此没有诊断信息。"
		if in.Path == "" {
			out.Summary = "工作区里没有可用的语言服务器（检查 tools.lsp.servers 配置），因此没有诊断信息。"
		}
		return out, nil
	}

	items := make([]DiagItem, 0, 16)
	ready := true
	checked := 0
	for _, file := range files {
		if err := client.Open(ctx, file); err != nil {
			// A file that cannot be read is skipped rather than failing the whole
			// query: an unreadable file in a big workspace should not hide the
			// errors in the readable ones.
			continue
		}
		checked++
		diags, settled := client.WaitDiagnostics(ctx, file, lsp.DefaultRequestTimeout)
		if !settled {
			ready = false
		}
		rel, relErr := opts.Workspace.RelWithin(file)
		display := file
		if relErr == nil {
			display = rel
		}
		for _, d := range diags {
			if d.Severity != 0 && d.Severity > floor {
				continue
			}
			// A position that cannot be converted (a report about a version of
			// the file that is no longer on disk) keeps its raw 1-based numbers
			// rather than being dropped: the message is the part a reader acts
			// on, and silently losing a problem is the worst failure this tool
			// has.
			at, cerr := positionOf(file, d.Range.Start)
			if cerr != nil {
				at = lsp.LineCol{Line: d.Range.Start.Line + 1, Column: d.Range.Start.Character + 1}
			}
			items = append(items, DiagItem{
				Path:     display,
				Line:     at.Line,
				Column:   at.Column,
				Severity: lsp.SeverityName(d.Severity),
				Message:  oneLineText(d.Message),
				Source:   d.Source,
				Code:     d.Code,
			})
		}
	}

	sortDiagItems(items)
	out := DiagnosticsOutput{Count: len(items), Files: checked, Ready: ready, Items: items}
	if len(items) > opts.maxResults() {
		out.Items = items[:opts.maxResults()]
	}
	out.Summary = renderDiagnostics(out, len(items))
	return out, nil
}

// renderDiagnostics turns the answer into text a model can act on.
//
// It renders rather than leaving the JSON alone because the model has to use it:
// "internal/foo/bar.go:42:9 error undefined: BAZ" is directly usable, while an
// array of objects with numeric severities has to be decoded first.
func renderDiagnostics(out DiagnosticsOutput, total int) string {
	var b strings.Builder
	switch {
	case total == 0 && out.Ready:
		fmt.Fprintf(&b, "无诊断（已检查 %d 个文件）", out.Files)
		return b.String()
	case total == 0:
		return fmt.Sprintf("语言服务器尚未返回诊断（已检查 %d 个文件，可能仍在索引）", out.Files)
	}

	fmt.Fprintf(&b, "%d 条诊断", total)
	if total > len(out.Items) {
		fmt.Fprintf(&b, "（只显示前 %d 条）", len(out.Items))
	}
	b.WriteString("：")
	for _, d := range out.Items {
		fmt.Fprintf(&b, "\n  %s:%d:%d  %s  %s", d.Path, d.Line, d.Column, d.Severity, d.Message)
		if d.Source != "" {
			fmt.Fprintf(&b, "  (%s)", d.Source)
		}
	}
	if !out.Ready {
		b.WriteString("\n  （语言服务器尚未完成索引，以上可能不完整）")
	}
	return b.String()
}

// --- goto_definition ---

// PositionInput is the parameter schema of the position-based tools.
type PositionInput struct {
	Path string `json:"path" jsonschema:"description=File containing the symbol; a path inside the workspace, required"`
	// The comma inside this description would be read as a tag separator.
	Line   int `json:"line" jsonschema:"description=1-based line number as printed by read_file, required"`
	Column int `json:"column" jsonschema:"description=1-based column measured in characters, required"`
}

// LocationItem is one place in a file.
type LocationItem struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	// Snippet is the line's text, so the answer shows what it is talking about
	// instead of only where it is.
	Snippet string `json:"snippet,omitempty"`
}

// LocationsOutput is the answer of the position-based tools.
type LocationsOutput struct {
	Summary string         `json:"summary"`
	Count   int            `json:"count"`
	Items   []LocationItem `json:"items"`
}

// NewGotoDefinitionTool jumps to a symbol's definition.
func NewGotoDefinitionTool(opts CodeIntelOptions) (tool.InvokableTool, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	return utils.InferTool(GotoDefinitionToolName,
		"Find where the symbol at a position is defined, using the language server rather than a text search. "+
			"Positions are 1-based: line as printed by read_file, column counted in characters (1 is the first character of the line).",
		func(ctx context.Context, in PositionInput) (LocationsOutput, error) {
			return gotoDefinition(ctx, opts, in)
		})
}

func gotoDefinition(ctx context.Context, opts CodeIntelOptions, in PositionInput) (LocationsOutput, error) {
	path, err := opts.resolve(in.Path)
	if err != nil {
		return LocationsOutput{}, err
	}
	if err := checkPosition(path, in.Line, in.Column); err != nil {
		return LocationsOutput{}, fmt.Errorf("%s: %w", in.Path, err)
	}
	client, err := opts.Source.ClientFor(ctx, path)
	if err != nil {
		return LocationsOutput{}, codeIntelError(err)
	}
	if client == nil {
		return LocationsOutput{Summary: noServerForFile}, nil
	}

	locs, err := client.Definition(ctx, path, lsp.LineCol{Line: in.Line, Column: in.Column})
	if err != nil {
		return LocationsOutput{}, codeIntelError(err)
	}
	items := opts.toItems(locs, client.Root())
	out := LocationsOutput{Count: len(items), Items: items}
	out.Summary = renderLocations("定义", items, "没有找到定义。可能位置不准确（检查 column 是否指向符号本身），或者该符号由外部依赖提供。")
	return out, nil
}

// --- find_references ---

// ReferencesInput is the parameter schema of "find_references".
type ReferencesInput struct {
	Path               string `json:"path" jsonschema:"description=File containing the symbol; a path inside the workspace, required"`
	Line               int    `json:"line" jsonschema:"description=1-based line number as printed by read_file, required"`
	Column             int    `json:"column" jsonschema:"description=1-based column measured in characters, required"`
	IncludeDeclaration bool   `json:"include_declaration,omitempty" jsonschema:"description=Include the definition itself in the results"`
}

// NewFindReferencesTool lists every reference to a symbol.
func NewFindReferencesTool(opts CodeIntelOptions) (tool.InvokableTool, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	return utils.InferTool(FindReferencesToolName,
		"List every place that references the symbol at a position, using the language server's view of the code rather than a text search. "+
			"Use this before changing a signature: grep finds the strings, this finds the call sites, and a call site grep misses leaves a repository that does not compile. "+
			"Positions are 1-based.",
		func(ctx context.Context, in ReferencesInput) (LocationsOutput, error) {
			return findReferences(ctx, opts, in)
		})
}

func findReferences(ctx context.Context, opts CodeIntelOptions, in ReferencesInput) (LocationsOutput, error) {
	path, err := opts.resolve(in.Path)
	if err != nil {
		return LocationsOutput{}, err
	}
	if err := checkPosition(path, in.Line, in.Column); err != nil {
		return LocationsOutput{}, fmt.Errorf("%s: %w", in.Path, err)
	}
	client, err := opts.Source.ClientFor(ctx, path)
	if err != nil {
		return LocationsOutput{}, codeIntelError(err)
	}
	if client == nil {
		return LocationsOutput{Summary: noServerForFile}, nil
	}

	locs, err := client.References(ctx, path, lsp.LineCol{Line: in.Line, Column: in.Column}, in.IncludeDeclaration)
	if err != nil {
		return LocationsOutput{}, codeIntelError(err)
	}
	items := opts.toItems(locs, client.Root())
	total := len(items)
	if total > opts.maxResults() {
		items = items[:opts.maxResults()]
	}
	out := LocationsOutput{Count: total, Items: items}
	out.Summary = renderLocations("引用", items, "没有引用。如果这是导出符号，调用点可能在其他模块里（语言服务器只索引它自己的工作区）。")
	if total > len(items) {
		out.Summary += fmt.Sprintf("\n（共 %d 处，只显示前 %d 处）", total, len(items))
	}
	return out, nil
}

// --- workspace_symbols ---

// SymbolsInput is the parameter schema of "workspace_symbols".
type SymbolsInput struct {
	Query string `json:"query" jsonschema:"description=Name or part of a name to search for. An empty query lists the workspace's symbols, required"`
}

// SymbolItem is one symbol in the answer.
type SymbolItem struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Path string `json:"path"`
	Line int    `json:"line"`
	// Container is the enclosing type or package, when the server reports one.
	Container string `json:"container,omitempty"`
}

// SymbolsOutput is what "workspace_symbols" returns.
type SymbolsOutput struct {
	Summary string       `json:"summary"`
	Count   int          `json:"count"`
	Items   []SymbolItem `json:"items"`
}

// NewWorkspaceSymbolsTool searches for symbols by name.
func NewWorkspaceSymbolsTool(opts CodeIntelOptions) (tool.InvokableTool, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	return utils.InferTool(WorkspaceSymbolsToolName,
		"Search the workspace for symbols (functions, types, methods, constants) by name, using the language server's index. "+
			"This is the right tool for \"which code handles authentication\": search for a likely name and you get real definitions with their file and line, "+
			"where grep would give you every string that looks similar.",
		func(ctx context.Context, in SymbolsInput) (SymbolsOutput, error) {
			return workspaceSymbols(ctx, opts, in)
		})
}

func workspaceSymbols(ctx context.Context, opts CodeIntelOptions, in SymbolsInput) (SymbolsOutput, error) {
	client, err := opts.Source.ClientForRoot(ctx, opts.Workspace.Root())
	if err != nil {
		return SymbolsOutput{}, codeIntelError(err)
	}
	if client == nil {
		return SymbolsOutput{Summary: noServerForWorkspace}, nil
	}

	syms, err := client.WorkspaceSymbols(ctx, in.Query)
	if err != nil {
		return SymbolsOutput{}, codeIntelError(err)
	}

	items := make([]SymbolItem, 0, len(syms))
	for _, s := range syms {
		uri := s.Location.URI
		if uri == "" {
			uri = s.URI
		}
		file, uerr := lsp.URIToPath(uri)
		if uerr != nil {
			continue
		}
		display, relErr := opts.Workspace.RelWithin(file)
		if relErr != nil {
			continue // outside the workspace: not ours to report
		}
		at, perr := positionOf(file, s.Location.Range.Start)
		line := 0
		if perr == nil {
			line = at.Line
		}
		if line == 0 && s.Location.Range.Start.Line == 0 && s.Location.Range.Start.Character == 0 {
			line = 1 // some servers report no range for a workspace symbol
		}
		items = append(items, SymbolItem{
			Name:      s.Name,
			Kind:      lsp.SymbolKindName(s.Kind),
			Path:      display,
			Line:      line,
			Container: s.ContainerName,
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Path != items[j].Path {
			return items[i].Path < items[j].Path
		}
		return items[i].Line < items[j].Line
	})

	total := len(items)
	if total > opts.maxResults() {
		items = items[:opts.maxResults()]
	}
	out := SymbolsOutput{Count: total, Items: items}
	out.Summary = renderSymbols(in.Query, items, total)
	return out, nil
}

func renderSymbols(query string, items []SymbolItem, total int) string {
	if total == 0 {
		if strings.TrimSpace(query) == "" {
			return "没有找到符号。试一个更具体的名字，或者确认语言服务器已经完成索引。"
		}
		return fmt.Sprintf("没有匹配 %q 的符号。换一个名字试试，或者用 grep 找字符串。", query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "找到 %d 个符号", total)
	if total > len(items) {
		fmt.Fprintf(&b, "（只显示前 %d 个）", len(items))
	}
	b.WriteString("：")
	for _, s := range items {
		fmt.Fprintf(&b, "\n  %s:%d  %s  %s", s.Path, s.Line, s.Kind, s.Name)
		if s.Container != "" {
			fmt.Fprintf(&b, "  (%s)", s.Container)
		}
	}
	return b.String()
}

// --- shared ---

const (
	noServerForFile      = "该文件类型没有配置语言服务器，无法做语义查询。用 grep 代替，或者配置 tools.lsp.servers。"
	noServerForWorkspace = "工作区没有可用的语言服务器（检查 tools.lsp.servers），无法做符号搜索。"
)

func (o CodeIntelOptions) validate() error {
	if o.Workspace == nil {
		return fmt.Errorf("code intelligence: workspace is required")
	}
	if o.Source == nil {
		return fmt.Errorf("code intelligence: a language-server source is required")
	}
	return nil
}

// resolve puts a caller-supplied path through the sandbox, so a tool cannot be
// pointed outside the workspace by a `..` in its arguments.
func (o CodeIntelOptions) resolve(path string) (string, error) {
	abs, err := o.Workspace.Resolve(path)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// toItems converts LSP locations into the tool's answer, dropping anything
// outside the workspace and reading each line so the answer shows its context.
func (o CodeIntelOptions) toItems(locs []lsp.Location, root string) []LocationItem {
	items := make([]LocationItem, 0, len(locs))
	for _, l := range locs {
		file, err := lsp.URIToPath(l.URI)
		if err != nil {
			continue
		}
		display, relErr := o.Workspace.RelWithin(file)
		if relErr != nil {
			continue // outside the workspace
		}
		at, perr := positionOf(file, l.Range.Start)
		if perr != nil {
			continue
		}
		items = append(items, LocationItem{
			Path:    display,
			Line:    at.Line,
			Column:  at.Column,
			Snippet: lsp.SnippetAt(file, at),
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Path != items[j].Path {
			return items[i].Path < items[j].Path
		}
		return items[i].Line < items[j].Line
	})
	_ = root
	return items
}

func renderLocations(what string, items []LocationItem, empty string) string {
	if len(items) == 0 {
		return empty
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d 处%s：", len(items), what)
	for _, it := range items {
		fmt.Fprintf(&b, "\n  %s:%d:%d", it.Path, it.Line, it.Column)
		if it.Snippet != "" {
			fmt.Fprintf(&b, "  %s", it.Snippet)
		}
	}
	return b.String()
}

// checkPosition validates a caller-supplied 1-based position against the file on
// disk before a language server is involved.
//
// Doing it here rather than leaving it to the client is what makes the error the
// tool's own: "a.go: line 99 is out of range (the file has 6 lines)" tells the
// model exactly what to fix, where a clamped position would quietly answer about
// a different line.
func checkPosition(path string, line, column int) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	_, err = lsp.ToLSP(content, lsp.LineCol{Line: line, Column: column})
	return err
}

// positionOf converts an LSP position into 1-based line/column for a file on
// disk. A file that cannot be read gives an error rather than a guess.
func positionOf(file string, p lsp.Position) (lsp.LineCol, error) {
	content, err := os.ReadFile(file)
	if err != nil {
		return lsp.LineCol{}, fmt.Errorf("read %s: %w", file, err)
	}
	return lsp.FromLSP(content, p)
}

// codeIntelNoiseDir reports whether a directory is one the searches skip. It
// reuses the same list, so a workspace sweep and a grep agree about what is
// worth looking at.
func codeIntelNoiseDir(name string) bool {
	for _, d := range searchNoiseDirs {
		if d == name {
			return true
		}
	}
	return false
}

// codeIntelTextFiles lists the text files under a root, bounded and skipping the
// same noise directories the search tools skip.
//
// A workspace-wide diagnostics query has to name the files it asks about, and
// there is no cheaper way to name them: opening every file in a repository is
// slow, which is why the caller caps the count and reports how many it checked
// rather than implying it looked at everything.
func codeIntelTextFiles(root string, ws *workspace.Workspace, sweep CodeIntelSource, limit int) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable entry is skipped, not fatal
		}
		if d.IsDir() {
			if path != root && codeIntelNoiseDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		// Symlinks are skipped outright, the same rule the grep/glob walk uses:
		// resolving the start directory would still let a link inside the root
		// lead the walk out of it.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !sweep.Covers(path) {
			return nil // no configured server handles this file type
		}
		if _, rerr := ws.RelWithin(path); rerr != nil {
			return nil
		}
		out = append(out, path)
		if len(out) >= limit {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// clientAndFiles resolves the files a diagnostics query should check.
func clientAndFiles(ctx context.Context, opts CodeIntelOptions, path string) (CodeIntelClient, []string, error) {
	if strings.TrimSpace(path) != "" {
		abs, err := opts.resolve(path)
		if err != nil {
			return nil, nil, err
		}
		client, err := opts.Source.ClientFor(ctx, abs)
		if err != nil {
			return nil, nil, codeIntelError(err)
		}
		return client, []string{abs}, nil
	}

	client, err := opts.Source.ClientForRoot(ctx, opts.Workspace.Root())
	if err != nil {
		return nil, nil, codeIntelError(err)
	}
	if client == nil {
		return nil, nil, nil
	}
	files, err := codeIntelTextFiles(opts.Workspace.Root(), opts.Workspace, opts.Source, maxDiagFiles)
	if err != nil {
		return nil, nil, err
	}
	return client, files, nil
}

// codeIntelError turns an unavailable server into something the model can act on
// rather than a bare transport error.
func codeIntelError(err error) error {
	if err == nil {
		return nil
	}
	if lsp.IsServerMissing(err) {
		return fmt.Errorf("%w（语义查询不可用）", err)
	}
	return err
}

// severityFloor maps a configured name to the numeric threshold to include.
// LSP numbers severities 1..4 from most to least serious.
func severityFloor(name string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "":
		return lsp.SeverityWarning, nil
	case "error":
		return lsp.SeverityError, nil
	case "warning", "warn":
		return lsp.SeverityWarning, nil
	case "info", "information":
		return lsp.SeverityInformation, nil
	case "hint":
		return lsp.SeverityHint, nil
	default:
		return 0, fmt.Errorf("unknown severity %q (want error, warning, info or hint)", name)
	}
}

// sortDiagItems orders diagnostics by file and position, so the same query gives
// the same answer twice — which is what makes the output testable and diffable.
func sortDiagItems(items []DiagItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Path != items[j].Path {
			return items[i].Path < items[j].Path
		}
		if items[i].Line != items[j].Line {
			return items[i].Line < items[j].Line
		}
		return items[i].Column < items[j].Column
	})
}

// oneLineText collapses a message onto one line: a compiler prints suggestions
// inline, and a multi-line entry destroys the alignment that makes a list of
// diagnostics scannable.
func oneLineText(s string) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if len([]rune(s)) > 300 {
		return string([]rune(s)[:300]) + "…"
	}
	return s
}

// maxDiagFiles caps a workspace-wide diagnostics sweep. A large repository has
// thousands of files, and opening every one of them to ask a server is a worse
// use of time than answering about the file the caller actually changed.
const maxDiagFiles = 500
