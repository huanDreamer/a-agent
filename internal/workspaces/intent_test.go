package workspaces

import (
	"testing"
)

// testNames is the workspace set the parser is tested against.
var testNames = []string{"home", "blog-site", "api-server", "电商项目"}

// TestParseSwitchIntent covers the phrasings that must switch, and — the half
// that matters more — the ordinary requests that merely mention a workspace or a
// tool and must be answered instead of acted on. A false positive here moves the
// agent into a different project mid-conversation, so each negative case is a
// request someone would really type.
func TestParseSwitchIntent(t *testing.T) {
	cases := []struct {
		text string
		want Intent
		why  string
	}{
		// --- switches that name a workspace that exists ---
		{"切换到 blog-site", Intent{IntentSwitch, "blog-site"}, "plain"},
		{"切换到 blog-site 工作区", Intent{IntentSwitch, "blog-site"}, "with the noun"},
		{"切到 blog-site", Intent{IntentSwitch, "blog-site"}, "short verb"},
		{"切至 blog-site", Intent{IntentSwitch, "blog-site"}, "formal verb"},
		{"换到 blog-site", Intent{IntentSwitch, "blog-site"}, "换到"},
		{"换成 blog-site", Intent{IntentSwitch, "blog-site"}, "换成"},
		{"改用 blog-site", Intent{IntentSwitch, "blog-site"}, "改用"},
		{"回到 blog-site", Intent{IntentSwitch, "blog-site"}, "回到"},
		{"设为 blog-site", Intent{IntentSwitch, "blog-site"}, "设为"},
		{"切换到 blog-site。", Intent{IntentSwitch, "blog-site"}, "sentence-final period"},
		{"切换到 blog-site！", Intent{IntentSwitch, "blog-site"}, "exclamation"},
		{"帮我切到 blog-site", Intent{IntentSwitch, "blog-site"}, "polite prefix"},
		{"请帮我切换到 blog-site 工作区", Intent{IntentSwitch, "blog-site"}, "two polite prefixes"},
		{"把工作区切到 blog-site", Intent{IntentSwitch, "blog-site"}, "把 work area to"},
		{"工作区切到 blog-site", Intent{IntentSwitch, "blog-site"}, "topic-first"},
		{"切换到 BLOG-SITE", Intent{IntentSwitch, "blog-site"}, "case-insensitive, canonicalised"},
		{"切换到 blog", Intent{IntentSwitch, "blog-site"}, "unique prefix"},
		{"切换到 电商项目", Intent{IntentSwitch, "电商项目"}, "chinese name"},
		{"用 电商项目 工作区", Intent{IntentSwitch, "电商项目"}, "ambiguous verb with the noun"},
		{"use blog-site", Intent{IntentSwitch, "blog-site"}, "english verb"},
		{"switch to blog-site", Intent{IntentSwitch, "blog-site"}, "english phrase"},
		{"切换到 home", Intent{IntentSwitch, "home"}, "another workspace"},
		{"切到 api-server 吧", Intent{IntentSwitch, "api-server"}, "trailing particle"},

		// --- switch requests whose name does not exist: answered, not guessed ---
		{"切换到 dianqi", Intent{IntentSwitch, "dianqi"}, "unambiguous verb, unknown name"},
		{"切到 myproj", Intent{IntentSwitch, "myproj"}, "unknown name after 切到"},
		{"切换到", Intent{IntentSwitch, ""}, "no name given"},
		{"切换工作区", Intent{IntentSwitch, ""}, "noun then nothing"},

		// --- list / current ---
		{"列出工作区", Intent{Kind: IntentList}, "list"},
		{"有哪些工作区", Intent{Kind: IntentList}, "which exist"},
		{"工作区列表", Intent{Kind: IntentList}, "list noun-first"},
		{"所有工作区", Intent{Kind: IntentList}, "all"},
		{"看看工作区", Intent{Kind: IntentList}, "look at"},
		{"工作区", Intent{Kind: IntentList}, "bare noun"},
		{"工作区？", Intent{Kind: IntentList}, "bare noun, question"},
		{"list workspaces", Intent{Kind: IntentList}, "english list"},
		{"workspaces", Intent{Kind: IntentList}, "bare english noun"},
		{"当前工作区", Intent{Kind: IntentCurrent}, "current"},
		{"我现在在哪个工作区", Intent{Kind: IntentCurrent}, "which one am I in"},
		{"工作区是什么", Intent{Kind: IntentCurrent}, "what is it"},
		{"which workspace", Intent{Kind: IntentCurrent}, "english current"},
		{"current workspace", Intent{Kind: IntentCurrent}, "english current phrase"},

		// --- requests that must NOT be read as a switch ---
		{"在工作区里建个 hello.go", Intent{Kind: IntentNone}, "create inside the workspace"},
		{"帮我看看工作区里的代码", Intent{Kind: IntentNone}, "read the code"},
		{"把工作区里的文件列出来", Intent{Kind: IntentNone}, "list the files, not the workspaces"},
		{"工作区里有哪些文件", Intent{Kind: IntentNone}, "files, not workspaces"},
		{"用 Python 写个脚本", Intent{Kind: IntentNone}, "use a language, not a workspace"},
		{"用 go 写一个爬虫", Intent{Kind: IntentNone}, "ambiguous verb, no name"},
		{"打开 main.go", Intent{Kind: IntentNone}, "open a file"},
		{"切换到 main.go 然后编译", Intent{Kind: IntentNone}, "long sentence after an unambiguous verb"},
		{"帮我在工作区里搜索 TODO", Intent{Kind: IntentNone}, "search"},
		{"今天天气怎么样", Intent{Kind: IntentNone}, "unrelated"},
		{"你好", Intent{Kind: IntentNone}, "greeting"},
		{"", Intent{Kind: IntentNone}, "empty"},
		{"   ", Intent{Kind: IntentNone}, "whitespace"},
		{"/workspace x", Intent{Kind: IntentNone}, "slash commands are the channel's"},
		{"/workspaces", Intent{Kind: IntentNone}, "slash list"},
	}

	for _, tc := range cases {
		got := ParseSwitchIntent(tc.text, testNames)
		if got != tc.want {
			t.Errorf("ParseSwitchIntent(%q) = {Kind:%q Name:%q}, want {Kind:%q Name:%q} (%s)",
				tc.text, got.Kind, got.Name, tc.want.Kind, tc.want.Name, tc.why)
		}
	}
}

