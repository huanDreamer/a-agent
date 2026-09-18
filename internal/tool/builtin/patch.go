package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/huan/huan-agent/internal/edit"
)

// ApplyPatchToolName is the tool a model calls for a multi-file edit.
const ApplyPatchToolName = "apply_patch"

// ApplyPatchInput is the parameter schema of the "apply_patch" tool.
//
// The structured form is the primary one. A unified diff is accepted too, for the
// case where the model already has one (its own output, or a patch someone pasted
// into the conversation), and the two share one applier — so the atomicity
// guarantee does not depend on which form was used.
type ApplyPatchInput struct {
	Operations []PatchOperation `json:"operations,omitempty" jsonschema:"description=The edits to apply: one entry per file. Either this or patch is required"`
	Patch      string           `json:"patch,omitempty" jsonschema:"description=Unified diff to apply instead of operations; for pasting an existing diff"`
	DryRun     bool             `json:"dry_run,omitempty" jsonschema:"description=Compute and show the plan without changing any file"`
	// The commas in these descriptions would be read as tag separators.
	Reason string `json:"reason,omitempty" jsonschema:"description=One line on why this change is needed; shown to the person who approves it"`
}

// PatchOperation is one file's edits in the structured form.
type PatchOperation struct {
	Path  string      `json:"path" jsonschema:"description=Workspace-relative path of the file to edit, required"`
	Edits []PatchEdit `json:"edits" jsonschema:"description=Edits applied in order, each to the result of the previous one, required"`
}

// PatchEdit is one replacement, with the same matching rules as edit_file.
type PatchEdit struct {
	OldString  string `json:"old_string" jsonschema:"description=Exact text to find; must be unique in the file unless replace_all is set, required"`
	NewString  string `json:"new_string" jsonschema:"description=Text to put in its place, required"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"description=Replace every occurrence instead of requiring a unique match"`
}

// ApplyPatchOutput is what the tool returns.
type ApplyPatchOutput struct {
	// DryRun reports that nothing was written.
	DryRun bool `json:"dry_run,omitempty"`
	// Summary is the one-line-plus-list account of the change.
	Summary string `json:"summary"`
	// Files is the per-file detail, in the order the operations named them.
	Files []edit.FileResult `json:"files"`
	// Preview is the bounded diff; returned for a dry run and for the approval
	// card, so what a person approves is the change that will happen.
	Preview []edit.PreviewEntry `json:"preview,omitempty"`
	Added   int                 `json:"added"`
	Removed int                 `json:"removed"`
	// Applied reports that the files were written.
	Applied bool `json:"applied"`
	// Warnings are things worth knowing that are not reasons to refuse: today, a
	// diff whose @@ line counts disagree with its body.
	Warnings []string `json:"warnings,omitempty"`
}

// PatchWorkspace is the slice of the workspace this tool needs, so a test can
// drive it without a real sandbox.
type PatchWorkspace interface {
	Plan(ctx context.Context, ops []edit.Operation) (*edit.Plan, error)
	Apply(ctx context.Context, plan *edit.Plan) (edit.Report, error)
}

// NewApplyPatchTool builds the tool.
func NewApplyPatchTool(ws PatchWorkspace, opts ApplyPatchOptions) (tool.InvokableTool, error) {
	if ws == nil {
		return nil, fmt.Errorf("apply_patch: 需要一个工作区")
	}
	return utils.InferTool(ApplyPatchToolName,
		"Apply a set of edits to one or more files, all or nothing. "+
			"Use it when a change spans several files (a rename, a signature change, the same fix in three places): either every edit lands or none does, "+
			"so you never leave the repository in a half-changed state that does not compile. "+
			"For a single change in one file use edit_file instead; to change every reference to a symbol use rename_symbol. "+
			"Each old_string must match exactly once unless replace_all is set. "+
			"Pass dry_run=true to see the plan without changing anything. "+
			"It cannot create or delete files (use write_file or bash for that).",
		func(ctx context.Context, in ApplyPatchInput) (ApplyPatchOutput, error) {
			return applyPatch(ctx, ws, in, opts)
		})
}

// ApplyPatchOptions configures the tool.
type ApplyPatchOptions struct {
	// Stale reports whether this workspace is read-only.
	ReadOnly bool
	// MaxBytes is the per-file write limit.
	MaxBytes int64
}

func applyPatch(ctx context.Context, ws PatchWorkspace, in ApplyPatchInput, opts ApplyPatchOptions) (ApplyPatchOutput, error) {
	if opts.ReadOnly {
		return ApplyPatchOutput{}, fmt.Errorf("apply_patch: 这个工作区是只读的")
	}
	operations := make([]edit.Operation, 0, len(in.Operations))
	for _, op := range in.Operations {
		edits := make([]edit.Edit, 0, len(op.Edits))
		for _, e := range op.Edits {
			edits = append(edits, edit.Edit{Old: e.OldString, New: e.NewString, ReplaceAll: e.ReplaceAll})
		}
		operations = append(operations, edit.Operation{Path: op.Path, Edits: edits})
	}
	if len(operations) == 0 && strings.TrimSpace(in.Patch) == "" {
		return ApplyPatchOutput{}, fmt.Errorf("apply_patch: 需要 operations 或 patch 其中之一")
	}

	// The diff form, for when the model already has one. It is parsed into the same
	// operations the structured form produces, so it goes through the same applier:
	// there is one place where "either every edit lands or none does" is written,
	// and which input form was used cannot weaken it.
	var warnings []string
	if len(operations) == 0 {
		parsed, warns, perr := edit.ParseUnifiedWithWarnings(in.Patch)
		if perr != nil {
			return ApplyPatchOutput{}, perr
		}
		operations = parsed
		warnings = warns
	}

	plan, err := ws.Plan(ctx, operations)
	if err != nil {
		return ApplyPatchOutput{}, err
	}
	if in.DryRun {
		return ApplyPatchOutput{
			DryRun:   true,
			Summary:  fmt.Sprintf("将会修改 %d 个文件（+%d -%d），这是预演，没有落盘。", len(plan.Files), plan.Added, plan.Removed),
			Preview:  plan.Preview(previewLines),
			Added:    plan.Added,
			Removed:  plan.Removed,
			Files:    filesOf(plan),
			Warnings: warnings,
		}, nil
	}

	report, err := ws.Apply(ctx, plan)
	if err != nil {
		// The applier has already restored the disk; the message says so, and
		// repeating it here is what keeps a model from trying to "check whether the
		// first files went through".
		return ApplyPatchOutput{}, err
	}
	return ApplyPatchOutput{
		Summary:  fmt.Sprintf("改了 %d 个文件（+%d -%d）。", len(report.Files), report.Added, report.Removed),
		Files:    report.Files,
		Added:    report.Added,
		Removed:  report.Removed,
		Applied:  true,
		Warnings: warnings,
	}, nil
}

// previewLines bounds how much of each file the preview shows.
const previewLines = 30

func filesOf(plan *edit.Plan) []edit.FileResult {
	out := make([]edit.FileResult, 0, len(plan.Files))
	for _, f := range plan.Files {
		out = append(out, edit.FileResult{
			Path: f.Path, Added: f.Added, Removed: f.Removed, Replacements: f.Replacements,
		})
	}
	return out
}
