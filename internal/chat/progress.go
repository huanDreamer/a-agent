package chat

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/cloudwego/eino/schema"

	"github.com/huan/huan-agent/internal/tool"
)

// This file is the harness's answer to the failure mode that budgets cannot see.
//
// A turn can be well inside every limit — steps, tokens, wall clock — and still
// be going nowhere: a model that re-runs the same command, re-reads the same
// file, or re-derives the same design decision it already made twenty steps ago.
// Nothing about that is an error, so nothing in the stack above this fires; it
// just costs an hour and a lot of tokens, and the user gets "已达到本轮最大步数".
//
// Two mechanisms, both standard practice in coding agents:
//
//   - a loop guard, which notices repetition and no-progress *within* a turn and
//     steers the model — the same way a colleague would say "you already looked
//     at that file, is it time to write the change?" — before ending the turn as
//     a last resort;
//   - a turn ledger, which survives compaction. Compaction folds the middle of
//     the window into a summary, and the thing that gets lost first is the
//     *decisions*: the summary keeps what happened, not what was settled. So the
//     runner keeps its own record of the goal, the plan and the files already
//     touched, and re-injects it after every compression.
//
// Both are deliberately conservative. A guard that stops productive work is a
// worse failure than the loop it was watching for, so every uncertain signal is
// read as progress, and the guard steers several times before it stops.

// Defaults for the loop guard. They are exported so the config package and the
// console can name the same numbers, and they are set where a normal turn never
// reaches them: three identical calls, or twenty-five steps without a single
// change to anything.
const (
	// DefaultRepeatNudge is how many identical tool calls earn a steering
	// message.
	DefaultRepeatNudge = 3
	// DefaultRepeatStop is how many identical calls end the turn.
	DefaultRepeatStop = 5
	// DefaultIdleNudgeSteps is how many consecutive steps may change nothing
	// before the model is steered.
	DefaultIdleNudgeSteps = 25
	// DefaultIdleStopSteps is how many such steps end the turn.
	DefaultIdleStopSteps = 75
	// DefaultMaxSteers caps the steering messages one turn may carry.
	DefaultMaxSteers = 6
	// DefaultRereadNudge is how many times one file may be read before the model
	// is told it has been there already.
	DefaultRereadNudge = 5
	// DefaultToolResultMaxChars is the bound on one tool result as the model sees
	// it, when the deployment does not set one. It mirrors
	// context.DefaultToolResultMaxChars, which is where the config default comes
	// from; the two are asserted equal in the tests.
	DefaultToolResultMaxChars = 16000
)

// GuardConfig tunes the loop guard. The zero value is "on, with the defaults":
// a caller that does not configure it gets the protection, and Disable is the
// one field that switches it off.
type GuardConfig struct {
	// Disable turns the guard off entirely.
	Disable bool
	// RepeatNudge / RepeatStop are the identical-call thresholds. 0 uses the
	// defaults; Stop is raised to Nudge+1 when it is not above it, because a
	// stop the model was never warned about is the guard misfiring.
	RepeatNudge int
	RepeatStop  int
	// IdleNudgeSteps / IdleStopSteps are the no-progress thresholds, in steps.
	IdleNudgeSteps int
	IdleStopSteps  int
	// MaxSteers caps how many steering messages one turn may carry.
	MaxSteers int
	// RereadNudge is how many reads of one file earn a steering message.
	RereadNudge int
}

// guardRules is GuardConfig with every default filled in.
type guardRules struct {
	repeatNudge int
	repeatStop  int
	idleNudge   int
	idleStop    int
	maxSteers   int
	rereadNudge int
}

