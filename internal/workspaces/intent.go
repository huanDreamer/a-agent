package workspaces

import (
	"strings"
	"unicode"
)

// IntentKind classifies what a message is asking about workspaces.
type IntentKind string

const (
	// IntentNone means the message is not about workspaces at all, and must be
	// answered as an ordinary request.
	IntentNone IntentKind = ""
	// IntentSwitch means the message asks to work in a workspace. Name is what
	// was asked for, as typed; it may be empty (no name was given) or name
	// something that does not exist (the caller answers with the list).
	IntentSwitch IntentKind = "switch"
	// IntentList means the message asks which workspaces exist.
	IntentList IntentKind = "list"
	// IntentCurrent means the message asks which workspace is active.
	IntentCurrent IntentKind = "current"
)

// Intent is the parsed result of one message.
type Intent struct {
	Kind IntentKind
	// Name is the requested workspace name. It is the caller's job to resolve
	// it (see ResolveName) — the parser only decides whether this is a switch
	// request and what name was asked for.
	Name string
}

// Match describes how well a requested name resolved against the known set.
type Match int

const (
	// MatchNone means nothing matched.
	MatchNone Match = iota
	// MatchExact means the name matched a workspace exactly (ignoring case).
	MatchExact
	// MatchFuzzy means the name matched exactly one workspace as a prefix or a
	// substring, which is how "blog" finds "blog-site".
	MatchFuzzy
	// MatchAmbiguous means several workspaces matched; a guess here would be a
	// coin flip, so the caller must ask instead.
	MatchAmbiguous
)

// maxIntentRunes bounds a switch sentence. A long message is a request that
// happens to mention a workspace, not an instruction to move — and treating it
// as one would move the agent off the project it is working on mid-sentence.
const maxIntentRunes = 40

// maxUnknownNameRunes bounds the name of an unresolved switch. A short name
// after an unambiguous verb ("切换到 dianqi") is a typo or a workspace that does
// not exist yet and deserves an answer; a long remainder is a sentence.
const maxUnknownNameRunes = 24

// switchVerb is one way of asking to switch, and whether the verb alone is
// unambiguous enough that whatever follows it must be a workspace name.
type switchVerb struct {
	text string
	// unambiguous reports that the verb has no other common use in a request to
	// the agent, so what follows it can be read as a workspace name even when no
	// workspace matches. "切换到 x" is unambiguous; "用 x" is not.
	unambiguous bool
}

// switchVerbs are matched longest-first, so "切换到" wins over "切换".
var switchVerbs = []switchVerb{
	{text: "切换到", unambiguous: true},
	{text: "切换至", unambiguous: true},
	{text: "切到", unambiguous: true},
	{text: "切至", unambiguous: true},
	{text: "换到", unambiguous: true},
	{text: "换成", unambiguous: true},
	{text: "改用", unambiguous: true},
	{text: "回到", unambiguous: true},
	{text: "设为", unambiguous: true},
	{text: "切换", unambiguous: true},
	{text: "switch to", unambiguous: true},
	{text: "switch", unambiguous: true},
	{text: "进入", unambiguous: false},
	{text: "打开工作区", unambiguous: true},
	{text: "用", unambiguous: false},
	{text: "use", unambiguous: false},
}

// workspaceNouns are the words a person appends to say "the workspace named
// ...". They are stripped from the end of a candidate name, so both "切到 blog"
// and "切到 blog 工作区" name blog.
var workspaceNouns = []string{"工作区", "工作目录", "workspace", "workspaces"}

// politePrefixes are dropped before a verb is looked for: they carry no meaning
// here, and requiring the verb to be the first thing in the message would refuse
// the ordinary way people ask ("帮我切到 blog").
var politePrefixes = []string{"帮忙", "麻烦", "帮我", "给我", "请", "能不能", "可以", "能否"}

// trailingParticles are dropped from the end of a name candidate: they are
// politeness or sentence-final, not part of a name.
var trailingParticles = []string{"呢", "吧", "啊", "了", "谢谢", "多谢", "please"}

