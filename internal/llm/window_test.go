package llm

import "testing"

// TestParseWindowAnswer pins the shapes a model actually answers this question
// in. Refusing any of them would throw away the only available source for a
// provider that publishes no window of its own.
func TestParseWindowAnswer(t *testing.T) {
	cases := map[string]int{
		"128000":                          128_000,
		"  128000  ":                      128_000,
		"128,000":                         128_000,
		"我的上下文窗口是 128000 token。":          128_000,
		"131072 tokens":                   131_072,
		"1M":                              1_000_000,
		"128K":                            128_000,
		"上下文窗口为 200K token":               200_000,
		"100万":                            1_000_000,
		"**128000**":                      128_000,
		"1,048,576":                       1_048_576,
		"我是 Claude，上下文窗口 200000 token":   200_000,
		// The model does not know, or does not want to answer.
		"0":        0,
		"":         0,
		"我不知道":     0,
		"抱歉，我无法提供该信息": 0,
		// Implausible values are refused rather than recorded: a parameter
		// count, a date or a version number is not a window.
		"7":              0,
		"1000000000":     0,
		"2026-09-19":     0,
		"gpt-4":          0,
		"我的窗口是 0 token": 0,
	}
	for reply, want := range cases {
		if got := ParseWindowAnswer(reply); got != want {
			t.Errorf("ParseWindowAnswer(%q) = %d, want %d", reply, got, want)
		}
	}
}