// rules resolves the configuration. The second result is false when the guard is
// switched off, which is a different thing from "no threshold was reached".
func (g GuardConfig) rules() (guardRules, bool) {
	if g.Disable {
		return guardRules{}, false
	}
	r := guardRules{
		repeatNudge: orDefault(g.RepeatNudge, DefaultRepeatNudge),
		repeatStop:  orDefault(g.RepeatStop, DefaultRepeatStop),
		idleNudge:   orDefault(g.IdleNudgeSteps, DefaultIdleNudgeSteps),
		idleStop:    orDefault(g.IdleStopSteps, DefaultIdleStopSteps),
		maxSteers:   orDefault(g.MaxSteers, DefaultMaxSteers),
		rereadNudge: orDefault(g.RereadNudge, DefaultRereadNudge),
	}
	if r.repeatStop <= r.repeatNudge {
		r.repeatStop = r.repeatNudge + 1
	}
	if r.idleStop <= r.idleNudge {
		// The stop has to come after at least one steer, or the model is ended
		// without ever being told what it was doing.
		r.idleStop = r.idleNudge * 3
	}
	return r, true
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// Steering kinds, as carried on EventSteer and shown in the console.
const (
	// SteerRepeat: the same tool call is being repeated.
	SteerRepeat = "repeat"
	// SteerIdle: steps are running without changing anything.
	SteerIdle = "idle"
	// SteerReread: one file is being read over and over.
	SteerReread = "reread"
)

// steer is the guard's verdict on one step: something to tell the model, and
// whether the turn is over.
type steer struct {
	// Kind is one of the Steer* constants. Empty means nothing to say.
	Kind string
	// Text is the message the model receives. It is written as an instruction,
	// because that is what it is: a correction of how the turn is being run.
	Text string
	// StopReason is non-empty when the turn should end here.
	StopReason string
	// Detail explains a stop in the answer's own words: which call was repeated,
	// how many steps changed nothing.
	Detail string
}

// empty reports whether the guard has nothing to say about this step.
func (s steer) empty() bool { return s.Text == "" && s.StopReason == "" }

// effect is what one tool call did to the world.
type effect int

const (
	// effectRead observed something and changed nothing.
	effectRead effect = iota
	// effectChange changed a file, ran a command that could, or moved the plan
	// forward.
	effectChange
	// effectNeutral is a call that neither reads nor changes: asking the user a
	// question, today. It resets the idle streak — a person was consulted, so the
	// turn has new information — without counting as progress.
	effectNeutral
)

// turnProgress watches one turn for repetition and for no-progress.
type turnProgress struct {
	rules guardRules

	// repeats counts identical tool calls, keyed by tool name and arguments. A
	// change to the world resets every counter: running the same test twice after
	// a fix is not a loop.
	repeats map[string]*repeatState
	// calls counts every tool call in the turn, which is what the repeat state
	// compares against.
	calls int
	// lastChange is the call index of the most recent change.
	lastChange int

	// reads counts file reads by path, and steeredPaths remembers which ones the
	// model has already been warned about — once per file, not once per reread.
	reads        map[string]int
	steeredPaths map[string]bool

	// repeatWarned records which repeated calls have already crossed the nudge
	// threshold, so a stop is never the first time the model hears about it.
	repeatWarned map[string]bool
	// repeatSteered records which ones have actually had a message written, so
	// the same complaint is not re-sent every step until the stop.
	repeatSteered map[string]bool

	// idle counts consecutive steps that changed nothing.
	idle int
	// idleWarned is whether this idle streak has crossed the nudge threshold;
	// idleSteered is whether a message has gone out for it.
	idleWarned  bool
	idleSteered bool

	// steers counts the steering messages already emitted, which the budget caps.
	steers int
}

// repeatState is one identical call's history.
type repeatState struct {
	count int
	// lastChangeAt is the value of turnProgress.lastChange when the call was
	// last seen. When the world has changed since, the count starts over.
	lastChangeAt int
}

// newTurnProgress builds the guard for one turn. A disabled guard is a nil
// progress, and every method below tolerates that: the alternative is a boolean
// checked at each of the half-dozen places that report to it.
func newTurnProgress(cfg GuardConfig) *turnProgress {
	rules, on := cfg.rules()
	if !on {
		return nil
	}
	return &turnProgress{
		rules:         rules,
		repeats:       map[string]*repeatState{},
		repeatWarned:  map[string]bool{},
		repeatSteered: map[string]bool{},
		reads:         map[string]int{},
		steeredPaths:  map[string]bool{},
	}
}

// observe records one step's tool calls and reports what, if anything, the model
// should be told.
//
// It is called once per step that ran tools, with the runs in the model's order.
func (p *turnProgress) observe(reg *tool.Registry, runs []ToolRun) steer {
	if p == nil {
		return steer{}
	}

	var (
		changed bool
		neutral bool
		// repeat is the most-repeated call of this step. Only calls that changed
		// nothing are counted at all — a repeated *read* is provably pointless
		// (nothing outside the window can have changed, or a change would have
		// been made), while a repeated command may legitimately be a re-run.
		repeat *repeatHit
	)
	for _, r := range runs {
		eff := classifyEffect(reg, r)
		switch eff {
		case effectChange:
			changed = true
		case effectNeutral:
			neutral = true
		}
		if eff != effectRead {
			continue
		}
		// Only calls that changed nothing are counted as repeats. A re-run of a
		// command that does something (a build, a test) after an edit is how work
		// is done, and counting it produced nudges the model was right to ignore.
		if hit := p.noteCall(r); hit != nil && (repeat == nil || hit.count > repeat.count) {
			repeat = hit
		}
		for _, ref := range readRefs(r) {
			p.reads[ref.readKey()]++
		}
	}

	switch {
	case changed, neutral:
		// A step that changed something — or asked the user a question, whose
		// answer is information the turn did not have — ends the idle streak.
		// A question is deliberately not progress: it must not reset the repeat
		// counters.
		p.idle = 0
		p.idleWarned = false
		p.idleSteered = false
	default:
		p.idle++
	}
	if p.stepChangedWorld(reg, runs) {
		// The world moved, so "the same call again" starts counting from one: a
		// test re-run after a fix is not a loop.
		p.lastChange++
	}

	// The idle nudge is emitted before the budget check and is not subject to it:
	// at most one per idle streak, and the idle stop is defined as "the model was
	// told and kept going". A budget that could suppress this message would make
	// the stop unreachable, which is a bug this guard has already had once.
	if p.idle >= p.rules.idleNudge && !p.idleSteered {
		p.idleSteered = true
		p.idleWarned = true
		return p.steer(steer{Kind: SteerIdle, Text: idleMessage(p.idle)})
	}

	// A repeated read is stopped only when the model has actually been told about
	// it. If the steering budget was spent on other problems, the message it is
	// owed is delivered now and the stop waits one more repetition: a turn ended
	// for a reason the model never heard is the guard misfiring.
	if repeat != nil && repeat.count >= p.rules.repeatStop {
		if !p.repeatWarned[repeat.key] {
			p.repeatWarned[repeat.key] = true
			p.repeatSteered[repeat.key] = true
			return p.steer(steer{
				Kind: SteerRepeat,
				Text: repeatMessage(findRun(runs, repeat), repeat.count),
			})
		}
		return steer{
			Kind:       SteerRepeat,
			Text:       repeatMessage(findRun(runs, repeat), repeat.count),
			StopReason: StopLoop,
			Detail:     repeatDetail(repeat, runs),
		}
	}
	if p.idle >= p.rules.idleStop && p.idleWarned {
		return steer{
			Kind:       SteerIdle,
			Text:       idleMessage(p.idle),
			StopReason: StopIdle,
			Detail:     fmt.Sprintf("连续 %d 步只查看、没有任何改动", p.idle),
		}
	}

	// The rest of the steering is optional, so it is what the budget caps: one
	// message per problem, not one per step, because the same sentence repeated
	// every step is noise the model learns to skip.
	if p.steers >= p.rules.maxSteers {
		return steer{}
	}
	if repeat != nil && repeat.count >= p.rules.repeatNudge && !p.repeatSteered[repeat.key] {
		p.repeatSteered[repeat.key] = true
		p.repeatWarned[repeat.key] = true
		return p.steer(steer{Kind: SteerRepeat, Text: repeatMessage(findRun(runs, repeat), repeat.count)})
	}
	if key, n := p.rereadHit(); n >= p.rules.rereadNudge {
		p.steeredPaths[key] = true
		return p.steer(steer{Kind: SteerReread, Text: rereadMessage(readFileKey(key), n)})
	}
	return steer{}
}

// stepChangedWorld reports whether this step actually changed something, which is
// what resets the repeat counters: a file read again after an edit is not a
// repeat, it is a re-read of something that may now say something else.
//
// Two properties matter. It only counts *successful* calls — a refused or failed
// write changed nothing, and counting it let a model stuck re-calling the same
// refused tool look busy to the guard forever. And it counts commands, not just
// file tools: a code generator, git apply or an in-place edit makes the world
// move just as much as write_file does.
func (p *turnProgress) stepChangedWorld(reg *tool.Registry, runs []ToolRun) bool {
	for _, r := range runs {
		if r.Err != "" {
			continue
		}
		if classifyEffect(reg, r) == effectChange {
			return true
		}
	}
	return false
}

// steer counts a steering message and returns it.
func (p *turnProgress) steer(s steer) steer {
	p.steers++
	return s
}

// noteCall counts one identical call and reports the repeat it is part of.
//
// Every call counts, including the ones that changed something: the counter is
// reset by a change to a file or the plan (see stepChangedWorld), so "the same
// command five times with nothing edited in between" is a loop whether the
// command reads or writes.
func (p *turnProgress) noteCall(r ToolRun) *repeatHit {
	if r.Name == "" {
		return nil
	}
	// Reading the plan back is not a repeat, however many times it happens: the
	// tool takes no arguments, so every call is identical by construction, and
	// the instructions actively tell a model that has lost track to call it. The
	// first version of this guard stopped a real turn on its fifth plan_read,
	// which is exactly the "guard that kills working turns" failure it is meant
	// to avoid.
	if r.Name == "plan_read" {
		return nil
	}
	p.calls++
	key := r.Name + "\x00" + normalizeArgs(r.Args)
	state, ok := p.repeats[key]
	if !ok {
		state = &repeatState{lastChangeAt: p.lastChange}
		p.repeats[key] = state
	}
	if state.lastChangeAt != p.lastChange {
		// The world has changed since this call was last made, so the count —
		// and with it the warning — starts over: the second streak deserves its
		// own steering message before it could ever be stopped.
		state.count = 0
		state.lastChangeAt = p.lastChange
		delete(p.repeatWarned, key)
		delete(p.repeatSteered, key)
	}
	state.count++
	return &repeatHit{key: key, count: state.count}
}

// rereadHit returns the most-read (file, part) that has not been warned about
// yet, as the read key and how many times it was read. The key is what the
// caller records as warned, so the bookkeeping and the lookup stay the same
// shape — marking the file while looking up the key meant the same file was
// warned about again for every way it was read.
func (p *turnProgress) rereadHit() (string, int) {
	bestKey, bestN := "", 0
	for key, n := range p.reads {
		if p.steeredPaths[key] || n < bestN {
			continue
		}
		if n > bestN || (n == bestN && key < bestKey) {
			bestKey, bestN = key, n
		}
	}
	return bestKey, bestN
}

// repeatHit is one repeated call, as far as the guard knows it.
type repeatHit struct {
	key   string
	count int
}

// findRun finds the run a repeat hit came from, for the message: the model needs
// to see the call, not a hash of it.
func findRun(runs []ToolRun, hit *repeatHit) ToolRun {
	if hit == nil {
		return ToolRun{}
	}
	for _, r := range runs {
		if r.Name+"\x00"+normalizeArgs(r.Args) == hit.key {
			return r
		}
	}
	return ToolRun{Name: strings.SplitN(hit.key, "\x00", 2)[0]}
}

func repeatDetail(hit *repeatHit, runs []ToolRun) string {
	r := findRun(runs, hit)
	return fmt.Sprintf("%s 被用完全相同的参数调用了 %d 次", describeCall(r), hit.count)
}

// describeCall renders one call for a message: the tool and the one argument
// that identifies it.
func describeCall(r ToolRun) string {
	if r.Name == "" {
		return "同一个工具"
	}
	arg := firstArg(r.Args)
	if arg == "" {
		return fmt.Sprintf("工具 %s", r.Name)
	}
	return fmt.Sprintf("%s(%s)", r.Name, arg)
}

// firstArg pulls one identifying value out of a tool call's JSON arguments.
func firstArg(args string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		return truncate(strings.TrimSpace(args), 80)
	}
	for _, key := range []string{"command", "path", "question", "pattern", "file_path", "task"} {
		if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
			return truncate(collapseSpaces(v), 80)
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return truncate(collapseSpaces(v), 80)
		}
	}
	return ""
}