// TestParseSwitchIntentNoNames: with only the built-in workspace known, a name
// that is not it must still be reported as an attempted switch, so the bot can
// answer "no such workspace" instead of silently ignoring the request.
func TestParseSwitchIntentNoNames(t *testing.T) {
	got := ParseSwitchIntent("切换到 blog", []string{"home"})
	if got.Kind != IntentSwitch || got.Name != "blog" {
		t.Errorf("got %+v, want a switch to blog", got)
	}
}

// TestResolveName pins the resolution order: an exact name always wins, a unique
// prefix or substring is accepted, and anything ambiguous is refused rather than
// guessed — switching to the wrong project is a mistake the model then acts on.
func TestResolveName(t *testing.T) {
	specs := []Spec{
		{Name: "home"},
		{Name: "blog-site"},
		{Name: "blog-docs"},
		{Name: "电商项目"},
	}

	cases := []struct {
		want  string
		match Match
	}{
		{"blog-site", MatchExact},
		{"BLOG-SITE", MatchExact},
		{"Blog-Site", MatchExact},
		{"home", MatchExact},
		{"电商项目", MatchExact},
		{"blog-", MatchAmbiguous}, // prefix of two
		{"blog", MatchAmbiguous},  // prefix of two
		{"电商", MatchFuzzy},        // unique prefix
		{"site", MatchFuzzy},      // unique substring
		{"ghost", MatchNone},      // nothing
		{"", MatchNone},           // empty
	}
	for _, tc := range cases {
		spec, match := ResolveName(specs, tc.want)
		if match != tc.match {
			t.Errorf("ResolveName(%q) match = %v, want %v", tc.want, match, tc.match)
			continue
		}
		switch tc.match {
		case MatchExact, MatchFuzzy:
			if spec.Name == "" {
				t.Errorf("ResolveName(%q) matched but returned an empty spec", tc.want)
			}
		default:
			if spec.Name != "" {
				t.Errorf("ResolveName(%q) returned %q for a non-match", tc.want, spec.Name)
			}
		}
	}
}

// TestResolveNameUniqueSubstringIsCaseInsensitive: the Feishu path lowercases
// what someone typed, so the comparison must not depend on the case of either side.
func TestResolveNameUniqueSubstringIsCaseInsensitive(t *testing.T) {
	specs := []Spec{{Name: "MyBigProject"}}
	if spec, match := ResolveName(specs, "bigproj"); match != MatchFuzzy || spec.Name != "MyBigProject" {
		t.Errorf("ResolveName(bigproj) = %q, %v; want the unique fuzzy match", spec.Name, match)
	}
}