// listExact and currentExact are the question forms recognised in full, after
// politeness and trailing particles are stripped.
//
// They are matched in full rather than by containment on purpose. "帮我看看工作区
// 里的代码" contains "看看工作区" but asks for the code, not the list: acting on it
// would answer a question nobody asked. A phrasing that is not listed here falls
// through and is simply answered by the model — which is the safe direction, and
// /workspace is always available as the unambiguous way to ask.
var (
	listExact = []string{
		"工作区", "工作区列表", "列出工作区", "有哪些工作区", "哪些工作区",
		"所有工作区", "全部工作区", "有几个工作区", "几个工作区", "看看工作区",
		"查看工作区", "显示工作区", "工作区有哪些", "工作区都有哪些", "都有哪些工作区",
		"我有哪些工作区", "我有几个工作区",
		"workspaces", "list workspaces", "show workspaces", "ls workspaces",
		"workspace list", "list all workspaces",
	}
	currentExact = []string{
		"当前工作区", "现在的工作区", "目前的工作区", "哪个工作区", "什么工作区",
		"在哪个工作区", "工作区是什么", "工作区是哪个", "工作区是哪一个",
		"current workspace", "which workspace", "which workspace am i in",
		"what is the current workspace",
	}
	// listVerbs are heads that, before a trailing workspace noun, make a listing
	// request ("列出" + "所有工作区").
	listVerbs = []string{
		"列出", "有哪些", "都有哪些", "所有", "全部", "看看", "查看", "显示",
		"我有哪些", "我有几个", "有几个", "几个",
		"list", "show", "ls", "list all", "show all",
	}
	// currentSuffixes are endings that make a request a question about which
	// workspace is active, whatever precedes them ("我现在在哪个工作区").
	currentSuffixes = []string{"哪个工作区", "哪一个工作区", "什么工作区", "哪个工作目录"}
)

// ParseSwitchIntent classifies a message against the workspaces that exist.
//
// `names` is what makes the parser safe in both directions. A name that exists
// can be recognised loosely, so the many natural ways of naming it work. A name
// that does not exist is only read as a workspace name after an unambiguous verb
// and only when it is a single short token — because "在工作区里建个 hello.go"
// and "用 Python 写个脚本" mention workspaces and tools but are not instructions
// to move the agent somewhere else, and acting on them would silently change
// where its next writes land.
//
// Slash commands are not parsed here: the channel's own command handler owns
// them, so that "/workspace x" keeps one meaning.
func ParseSwitchIntent(text string, names []string) Intent {
	s := trimSentence(text)
	if s == "" || strings.HasPrefix(s, "/") {
		return Intent{Kind: IntentNone}
	}

	// Questions first: "有哪些工作区" would otherwise be read as a name after a
	// verb, and "哪个工作区" is a question rather than a switch.
	if kind := classifyQuestion(s); kind != IntentNone {
		return Intent{Kind: kind}
	}

	body := strings.ToLower(s)
	// "把工作区切到 x" / "工作区切到 x": drop a leading noun, since the question
	// forms were already handled above.
	if rest, ok := cutLeadingNoun(body); ok {
		body = rest
	}
	body = cutLeadingPolite(body)

	for _, v := range switchVerbs {
		if !strings.HasPrefix(body, v.text) {
			continue
		}
		rest := strings.TrimSpace(body[len(v.text):])
		// "切换到" leaves "到 x" only when the verb was the shorter "切换".
		rest = cutLeadingFiller(rest)
		name, hadNoun := cutTrailingNoun(rest)
		name = strings.TrimSpace(cutTrailingParticles(name))
		if name == "" {
			// A switch with no name: the caller answers with the current
			// workspace and the list rather than guessing.
			return Intent{Kind: IntentSwitch}
		}
		if len([]rune(s)) > maxIntentRunes {
			// Too long to be an instruction to move; it is a request that
			// mentions a workspace.
			return Intent{Kind: IntentNone}
		}
		if resolved, ok := matchName(names, name); ok {
			return Intent{Kind: IntentSwitch, Name: resolved}
		}
		// Nothing matched: only a single short token after an unambiguous verb
		// (or after an explicit "工作区") is still read as a workspace name.
		single := !strings.ContainsFunc(name, unicode.IsSpace)
		if single && len([]rune(name)) <= maxUnknownNameRunes && (v.unambiguous || hadNoun) {
			return Intent{Kind: IntentSwitch, Name: name}
		}
		return Intent{Kind: IntentNone}
	}
	return Intent{Kind: IntentNone}
}

// ResolveName finds the workspace a request meant.
//
// The order is deliberate: an exact name wins over a prefix, a unique prefix or
// substring is accepted so a shortened name still works, and anything ambiguous
// is refused rather than guessed — switching to the wrong project is a mistake
// the model would then act on, not a cosmetic one.
func ResolveName(specs []Spec, want string) (Spec, Match) {
	want = strings.TrimSpace(want)
	if want == "" {
		return Spec{}, MatchNone
	}
	for _, s := range specs {
		if strings.EqualFold(s.Name, want) {
			return s, MatchExact
		}
	}
	var (
		matches []Spec
	)
	wantLow := strings.ToLower(want)
	for _, s := range specs {
		name := strings.ToLower(s.Name)
		if strings.HasPrefix(name, wantLow) || strings.Contains(name, wantLow) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
		return Spec{}, MatchNone
	case 1:
		return matches[0], MatchFuzzy
	default:
		return Spec{}, MatchAmbiguous
	}
}