// normalizeArgs makes two spellings of the same call compare equal: whitespace
// inside the JSON is the model's formatting, not part of the call.
func normalizeArgs(args string) string {
	return collapseSpaces(strings.TrimSpace(args))
}

func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Messages the model receives. They are written to be actionable in one reading:
// what it is repeating, and the two things it can do about it.

func repeatMessage(r ToolRun, n int) string {
	return fmt.Sprintf("[系统提醒] 你已经用完全相同的参数调用 %s %d 次了，结果不会再变。请换一条路："+
		"改掉参数或换方法继续推进；如果这一步已经得出结论，就把结论写下来进入下一步（plan_update）；"+
		"如果任务已经做完，直接给出最终回答。不要重复同一次调用。", describeCall(r), n)
}

func rereadMessage(file string, n int) string {
	return fmt.Sprintf("[系统提醒] 文件 %s（同一段内容）你已经读过 %d 次了，结果不会变。不要再整读它："+
		"需要哪一段就用 read_file 的 offset/limit 或 grep 精确定位；如果只是为了确认已经知道的事，直接进入下一步。", file, n)
}

func idleMessage(steps int) string {
	return fmt.Sprintf("[系统提醒] 最近 %d 步都只是查看（读文件、搜索、跑只读命令），没有任何改动、没有推进计划。"+
		"请先停下来判断：如果已经看清了就该动手改代码或写出结论，不要再扩大探索范围；"+
		"如果这本来就是只读任务，就直接给出最终回答；如果确实还缺信息，只查最关键的一处，然后立刻动手。", steps)
}

