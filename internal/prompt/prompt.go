// Package prompt holds the system prompt huan-agent gives its models.
//
// The prose lives in the markdown files beside this file and is embedded at
// build time. A prompt is edited far more often than the code around it, and a
// .md file keeps it readable — headings, lists, and the backticks a Go raw
// string cannot contain at all — instead of turning every edit into escape
// noise.
//
// The text is Chinese, which is also the language the agent answers in by
// default. That is not decoration: the tool descriptions, the skills section
// the server appends, and the console itself are Chinese, so a prompt written
// in one language on top of instructions written in another only makes the
// model guess which one it is being addressed in.
package prompt

import (
	_ "embed"
	"strings"
)

// Surfaces name the kind of place the model is answering in, because that is
// what changes the rules: whether markdown is rendered, whether a question can
// be put to a person as a card, and how long an answer may be before it stops
// being readable.
const (
	// SurfaceWeb is the web console: rendered markdown, uploadable
	// attachments, and the ask_user card.
	SurfaceWeb = "web"
	// SurfaceCLI is `huan-agent chat` in a terminal: plain text, no cards.
	SurfaceCLI = "cli"
	// SurfaceFeishu is the Feishu bot: answers are delivered as interactive
	// cards, in a chat that may be read on a phone.
	SurfaceFeishu = "feishu"
	// SurfaceRun is `huan-agent run`: one prompt, one answer, no terminal and no
	// person watching. It gets its own section because its rules are the
	// opposite of the interactive ones — nothing can be asked, so every
	// ambiguity has to be resolved and stated in the answer.
	SurfaceRun = "run"
)

//go:embed base.md
var baseMD string

//go:embed surface_web.md
var webMD string

//go:embed surface_cli.md
var cliMD string

//go:embed surface_feishu.md
var feishuMD string

//go:embed surface_run.md
var runMD string

// basePrompt is the base prompt with its trailing newline removed: the prompt
// is concatenated and compared in tests, so a stray blank line at the end is
// only ever a source of confusing diffs.
var basePrompt = strings.TrimSpace(baseMD)

// sections maps a surface to its own section, trimmed the same way.
var sections = map[string]string{
	SurfaceWeb:    strings.TrimSpace(webMD),
	SurfaceCLI:    strings.TrimSpace(cliMD),
	SurfaceFeishu: strings.TrimSpace(feishuMD),
	SurfaceRun:    strings.TrimSpace(runMD),
}

// Base returns the surface-independent prompt: who the agent is, how it talks,
// how it works, how it uses its tools, and where its boundaries are.
func Base() string { return basePrompt }

// For returns the system prompt for one surface: the base prompt followed by
// the section describing what that surface can and cannot do.
//
// An unknown or empty surface gets the base prompt alone. That is the right
// default rather than an error: a surface nobody has written rules for still
// needs a competent agent, and the base prompt already tells the model that its
// tool list is its capability boundary — so a missing section cannot make it
// claim a capability the surface does not have.
func For(surface string) string {
	section, ok := sections[strings.ToLower(strings.TrimSpace(surface))]
	if !ok || section == "" {
		return basePrompt
	}
	return basePrompt + "\n\n" + section
}

// Effective returns the prompt a turn should actually use: the operator's
// override when they set one, otherwise the surface's default.
//
// The override replaces the built-in prompt rather than adding to it, and it
// replaces the surface's section with it. That is what "this deployment's
// prompt" has to mean to be useful — a section about cards and tables appended
// to someone's carefully written instructions is second-guessing them — and it
// is one rule for every surface, so an operator's prompt behaves the same on the
// console and in a chat. A blank or whitespace-only value is not an override: it
// is the absence of one, which is what an empty config field means.
func Effective(override, surface string) string {
	if p := strings.TrimSpace(override); p != "" {
		return p
	}
	return For(surface)
}