// matchName resolves a name candidate against bare names, for the parser, which
// only needs to decide whether the request named something that exists.
func matchName(names []string, want string) (string, bool) {
	wantLow := strings.ToLower(strings.TrimSpace(want))
	if wantLow == "" {
		return "", false
	}
	for _, n := range names {
		if strings.ToLower(n) == wantLow {
			return n, true
		}
	}
	var found string
	count := 0
	for _, n := range names {
		low := strings.ToLower(n)
		if strings.HasPrefix(low, wantLow) || strings.Contains(low, wantLow) {
			found = n
			count++
		}
	}
	if count == 1 {
		return found, true
	}
	return "", false
}

// trimSentence removes surrounding whitespace and sentence-final punctuation.
func trimSentence(s string) string {
	return strings.TrimSpace(strings.TrimFunc(s, func(r rune) bool {
		switch r {
		case '。', '！', '？', '!', '?', '~', '～', '，', ',', '、', ' ', '\t', '\n', '\r':
			return true
		}
		return false
	}))
}

// classifyQuestion reports whether the message asks about workspaces rather than
// asking to change one.
func classifyQuestion(s string) IntentKind {
	// Normalise the same way the switch path does, so "看看工作区吧" and
	// "帮我列出工作区" are recognised as the questions they are.
	q := strings.ToLower(strings.TrimSpace(cutTrailingParticles(trimSentence(cutLeadingPolite(strings.ToLower(s))))))
	if q == "" {
		return IntentNone
	}
	for _, form := range currentExact {
		if q == form {
			return IntentCurrent
		}
	}
	for _, suffix := range currentSuffixes {
		if strings.HasSuffix(q, suffix) {
			return IntentCurrent
		}
	}
	for _, form := range listExact {
		if q == form {
			return IntentList
		}
	}
	// "<list verb> <workspace noun>": anchored at both ends, so
	// "列出工作区里所有文件" is not a listing request.
	for _, noun := range workspaceNouns {
		if !strings.HasSuffix(q, noun) {
			continue
		}
		head := strings.TrimSpace(strings.TrimSuffix(q, noun))
		for _, verb := range listVerbs {
			if head == verb {
				return IntentList
			}
		}
	}
	return IntentNone
}

// cutLeadingNoun drops a leading "把工作区" / "工作区" so the verb behind it can
// be found.
func cutLeadingNoun(s string) (string, bool) {
	for _, prefix := range []string{"把", "将"} {
		for _, n := range workspaceNouns {
			if strings.HasPrefix(s, prefix+n) {
				return strings.TrimSpace(s[len(prefix)+len(n):]), true
			}
		}
	}
	for _, n := range workspaceNouns {
		if strings.HasPrefix(s, n) {
			return strings.TrimSpace(s[len(n):]), true
		}
	}
	return s, false
}

// cutLeadingPolite drops a leading politeness marker.
func cutLeadingPolite(s string) string {
	for {
		trimmed := s
		for _, p := range politePrefixes {
			if strings.HasPrefix(trimmed, p) {
				trimmed = strings.TrimSpace(trimmed[len(p):])
				break
			}
		}
		if trimmed == s {
			return s
		}
		s = trimmed
	}
}

// cutLeadingFiller drops a filler between the verb and the name ("切换 到 x").
func cutLeadingFiller(s string) string {
	for {
		trimmed := s
		for _, f := range []string{"到", "至", "为", "to ", "to"} {
			if strings.HasPrefix(trimmed, f) {
				trimmed = strings.TrimSpace(trimmed[len(f):])
				break
			}
		}
		if trimmed == s {
			return s
		}
		s = trimmed
	}
}

// cutTrailingNoun removes a trailing workspace noun and reports whether there
// was one. The flag matters: it is what lets an ambiguous verb ("用") still be
// read as a switch when the person said the word "工作区" explicitly.
func cutTrailingNoun(s string) (string, bool) {
	s = strings.TrimSpace(s)
	for {
		trimmed := strings.TrimSpace(cutTrailingParticles(s))
		matched := false
		for _, n := range workspaceNouns {
			if strings.HasSuffix(trimmed, n) {
				trimmed = strings.TrimSpace(trimmed[:len(trimmed)-len(n)])
				matched = true
				break
			}
		}
		if !matched {
			return trimmed, trimmed != s
		}
		s = trimmed
		if s == "" {
			return "", true
		}
	}
}

// cutTrailingParticles removes sentence-final particles.
func cutTrailingParticles(s string) string {
	for {
		trimmed := s
		for _, p := range trailingParticles {
			if strings.HasSuffix(trimmed, p) {
				trimmed = strings.TrimSpace(trimmed[:len(trimmed)-len(p)])
				break
			}
		}
		if trimmed == s {
			return s
		}
		s = trimmed
	}
}