// classifyEffect decides what one tool call did.
//
// The registry's capability is the primary signal, and bash is the exception
// that makes this worth writing: bash declares CapExec because it *can* do
// anything, and a turn that only ever runs grep and sed through it is the exact
// failure this guard exists for. So a command is read back to what it actually
// is, and anything unrecognised counts as a change — the direction that keeps
// the guard quiet rather than the direction that stops work.
func classifyEffect(reg *tool.Registry, run ToolRun) effect {
	switch run.Name {
	case "ask_user":
		// Neither reading nor changing: the turn is waiting for an answer, and
		// the answer is new information. It ends an idle streak but is not
		// progress, so it must not reset the repeat counters either.
		return effectNeutral
	case "web_search", "web_fetch", "describe_image":
		// Looking something up *outside* the repository is the work of a research
		// turn. Counting it as idle would steer — and eventually stop — a turn
		// that is doing exactly what it was asked to do, and the failure this
		// guard exists for is a turn re-reading its own workspace, not one
		// reading the web.
		return effectNeutral
	case "plan_create", "plan_add", "plan_update":
		return effectChange
	case "plan_read":
		return effectRead
	case "spawn_agent":
		// A nested run is work even though the tool that spawns it only reads:
		// it declares CapRead because it changes nothing *here*, but a turn that
		// delegates is making progress.
		return effectChange
	case "save_document":
		// Declared CapRead upstream (it changes no file in the workspace) while
		// actually writing to the document store.
		return effectChange
	case "bash":
		if commandChangesNothing(argValue(run.Args, "command")) {
			return effectRead
		}
		return effectChange
	}
	if reg == nil {
		return effectChange
	}
	t, ok := reg.Get(run.Name)
	if !ok {
		// An unknown tool: the call failed, so the turn did not progress, but
		// guessing "read" here would let a stream of unknown calls look idle.
		return effectChange
	}
	// An undeclared capability is not a promise that the tool is harmless, and
	// the default (CapRead) is what every MCP tool gets — create_pr, push and
	// send_message included (see internal/mcp, which declares nothing). Trusting
	// the default would make a turn whose work happens through MCP look idle and
	// stop it, so an undeclared tool counts as work.
	if !declaresCapability(t) {
		return effectChange
	}
	switch tool.CapabilityOf(t) {
	case tool.CapWrite, tool.CapExec:
		return effectChange
	default:
		return effectRead
	}
}

