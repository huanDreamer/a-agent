package prompt

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The prompt is the one instruction every surface shares and the model reads
// before anything else, so these tests pin what was deliberate about it instead
// of its wording: the language it is written in, the rules that were learnt from
// failures (the missing terminal above all), and the fact that a surface's
// section extends the base prompt rather than replacing it.

func TestBaseIsTheGeneralPurposeAgentPrompt(t *testing.T) {
	base := Base()
	if !strings.Contains(base, "huan-agent") {
		t.Errorf("the base prompt does not name the agent: %s", base)
	}
	// The sections are the table of contents a reader (and a model) navigates
	// by; losing one means a whole area of behaviour went undocumented.
	for _, want := range []string{
		"## 语言与表达",
		"## 做事方式",
		"## 工具使用",
		"## 边界与安全",
	} {
		if !strings.Contains(base, want) {
			t.Errorf("the base prompt is missing the %q section", want)
		}
	}
	// The tools the prompt tells the model to reach for. A rename that is not
	// reflected here leaves the model asking for a tool that no longer exists.
	for _, want := range []string{
		"read_file", "write_file", "edit_file", "list_dir",
		"glob", "grep", "bash_background", "bash_jobs", "bash_output", "bash_stop",
		"skill", "time",
	} {
		if !strings.Contains(base, want) {
			t.Errorf("the base prompt never mentions the %s tool", want)
		}
	}
}

// TestBaseCoversTheMissingTerminal pins the instruction that was added because a
// model kept learning it by failing: commands run with stdin at /dev/null, so
// the prompt has to say so *and* name the way out. A model that only finds out
// when a command hangs burns the turn on a prompt nobody can answer.
func TestBaseCoversTheMissingTerminal(t *testing.T) {
	base := Base()
	for _, want := range []string{"/dev/null", "非交互", "-y", "CI=1", "git commit -m"} {
		if !strings.Contains(base, want) {
			t.Errorf("the base prompt is missing %q", want)
		}
	}
}

func TestForAddsTheSurfaceSection(t *testing.T) {
	base := Base()
	for _, surface := range []string{SurfaceWeb, SurfaceCLI, SurfaceFeishu} {
		got := For(surface)
		if !strings.HasPrefix(got, base) {
			t.Errorf("For(%q) does not start with the base prompt", surface)
		}
		if !strings.Contains(got, "## 当前环境") {
			t.Errorf("For(%q) has no environment section: %s", surface, got)
		}
		if len(got) <= len(base) {
			t.Errorf("For(%q) adds nothing to the base prompt", surface)
		}
	}

	// What is true of one surface is not true of another: the console is the
	// only surface with the ask_user card, and a section that says so is the
	// point of having sections at all.
	web := For(SurfaceWeb)
	if !strings.Contains(web, "ask_user") {
		t.Error("the web section does not describe the ask_user card")
	}
	for _, surface := range []string{SurfaceCLI, SurfaceFeishu} {
		if !strings.Contains(For(surface), "ask_user 不存在") {
			t.Errorf("the %s section does not say that ask_user is unavailable there", surface)
		}
	}
	if !strings.Contains(For(SurfaceFeishu), "飞书") {
		t.Error("the Feishu section does not name the platform it is written for")
	}
	if !strings.Contains(For(SurfaceCLI), "终端") {
		t.Error("the CLI section does not describe the terminal it answers in")
	}
}

func TestForIsForgivingAboutTheSurfaceName(t *testing.T) {
	// The name travels from a caller, so case and stray whitespace must not
	// decide whether the model is told where it is answering.
	if got := For("  WEB "); got != For(SurfaceWeb) {
		t.Errorf("For(\"  WEB \") = %q, want the web prompt", got)
	}
	// An unknown surface gets the base prompt rather than nothing: a surface
	// nobody has written rules for still needs a competent agent.
	for _, surface := range []string{"", "   ", "telegram"} {
		if got := For(surface); got != Base() {
			t.Errorf("For(%q) = %q, want the base prompt", surface, got)
		}
	}
}

func TestEffectivePrefersTheOperatorsPrompt(t *testing.T) {
	// An override replaces the built-in prompt, the surface section included:
	// that is what makes it the operator's prompt rather than a suggestion.
	got := Effective("  你是一个只回答天气的机器人  ", SurfaceWeb)
	if want := "你是一个只回答天气的机器人"; got != want {
		t.Errorf("Effective = %q, want %q", got, want)
	}

	// A blank override is the absence of one rather than an empty prompt: that
	// is what an unset config field means, and an empty system message would
	// leave the model with no rules at all.
	for _, blank := range []string{"", "   ", "\n\t "} {
		if got := Effective(blank, SurfaceFeishu); got != For(SurfaceFeishu) {
			t.Errorf("Effective(%q) = %q, want the Feishu default", blank, got)
		}
	}
}

// englishProse are English function words with their surrounding spaces. Tool
// names, flags and paths are ASCII by nature and expected here; English grammar
// is not, because a prompt whose rules are Chinese half-translated into English
// leaves the model weighing two sets of instructions. A section that is added in
// English fails this test, which is the intent.
var englishProse = []string{
	" the ", " and ", " is ", " are ", " you ", " with ", " should ", " must ",
	" when ", " for the ", "The ", "This ", "Your ",
}

func TestPromptHasNoLeftoverEnglishProse(t *testing.T) {
	for _, surface := range []string{"", SurfaceWeb, SurfaceCLI, SurfaceFeishu} {
		text := For(surface)
		for _, word := range englishProse {
			if strings.Contains(text, word) {
				t.Errorf("the prompt for %q contains the English prose %q", surface, word)
			}
		}
		// The measure that catches a section written in another language
		// wholesale: Chinese text is overwhelmingly Han characters, and no
		// amount of tool names pulls it near half.
		if ratio := hanRatio(text); ratio < 0.5 {
			t.Errorf("the prompt for %q is only %.0f%% Han characters", surface, ratio*100)
		}
	}
}

// TestPromptStaysSmallEnoughToPinEveryStep guards the budget rather than the
// prose: context compression keeps the system prompt verbatim on every step of a
// turn, so its size is a per-step cost multiplied by the step cap. A prompt that
// triples silently would be paid on every one of sixty steps.
func TestPromptStaysSmallEnoughToPinEveryStep(t *testing.T) {
	const limit = 12 << 10 // 12 KiB, roughly twice the current prompt
	for _, surface := range []string{"", SurfaceWeb, SurfaceCLI, SurfaceFeishu} {
		if got := len(For(surface)); got > limit {
			t.Errorf("the prompt for %q is %d bytes, over the %d-byte budget", surface, got, limit)
		}
	}
}

// hanRatio is the share of the prompt's characters that are Han ideographs.
func hanRatio(text string) float64 {
	total := utf8.RuneCountInString(text)
	if total == 0 {
		return 0
	}
	var han int
	for _, r := range text {
		if r >= 0x4E00 && r <= 0x9FFF {
			han++
		}
	}
	return float64(han) / float64(total)
}
