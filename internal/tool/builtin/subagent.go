package builtin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/huan/huan-agent/internal/subagent"
	agenttool "github.com/huan/huan-agent/internal/tool"
)

// The spawn_agent tool: hand a question to a nested agent and get back a report.
//
// It is marked CapRead because it does not touch the workspace itself — the tools
// it may hand on are the parent's, already wrapped in whatever policies the parent
// runs under (approval, checkpoints, the sandbox). Nothing here is a way around
// those.

// SpawnAgentToolName is the tool a model calls to delegate.
const SpawnAgentToolName = "spawn_agent"

// SpawnAgentInput is the parameter schema of the "spawn_agent" tool.
type SpawnAgentInput struct {
	Prompt string `json:"prompt,omitempty" jsonschema:"description=The task for the subagent. It sees nothing else: no conversation history, no file contents. Write it as a self-contained brief. Give either this or tasks"`
	// Tasks runs several subagents **at the same time** and returns one report per
	// task, which is the point of the field: a model that wants three surveys does
	// not have to hope its own loop overlaps three calls.
	Tasks []string `json:"tasks,omitempty" jsonschema:"description=Several independent tasks to run in parallel; each gets its own subagent and report. Use this instead of prompt when the questions do not depend on each other"`
	Name  string   `json:"name,omitempty" jsonschema:"description=Short display name for this subagent, shown in the log and on its card"`
	// The commas inside these descriptions would be read as tag separators, so the
	// wording uses semicolons instead.
	Tools          []string `json:"tools,omitempty" jsonschema:"description=Extra tool names to grant beyond the read-only set; needed only if the subagent must write or run commands"`
	Model          string   `json:"model,omitempty" jsonschema:"description=Model to run the subagent on; a cheaper one for exploration is the usual reason to set it"`
	MaxSteps       int      `json:"max_steps,omitempty" jsonschema:"description=可选。只用来降低上限；不填时子 agent 与当前这一轮用同样的步数预算"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" jsonschema:"description=Wall-clock cap for this subagent; still bounded by the current turn's deadline"`
}

// SpawnAgentOutput is what the tool returns.
//
// For the tasks form, Reports carries one entry per task (in the order given) and
// Report is the combined text the model reads.
type SpawnAgentOutput struct {
	// Reports is one report per task, only for the tasks form.
	Reports []SpawnAgentReport `json:"reports,omitempty"`
	// Report is the bounded conclusion, ready to read. It is the only thing that
	// crossed back from the nested run.
	Report string `json:"report"`
	// Name, Steps, Tools and Usage describe how the report was produced, so the
	// model can judge how much to trust it.
	Name        string   `json:"name"`
	Steps       int      `json:"steps"`
	Tools       []string `json:"tools"`
	TotalTokens int      `json:"total_tokens"`
	// StopReason is set when the subagent ran out of budget rather than finishing.
	StopReason string `json:"stop_reason,omitempty"`
	// Failed reports that the subagent did not complete; Report says why.
	Failed bool `json:"failed,omitempty"`
	// Summary is the one-line account: what ran, how much it cost.
	Summary string `json:"summary,omitempty"`
}

// NewSpawnAgentTool builds the tool.
//
// spawner runs the nested turn; resolve maps a requested model name to a model.
// Both are injected rather than discovered, which is what keeps this tool free of
// any knowledge of how the deployment picks models or runs turns.
func NewSpawnAgentTool(spawner Spawner, resolve agenttool.ModelResolver) (tool.InvokableTool, error) {
	if spawner == nil {
		return nil, fmt.Errorf("spawn_agent: a spawner is required")
	}
	return utils.InferTool(SpawnAgentToolName,
		"Hand a self-contained question to a subagent that works in its own context and returns one short report. "+
			"Use it when answering means reading many files you do not need afterwards (surveying a subsystem, finding every place something is handled): "+
			"the reading stays in the subagent's context and only its conclusion comes back. "+
			"**To work in parallel, pass several independent questions as tasks** — they run at the same time and you get one report each — "+
			"or call this tool several times in one reply; both overlap. Use parallelism when the questions do not depend on each other; "+
			"asking them together is what makes them finish in the time of the slowest instead of the sum. "+
			"It sees no conversation history, so each brief must stand alone, and the subagent has read-only tools unless you list others in tools. "+
			"Do not use it for a question one or two file reads would answer, or for work whose exact lines matter.",
		func(ctx context.Context, in SpawnAgentInput) (SpawnAgentOutput, error) {
			return spawnAgent(ctx, spawner, resolve, in)
		})
}