// declaresCapability reports whether a tool says what it does at all.
func declaresCapability(t tool.Tool) bool {
	_, ok := t.(tool.Capable)
	return ok
}

// readOnlyVerbs are the commands that only look at the world.
//
// The list is a whitelist on purpose. A blacklist would have to name every way a
// shell can write — redirect, tee, xargs -I, find -exec, git checkout — and a
// missing entry would call a destructive command read-only. A missing entry here
// only costs an opportunity to steer.
var readOnlyVerbs = map[string]bool{
	"ls": true, "pwd": true, "cat": true, "head": true, "tail": true, "sed": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "ack": true,
	"find": true, "fd": true, "wc": true, "sort": true, "uniq": true, "cut": true,
	"awk": true, "gawk": true, "tr": true, "diff": true, "comm": true, "cmp": true,
	"tree": true, "file": true, "stat": true, "du": true, "df": true, "which": true,
	"whereis": true, "type": true, "echo": true, "printf": true, "date": true,
	"env": true, "printenv": true, "whoami": true, "uname": true, "hostname": true,
	"jq": true, "yq": true, "xxd": true, "od": true, "hexdump": true, "nl": true,
	"basename": true, "dirname": true, "realpath": true, "readlink": true,
	"seq": true, "true": true, "test": true, "column": true, "fold": true,
	"pgrep": true, "ps": true, "id": true, "uptime": true, "sw_vers": true,
}

// readOnlyGitSubcommands are the git subcommands that only read.
var readOnlyGitSubcommands = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true, "ls-files": true,
	"describe": true, "rev-parse": true, "blame": true, "shortlog": true,
	"grep": true, "cat-file": true, "name-rev": true, "whatchanged": true,
	"reflog": true, "annotate": true, "verify-commit": true, "merge-base": true,
}

// writingGitSubcommands are the subcommands that read or write depending on
// their arguments, so they are only treated as reads when a read-only flag is
// present. They are the reason this list exists rather than a plain whitelist:
// "git branch" lists, "git branch -D" deletes, and a guard that called the
// second read would both miscount idle steps and be willing to stop a turn that
// is doing real work.
var writingGitSubcommands = map[string]bool{
	"branch": true, "tag": true, "remote": true, "config": true, "note": true,
	"notes": true, "stash": true, "worktree": true, "submodule": true,
	"fetch": true, "gc": true, "prune": true, "update-ref": true, "symbolic-ref": true,
}

// readOnlyGitFlags are the flags that make one of the commands above a read.
var readOnlyGitFlags = []string{"--get", "--list", "-l", "--dry-run", "--show-current", "-v", "--porcelain", "--name-only", "--oneline"}

// mutatingTokens disqualify a command from being read-only wherever they appear.
//
// The list is only for things that write *wherever they sit*: a redirection, an
// -exec, an in-place editor, a git subcommand that moves the tree. Command names
// are deliberately absent — a command is judged by its leading verb, because the
// opposite rule misfires on paths: "cat internal/store/media.go" ends in ".go ",
// and a substring test for the go tool would call a plain read a build.
var mutatingTokens = []string{
	">", ">>", "tee ", "-exec", "-delete", "-execdir", "-ok ", "-fprint",
	"sed -i", "perl -i", "git add", "git commit", "git checkout", "git switch",
	"git restore", "git reset", "git apply", "git stash", "git push", "git pull",
	"git merge", "git rebase", "git clean", "git rm", "git mv", "git init",
	"sh -c", "bash -c", "zsh -c", "eval ", "source ",
}

// interpreterVerbs are the commands that can do anything at all, so a command
// whose leading verb is one of them is never a read.
var interpreterVerbs = map[string]bool{
	"python": true, "python3": true, "node": true, "deno": true, "bun": true,
	"ruby": true, "perl": true, "php": true, "go": true, "cargo": true,
	"make": true, "npm": true, "pnpm": true, "yarn": true, "pip": true,
	"pip3": true, "uv": true, "poetry": true, "docker": true, "kubectl": true,
	"curl": true, "wget": true, "ssh": true, "scp": true, "rsync": true,
	"kill": true, "pkill": true, "sudo": true, "install": true, "patch": true,
	"dd": true, "truncate": true, "rm": true, "rmdir": true, "mv": true,
	"cp": true, "ln": true, "touch": true, "mkdir": true, "chmod": true,
	"chown": true, "xargs": true, "nohup": true,
}

