package llm

import (
	"testing"

	"github.com/huan/huan-agent/internal/store"
)

// TestParseWindowAnswer pins the shapes a model actually answers this question
// in. Refusing any of them would throw away the only available source for a
// provider that publishes no window of its own.
func TestParseWindowAnswer(t *testing.T) {
	cases := map[string]int{
		"128000":     128_000,
		"  128000  ": 128_000,
		"128,000":    128_000,
		"我的上下文窗口是 128000 token。": 128_000,
		"131072 tokens":     131_072,
		"1M":                1_000_000,
		"128K":              128_000,
		"上下文窗口为 200K token": 200_000,
		"100万":              1_000_000,
		"**128000**":        128_000,
		"1,048,576":         1_048_576,
		"我是 Claude，上下文窗口 200000 token": 200_000,
		// The model does not know, or does not want to answer.
		"0":           0,
		"":            0,
		"我不知道":        0,
		"抱歉，我无法提供该信息": 0,
		// Implausible values are refused rather than recorded: a parameter
		// count, a date or a version number is not a window.
		"7":             0,
		"1000000000":    0,
		"2026-09-19":    0,
		"gpt-4":         0,
		"我的窗口是 0 token": 0,
	}
	for reply, want := range cases {
		if got := ParseWindowAnswer(reply); got != want {
			t.Errorf("ParseWindowAnswer(%q) = %d, want %d", reply, got, want)
		}
	}
}

/* ---------------------------------------------------------- model facts --- */

// TestParseModelFacts pins the answer shapes a model actually produces. The rule
// the whole design rests on: a field that is absent or null is *not* false.
func TestParseModelFacts(t *testing.T) {
	facts := ParseModelFacts(`{"context_window": 128000, "text": true, "vision": false,
		"tools": true, "tts": null, "asr": "no", "image_gen": "不支持"}`)
	if !facts.Answered {
		t.Fatal("a JSON object was not recognised as an answer")
	}
	if facts.ContextWindow != 128_000 {
		t.Errorf("window = %d", facts.ContextWindow)
	}
	for cap, want := range map[store.Capability]bool{
		store.CapChat: true, store.CapVision: false, store.CapTools: true,
		store.CapAudioTranscribe: false, store.CapImageGen: false,
	} {
		got, definite := facts.Fact(cap)
		if !definite || got != want {
			t.Errorf("%s = (%v, definite=%v), want (%v, true)", cap, got, definite, want)
		}
	}
	// The central rule: null (and a field that is simply absent) is unknown, not
	// false. Recording it as false would silently strip a model of something it
	// can do.
	for _, unknown := range []store.Capability{store.CapEmbedding, store.CapAudioSpeech} {
		if _, definite := facts.Fact(unknown); definite {
			t.Errorf("%s was recorded as an answer, but the model did not give one", unknown)
		}
	}

	cases := []struct {
		name     string
		reply    string
		answered bool
		window   int
	}{
		{"fenced", "```json\n{\"context_window\": 200000}\n```", true, 200_000},
		{"preamble", "当然，我的参数如下：\n{\"context_window\": \"128K\"}", true, 128_000},
		{"null window", `{"context_window": null, "text": true}`, true, 0},
		{"prose only", "我是 Claude，上下文窗口 200000 token。", false, 200_000},
		{"refusal", "抱歉，我无法提供该信息。", false, 0},
		{"empty", "", false, 0},
	}
	for _, tc := range cases {
		got := ParseModelFacts(tc.reply)
		if got.Answered != tc.answered {
			t.Errorf("%s: Answered = %v, want %v", tc.name, got.Answered, tc.answered)
		}
		if got.ContextWindow != tc.window {
			t.Errorf("%s: window = %d, want %d", tc.name, got.ContextWindow, tc.window)
		}
	}

	// Synonyms: models answer with their own vocabulary.
	syn := ParseModelFacts(`{"function_calling": true, "stt": true, "text_to_speech": false, "image_input": true}`)
	for cap, want := range map[store.Capability]bool{
		store.CapTools: true, store.CapAudioTranscribe: true, store.CapAudioSpeech: false, store.CapVision: true,
	} {
		if got, ok := syn.Fact(cap); !ok || got != want {
			t.Errorf("synonym %s = (%v, %v), want (%v, true)", cap, got, ok, want)
		}
	}

	// A real answer, from deepseek-v4-pro: it commits to a window with a
	// confidence flag, answers the fields it is sure about, and names the ones it
	// is not — which is exactly the behaviour the prompt asks for.
	real := ParseModelFacts(`{"context_window":128000,"window_confidence":"low","text":true,
		"vision":null,"tools":null,"image_gen":false,"asr":null,"tts":false,
		"unsure":["vision","tools","asr"]}`)
	if real.ContextWindow != 128_000 || real.WindowConfidence != "low" {
		t.Errorf("window = %d (%s), want 128000 (low)", real.ContextWindow, real.WindowConfidence)
	}
	if v, ok := real.Fact(store.CapChat); !ok || !v {
		t.Errorf("chat = (%v, %v)", v, ok)
	}
	if v, ok := real.Fact(store.CapImageGen); !ok || v {
		t.Errorf("image_gen = (%v, %v), want a definite false", v, ok)
	}
	for _, unknown := range []store.Capability{store.CapVision, store.CapTools, store.CapAudioTranscribe} {
		if _, ok := real.Fact(unknown); ok {
			t.Errorf("%s was recorded although the model listed it as unsure", unknown)
		}
	}
	if len(real.Unsure) != 3 {
		t.Errorf("unsure = %v, want the three fields it named", real.Unsure)
	}

	// A boolean written as a word is still a definite answer.
	words := ParseModelFacts(`{"vision": "yes", "asr": "不确定"}`)
	if v, ok := words.Fact(store.CapVision); !ok || !v {
		t.Errorf("vision = (%v, %v), want (true, true)", v, ok)
	}
	if _, ok := words.Fact(store.CapAudioTranscribe); ok {
		t.Error("「不确定」 was recorded as an answer")
	}
}

// TestCapabilitiesFromEndpoints: a provider's own endpoint list is evidence for
// what a model can do, and never evidence against.
func TestCapabilitiesFromEndpoints(t *testing.T) {
	got := CapabilitiesFromEndpoints([]string{"/chat/completions", "/responses"})
	if len(got) != 1 || got[0] != store.CapChat {
		t.Fatalf("capabilities = %v, want just chat", got)
	}
	if got := CapabilitiesFromEndpoints([]string{"/messages"}); len(got) != 1 || got[0] != store.CapChat {
		t.Errorf("an Anthropic-shaped endpoint is still text chat: %v", got)
	}
	if got := CapabilitiesFromEndpoints([]string{"/images/generations"}); len(got) != 1 || got[0] != store.CapImageGen {
		t.Errorf("images = %v", got)
	}
	if got := CapabilitiesFromEndpoints([]string{"/audio/transcriptions"}); len(got) != 1 || got[0] != store.CapAudioTranscribe {
		t.Errorf("transcriptions = %v", got)
	}
	// Nothing said means nothing claimed.
	if got := CapabilitiesFromEndpoints(nil); len(got) != 0 {
		t.Errorf("no endpoints = %v, want nothing", got)
	}
}