// Spawner runs subagents. It is what lets this tool be tested without a model.
type Spawner interface {
	Spawn(ctx context.Context, opts subagent.Options) (subagent.Report, error)
	// SpawnMany runs several at once, returning one report per task in order.
	SpawnMany(ctx context.Context, opts []subagent.Options) ([]subagent.Report, error)
}

// SpawnAgentReport is one task's outcome in the tasks form.
type SpawnAgentReport struct {
	Task        string   `json:"task"`
	Name        string   `json:"name"`
	Report      string   `json:"report"`
	Steps       int      `json:"steps"`
	Tools       []string `json:"tools,omitempty"`
	TotalTokens int      `json:"total_tokens"`
	StopReason  string   `json:"stop_reason,omitempty"`
	Failed      bool     `json:"failed,omitempty"`
}

func spawnAgent(ctx context.Context, spawner Spawner, resolve agenttool.ModelResolver, in SpawnAgentInput) (SpawnAgentOutput, error) {
	// The tasks form: several briefs, one subagent each, run together.
	if len(in.Tasks) > 0 {
		if strings.TrimSpace(in.Prompt) != "" {
			return SpawnAgentOutput{}, fmt.Errorf("spawn_agent: prompt 与 tasks 只能给一个" +
				"（prompt 是一个任务，tasks 是要并行跑的多个任务）")
		}
		return spawnMany(ctx, spawner, resolve, in)
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return SpawnAgentOutput{}, fmt.Errorf("spawn_agent: 需要 prompt 或 tasks 其中之一")
	}
	res, ok := agenttool.TurnResourcesFrom(ctx)
	if !ok {
		// A surface that did not publish them cannot have subagents, and saying so
		// beats a nested run against a guessed registry.
		return SpawnAgentOutput{}, fmt.Errorf(
			"spawn_agent：这一轮没有可用的模型或工具注册表，无法派生子 agent（这个工具只在完整的轮次里可用）")
	}

	opts := subagent.Options{
		Prompt:       in.Prompt,
		Name:         in.Name,
		Tools:        in.Tools,
		Parent:       res,
		ParentCallID: agenttool.ToolCallIDFrom(ctx),
	}
	if in.MaxSteps > 0 {
		opts.MaxSteps = in.MaxSteps
	}
	if in.TimeoutSeconds > 0 {
		opts.Timeout = time.Duration(in.TimeoutSeconds) * time.Second
	}
	if name := strings.TrimSpace(in.Model); name != "" {
		if resolve == nil {
			return SpawnAgentOutput{}, fmt.Errorf("spawn_agent：这个部署不支持为子 agent 指定模型")
		}
		m, err := resolve(ctx, name)
		if err != nil {
			return SpawnAgentOutput{}, err
		}
		opts.Model = m
		// The nested loop sizes its context window from the model it runs on, so
		// an override travels with its name.
		opts.ModelName = name
	}

	report, err := spawner.Spawn(ctx, opts)
	if err != nil {
		return SpawnAgentOutput{}, err
	}
	out := SpawnAgentOutput{
		Report:      report.Text,
		Name:        report.Name,
		Steps:       report.Steps,
		Tools:       report.Tools,
		TotalTokens: report.Usage.TotalTokens,
		StopReason:  report.StopReason,
		Failed:      report.Failed,
	}
	out.Summary = fmt.Sprintf("子 agent「%s」跑完，%d tokens", report.Name, report.Usage.TotalTokens)
	if report.Failed {
		out.Summary = fmt.Sprintf("子 agent「%s」没有跑完", report.Name)
	}
	return out, nil
}