// commandChangesNothing reports whether a shell command only looks at the world.
//
// It is the classifier behind "this turn is idling". Every uncertain answer is
// false (i.e. "this changed something"), because a false "read" can end a
// working turn while a false "change" only means the guard says nothing.
func commandChangesNothing(command string) bool {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return false
	}
	// Everything below looks at the command's *structure*, so the contents of
	// quoted strings are blanked out first. Without this, the most common shape
	// in a real turn — grep with a pattern that contains a pipe or a redirect,
	// as in grep -n "a\|b" file.go — parses as a pipeline of nonsense verbs and
	// the command is called work. The bug is invisible in a hand-written test and
	// obvious the moment 881 real commands are replayed through it.
	structured := unquoted(cmd)
	// Redirections to /dev/null are the standard way to silence a read-only
	// command, so they are removed before the write check rather than counted as
	// writes.
	cleaned := devNullRedirect.ReplaceAllString(structured, "")
	for _, tok := range mutatingTokens {
		if strings.Contains(cleaned, tok) {
			return false
		}
	}
	for _, segment := range splitCommand(cleaned) {
		verb, rest := leadingVerb(segment)
		if verb == "" {
			continue
		}
		switch {
		case interpreterVerbs[verb]:
			// A command that can do anything is never read-only, however it is
			// spelled: this is what catches "cat x | python3 -c ...".
			return false
		case readOnlyVerbs[verb]:
		case verb == "git" && readOnlyGitSubcommands[firstWord(rest)]:
		case verb == "git" && writingGitSubcommands[firstWord(rest)] && hasReadFlag(rest):
		case verb == "cd" || verb == "true" || verb == ":":
			// Navigation and no-ops: neither read nor change, and they appear at
			// the head of almost every command a model writes.
		default:
			return false
		}
	}
	return true
}

// devNullRedirect matches a redirect to /dev/null, with or without a file
// descriptor: "2>/dev/null", ">/dev/null", "&>/dev/null".
var devNullRedirect = regexp.MustCompile(`\d?&?>>?\s*/dev/null`)

