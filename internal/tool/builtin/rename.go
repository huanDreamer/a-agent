package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/huan/huan-agent/internal/edit"
	"github.com/huan/huan-agent/internal/lsp"
)

// RenameSymbolToolName is the tool a model calls to rename a symbol everywhere.
const RenameSymbolToolName = "rename_symbol"

// RenameSymbolInput is the parameter schema of the "rename_symbol" tool.
type RenameSymbolInput struct {
	// The commas that would normally separate tag options are semicolons here: the
	// schema parser splits on commas.
	Path    string `json:"path" jsonschema:"description=Workspace-relative path of the file containing the symbol, required"`
	Line    int    `json:"line" jsonschema:"description=1-based line of the symbol; the column is 1-based too, required"`
	Column  int    `json:"column" jsonschema:"description=1-based column of the symbol name in that line, required"`
	NewName string `json:"new_name" jsonschema:"description=The new name, required"`
	DryRun  bool   `json:"dry_run,omitempty" jsonschema:"description=Show what would change without writing anything"`
}

// RenameSymbolOutput is what the tool returns.
type RenameSymbolOutput struct {
	DryRun      bool                `json:"dry_run,omitempty"`
	Applied     bool                `json:"applied"`
	NewName     string              `json:"new_name"`
	Summary     string              `json:"summary"`
	Files       []edit.FileResult   `json:"files"`
	Preview     []edit.PreviewEntry `json:"preview,omitempty"`
	Added       int                 `json:"added"`
	Removed     int                 `json:"removed"`
	Occurrences int                 `json:"occurrences"`
}

// Renamer is the language-server side: it answers "rename this symbol" with the
// edits to make.
type Renamer interface {
	// Rename asks the server for the edits that rename the symbol at a position.
	Rename(ctx context.Context, path string, at lsp.LineCol, newName string) (lsp.WorkspaceEdit, error)
}

// EditBatch is the applier this tool uses, which is the same one apply_patch uses.
//
// It takes the rename's answer rather than a converted batch, because the
// conversion has to go through the sandbox: a language server names absolute paths
// that have never been confined, and the out-of-workspace refusal has to happen
// before anything is planned, let alone written.
type EditBatch interface {
	PlanWorkspaceEdit(ctx context.Context, we lsp.WorkspaceEdit) (*edit.Plan, error)
	Apply(ctx context.Context, plan *edit.Plan) (edit.Report, error)
}

// NewRenameSymbolTool builds the tool.
func NewRenameSymbolTool(renamer Renamer, batch EditBatch) (tool.InvokableTool, error) {
	if renamer == nil || batch == nil {
		return nil, fmt.Errorf("rename_symbol: 需要语言服务器与工作区")
	}
	return utils.InferTool(RenameSymbolToolName,
		"Rename a symbol and every reference to it, using the language server's knowledge of what the name refers to. "+
			"Use it whenever a rename touches more than the definition: it changes exactly the references the compiler would, "+
			"not every line that happens to contain the same word (a grep-and-replace also rewrites comments, strings and unrelated symbols with the same name). "+
			"Give the 1-based line and column of the symbol name itself. "+
			"The change is applied all or nothing, exactly like apply_patch, and nothing is written when dry_run is true.",
		func(ctx context.Context, in RenameSymbolInput) (RenameSymbolOutput, error) {
			return renameSymbol(ctx, renamer, batch, in)
		})
}

func renameSymbol(ctx context.Context, renamer Renamer, batch EditBatch, in RenameSymbolInput) (RenameSymbolOutput, error) {
	path := strings.TrimSpace(in.Path)
	newName := strings.TrimSpace(in.NewName)
	if path == "" || newName == "" {
		return RenameSymbolOutput{}, fmt.Errorf("rename_symbol: path 与 new_name 都不能为空")
	}
	if in.Line <= 0 || in.Column <= 0 {
		return RenameSymbolOutput{}, fmt.Errorf(
			"rename_symbol: line 与 column 都是 1-based 且必须大于 0（收到 %d:%d）；"+
				"先用 read_file 确认符号所在的行，再数一下它在这一行的第几个字符", in.Line, in.Column)
	}

	we, err := renamer.Rename(ctx, path, lsp.LineCol{Line: in.Line, Column: in.Column}, newName)
	if err != nil {
		return RenameSymbolOutput{}, fmt.Errorf("rename_symbol: %w", err)
	}
	plan, err := batch.PlanWorkspaceEdit(ctx, we)
	if err != nil {
		return RenameSymbolOutput{}, fmt.Errorf("rename_symbol: %w", err)
	}
	occurrences := 0
	for _, f := range plan.Files {
		occurrences += f.Replacements
	}

	if in.DryRun {
		return RenameSymbolOutput{
			DryRun:      true,
			NewName:     newName,
			Occurrences: occurrences,
			Added:       plan.Added,
			Removed:     plan.Removed,
			Preview:     plan.Preview(previewLines),
			Files:       filesOf(plan),
			Summary: fmt.Sprintf("将会重命名为 %s：%d 个文件、%d 处引用（+%d -%d）。这是预演，没有落盘。",
				newName, len(plan.Files), occurrences, plan.Added, plan.Removed),
		}, nil
	}

	report, err := batch.Apply(ctx, plan)
	if err != nil {
		return RenameSymbolOutput{}, err
	}
	return RenameSymbolOutput{
		Applied:     true,
		NewName:     newName,
		Occurrences: occurrences,
		Added:       report.Added,
		Removed:     report.Removed,
		Files:       report.Files,
		Summary: fmt.Sprintf("已重命名为 %s：%d 个文件、%d 处引用（+%d -%d）。",
			newName, len(report.Files), occurrences, report.Added, report.Removed),
	}, nil
}