// spawnMany runs the tasks form: one subagent per task, in parallel.
func spawnMany(ctx context.Context, spawner Spawner, resolve agenttool.ModelResolver, in SpawnAgentInput) (SpawnAgentOutput, error) {
	res, ok := agenttool.TurnResourcesFrom(ctx)
	if !ok {
		return SpawnAgentOutput{}, fmt.Errorf(
			"spawn_agent：这一轮没有可用的模型或工具注册表，无法派生子 agent（这个工具只在完整的轮次里可用）")
	}
	callID := agenttool.ToolCallIDFrom(ctx)

	shared := subagent.Options{
		Tools:        in.Tools,
		Parent:       res,
		ParentCallID: callID,
	}
	if in.MaxSteps > 0 {
		shared.MaxSteps = in.MaxSteps
	}
	if in.TimeoutSeconds > 0 {
		shared.Timeout = time.Duration(in.TimeoutSeconds) * time.Second
	}
	if name := strings.TrimSpace(in.Model); name != "" {
		if resolve == nil {
			return SpawnAgentOutput{}, fmt.Errorf("spawn_agent：这个部署不支持为子 agent 指定模型")
		}
		m, err := resolve(ctx, name)
		if err != nil {
			return SpawnAgentOutput{}, err
		}
		shared.Model = m
	}

	briefs := make([]string, 0, len(in.Tasks))
	opts := make([]subagent.Options, 0, len(in.Tasks))
	for _, task := range in.Tasks {
		task = strings.TrimSpace(task)
		if task == "" {
			continue
		}
		briefs = append(briefs, task)
		// A copy per task: the fan-out mutates nothing shared, and giving each its
		// own value keeps that true if that ever changes.
		one := shared
		one.Prompt = task
		opts = append(opts, one)
	}
	if len(opts) == 0 {
		return SpawnAgentOutput{}, fmt.Errorf("spawn_agent: tasks 里没有非空的任务")
	}

	reports, err := spawner.SpawnMany(ctx, opts)
	if err != nil {
		return SpawnAgentOutput{}, err
	}

	out := SpawnAgentOutput{Reports: make([]SpawnAgentReport, 0, len(reports))}
	var combined strings.Builder
	failed, tokens := 0, 0
	for i, report := range reports {
		task := ""
		if i < len(briefs) {
			task = briefs[i]
		}
		if report.Failed {
			failed++
		}
		tokens += report.Usage.TotalTokens
		out.Reports = append(out.Reports, SpawnAgentReport{
			Task:        task,
			Name:        report.Name,
			Report:      report.Text,
			Steps:       report.Steps,
			Tools:       report.Tools,
			TotalTokens: report.Usage.TotalTokens,
			StopReason:  report.StopReason,
			Failed:      report.Failed,
		})
		// The combined text is what the model reads first, and it names each
		// subagent's task so the reports cannot be mixed up.
		fmt.Fprintf(&combined, "### 子 agent %d：%s\n\n%s\n\n", i+1, task, report.Text)
	}

	out.Report = strings.TrimRight(combined.String(), "\n")
	out.Name = fmt.Sprintf("%d 个并行子 agent", len(reports))
	out.Steps = -1
	out.TotalTokens = tokens
	out.Failed = failed > 0
	out.Summary = fmt.Sprintf("%d 个子 agent 并行跑完，共 %d tokens", len(reports), tokens)
	if failed > 0 {
		out.Summary = fmt.Sprintf("%d 个子 agent 并行跑完（%d 个没跑成），共 %d tokens", len(reports), failed, tokens)
	}
	return out, nil
}