// unquoted blanks out the contents of every quoted span, keeping the quotes'
// boundaries as spaces.
//
// It is what makes the structural checks below safe to apply to a command a
// model wrote rather than to a sanitised one: a grep pattern may contain a pipe,
// a redirect or a semicolon, and none of those are the shell's.
func unquoted(cmd string) string {
	var b strings.Builder
	b.Grow(len(cmd))
	var quote rune
	escaped := false
	for _, r := range cmd {
		switch {
		case escaped:
			escaped = false
			if quote != 0 {
				b.WriteRune(' ')
				continue
			}
			b.WriteRune(r)
		case r == '\\':
			escaped = true
			if quote == 0 {
				b.WriteRune(r)
			}
		case quote != 0:
			if r == quote {
				quote = 0
			}
			b.WriteRune(' ')
		case r == '\'' || r == '"':
			quote = r
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// splitCommand cuts a compound command into its segments, so "cd x && grep y z"
// is judged by both halves.
func splitCommand(cmd string) []string {
	fields := strings.FieldsFunc(cmd, func(r rune) bool {
		switch r {
		case '&', '|', ';', '\n', '(', ')':
			return true
		}
		return false
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if s := strings.TrimSpace(f); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// leadingVerb returns the first word of a segment, skipping the env assignments
// and wrappers that commonly precede it.
func leadingVerb(segment string) (string, string) {
	fields := strings.Fields(segment)
	for i, f := range fields {
		if strings.Contains(f, "=") && !strings.HasPrefix(f, "-") && i == 0 {
			continue // FOO=bar cmd
		}
		if f == "time" || f == "env" || f == "nohup" || f == "sudo" {
			continue
		}
		rest := ""
		if i+1 < len(fields) {
			rest = strings.Join(fields[i+1:], " ")
		}
		return strings.Trim(f, `"'`), rest
	}
	return "", ""
}

// hasReadFlag reports whether one of the git commands that can write was asked to
// read instead.
func hasReadFlag(args string) bool {
	for _, flag := range readOnlyGitFlags {
		if strings.Contains(" "+args+" ", " "+flag) || strings.Contains(args, flag+" ") {
			return true
		}
	}
	return false
}

func firstWord(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// fileToken matches something that looks like a source file path, for the
// reread signal. It is intentionally narrow: a false path costs one steering
// message about a file the model may not have read.
var fileToken = regexp.MustCompile(`[\w./-]+\.(?:go|js|mjs|cjs|ts|tsx|vue|jsx|py|rb|rs|java|kt|c|h|cc|cpp|hpp|cs|php|swift|sh|sql|md|markdown|json|jsonl|ya?ml|toml|ini|cfg|conf|css|scss|html|txt|proto|mod|sum)\b`)

// readRef is one file read: the file, and which part of it was read.
//
// The variant is what keeps the reread rule honest. "Read this file six times"
// is the signal; "read six consecutive slices of a 5000-line file" is how a large
// file is read at all, and counting the second as the first produced a steering
// message telling the model to stop doing the thing it was doing correctly. So
// the count is per (file, part) — an offset/limit window, a grep pattern, a sed
// range — and the message still names the file.
type readRef struct {
	file    string
	variant string
}

// readKey is the identity a read is counted under.
func (r readRef) readKey() string { return r.file + "\x00" + r.variant }

// readFileKey splits a readKey back into its file, for messages.
func readFileKey(key string) string {
	if i := strings.IndexByte(key, 0); i >= 0 {
		return key[:i]
	}
	return key
}

// readRefs returns the files one call read, as far as they can be told from its
// arguments. Only file-reading tools and read-only commands contribute: a
// command that writes is not a read, whatever it also looked at.
func readRefs(run ToolRun) []readRef {
	switch run.Name {
	case "read_file":
		var in struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if err := json.Unmarshal([]byte(run.Args), &in); err != nil || strings.TrimSpace(in.Path) == "" {
			return nil
		}
		variant := ""
		if in.Offset != 0 || in.Limit != 0 {
			variant = fmt.Sprintf("%d+%d", in.Offset, in.Limit)
		}
		return []readRef{{file: path.Clean(in.Path), variant: variant}}
	case "list_dir":
		var in struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(run.Args), &in); err == nil && strings.TrimSpace(in.Path) != "" {
			return []readRef{{file: path.Clean(in.Path)}}
		}
		return nil
	case "grep":
		var in struct {
			Path    string `json:"path"`
			Pattern string `json:"pattern"`
		}
		if err := json.Unmarshal([]byte(run.Args), &in); err == nil && strings.TrimSpace(in.Path) != "" {
			// The pattern is part of the identity: searching one file for five
			// different things is five searches, not five reads of one file.
			return []readRef{{file: path.Clean(in.Path), variant: in.Pattern}}
		}
		return nil
	case "bash":
		cmd := argValue(run.Args, "command")
		if !commandChangesNothing(cmd) {
			return nil
		}
		// The ranges are part of the identity too: "sed -n '1,120p' f" and
		// "sed -n '121,240p' f" are one file read in two halves.
		variant := readRange(cmd)
		seen := map[string]bool{}
		out := make([]readRef, 0, 4)
		for _, m := range fileToken.FindAllString(cmd, -1) {
			f := path.Clean(m)
			key := readRef{file: f, variant: variant}.readKey()
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, readRef{file: f, variant: variant})
		}
		return out
	}
	return nil
}

// readRange picks the part of a command that says *which slice* of a file is
// being read: the range of a sed/awk program, or the count of head/tail.
func readRange(cmd string) string {
	if m := sedRange.FindStringSubmatch(cmd); m != nil {
		return m[1]
	}
	if m := headTailRange.FindStringSubmatch(cmd); m != nil {
		return m[1] + m[2]
	}
	return ""
}

// sedRange captures the address part of a sed script: a line number or a range,
// with or without -n and quotes. The capture is the address itself — a group
// around the optional -n would make every chunk of one file look identical,
// which is the bug this exists to avoid.
var sedRange = regexp.MustCompile(`(?:^|\s)(?:-n\s*)?['"]?(\d+(?:,\d+|,\$)?)p`)

// headTailRange matches "head -20" / "tail -n 40".
var headTailRange = regexp.MustCompile(`\b(head|tail)\b[^|;]*?-n?\s*(\d+)`)

// turnLedger is what a long turn must not forget when its window is compressed.
//
// Compaction keeps the recent turns and rolls the rest into a summary. The
// summary is written by a model reading the folded messages, and what survives
// that is what *happened*; what is lost first is what was *settled* — the design
// decision made at step 60, the file already rewritten, the question already
// answered by the user. A model that loses those re-derives them, which is how a
// twenty-step task becomes a five-hundred-step one.
//
// So the runner keeps its own small record — the goal, the plan, the files it
// has touched — and puts it back in the window after every compression. It is
// rendered from live state each time, so it cannot go stale, and it is
// deliberately short: it is re-sent on every step that follows.
type turnLedger struct {
	goal string

	planSummary   string
	planChecklist string

	writes   []string
	commands []string

	// steps is how many model iterations the ledger has seen, for the "阶段"
	// line: a reader (and the model) can tell a fresh turn from a long one.
	steps int
}

// ledgerPlanOutput mirrors the plan tools' own result shape (see
// builtin.PlanOutput). It is decoded rather than imported so this package does
// not depend on the tool implementations.
type ledgerPlanOutput struct {
	Summary   string `json:"summary"`
	Checklist string `json:"checklist"`
}

func newTurnLedger(goal string) *turnLedger {
	return &turnLedger{goal: truncate(collapseSpaces(goal), 400)}
}

// observe folds one step's tool calls into the ledger.
func (l *turnLedger) observe(runs []ToolRun) {
	if l == nil {
		return
	}
	l.steps++
	for _, run := range runs {
		switch run.Name {
		case "plan_create", "plan_add", "plan_update", "plan_read":
			var out ledgerPlanOutput
			if err := json.Unmarshal([]byte(run.Result), &out); err != nil {
				continue
			}
			if strings.TrimSpace(out.Summary) != "" {
				l.planSummary = truncate(collapseSpaces(out.Summary), 400)
			}
			if strings.TrimSpace(out.Checklist) != "" {
				l.planChecklist = truncate(out.Checklist, 1600)
			}
			continue
		}
		if run.Err != "" {
			continue
		}
		switch run.Name {
		case "write_file", "edit_file", "apply_patch", "rename_path", "move_path":
			if p := argValue(run.Args, "path", "file_path", "to"); p != "" && !contains(l.writes, p) {
				l.writes = append(l.writes, p)
			}
		case "bash":
			cmd := argValue(run.Args, "command")
			if cmd == "" || commandChangesNothing(cmd) {
				continue
			}
			if n := len(l.commands); n > 0 && l.commands[n-1] == cmd {
				continue
			}
			l.commands = append(l.commands, truncate(collapseSpaces(cmd), 140))
		}
	}
	// Bound both lists: a ledger that grows without limit is the window problem
	// it exists to solve.
	if len(l.writes) > 20 {
		l.writes = l.writes[len(l.writes)-20:]
	}
	if len(l.commands) > 6 {
		l.commands = l.commands[len(l.commands)-6:]
	}
}

// render is the ledger as a message for the model. Empty when there is nothing
// worth saying yet.
func (l *turnLedger) render() string {
	if l == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("【本轮台账】系统维护的这一轮状态，压缩上下文后依然有效。已经确定的事不要重新推导，已经读过的文件不要反复读：\n")
	if l.goal != "" {
		b.WriteString("- 用户要求：" + l.goal + "\n")
	}
	if l.planSummary != "" {
		b.WriteString("- 计划：" + l.planSummary + "\n")
	}
	if l.planChecklist != "" {
		b.WriteString("- 任务清单：\n" + l.planChecklist + "\n")
	}
	if len(l.writes) > 0 {
		b.WriteString("- 已改动文件：" + strings.Join(l.writes, "、") + "\n")
	} else {
		b.WriteString("- 已改动文件：无（还没有写任何文件）\n")
	}
	if len(l.commands) > 0 {
		b.WriteString("- 最近执行的命令：" + strings.Join(l.commands, " ； ") + "\n")
	}
	b.WriteString("- 已进行 " + fmt.Sprintf("%d", l.steps) + " 步。下一步请直接推进计划里未完成的那一条。")
	return b.String()
}

// argValue reads one string field out of a tool call's JSON arguments, trying
// each name in turn.
func argValue(args string, names ...string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		return ""
	}
	for _, name := range names {
		if v, ok := m[name].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// goalText is the request a ledger is built against: the most recent user
// message, which is what the turn is answering.
func goalText(messages []*schema.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m != nil && m.Role == schema.User && strings.TrimSpace(m.Content) != "" {
			return m.Content
		}
	}
	return ""
}

// withLedger puts the ledger into a window, right after the pinned head, so a
// compressed turn carries its own state instead of trusting the summary for it.
//
// When the ledger has nothing to say the window is returned unchanged: an empty
// block of headings on every step would be pure cost.
func withLedger(window []*schema.Message, head int, text string) []*schema.Message {
	if strings.TrimSpace(text) == "" {
		return window
	}
	if head < 0 {
		head = 0
	}
	if head > len(window) {
		head = len(window)
	}
	out := make([]*schema.Message, 0, len(window)+1)
	out = append(out, window[:head]...)
	out = append(out, &schema.Message{Role: schema.System, Content: text})
	out = append(out, window[head:]...)
	return out
}

// resolvedToolResultCap turns the configured bound into the one capToolResult
// applies: the default when unset, nothing at all when negative.
func resolvedToolResultCap(configured int) int {
	switch {
	case configured < 0:
		return 0
	case configured == 0:
		return DefaultToolResultMaxChars
	default:
		return configured
	}
}

// capToolResult bounds one tool result as the model sees it.
//
// The console and the stored turn keep the whole thing; this is only the copy
// that is replayed on every following step of the turn. Head and tail are both
// kept because a command's output is read from both ends: the head says what ran
// and on what, the tail carries the error or the summary, and the middle is what
// nobody reads.
//
// A result that fits is returned untouched, so the common case costs nothing.
func capToolResult(result string, max int) string {
	if max <= 0 || len(result) <= max {
		return result
	}
	head := max * 7 / 10
	tail := max - head
	if head < 0 {
		head = 0
	}
	if tail < 0 {
		tail = 0
	}
	dropped := len(result) - head - tail
	var b strings.Builder
	b.Grow(max + 320)
	b.WriteString(cutRunes(result, head))
	fmt.Fprintf(&b, "\n\n[结果过长] 已省略中间 %d 字符（原始 %d 字符）。会话记录里有完整输出；"+
		"需要中间那段用 read_file 的 offset/limit 或 grep 精确取值，不要重新整跑一遍同样的命令。\n\n",
		dropped, len(result))
	b.WriteString(tailRunes(result, tail))
	return b.String()
}

// cutRunes cuts s to at most n bytes without splitting a UTF-8 sequence.
func cutRunes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

// tailRunes is cutRunes from the end.
func tailRunes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8Start(s[cut]) {
		cut++
	}
	return s[cut:]
}
