package obs

import (
	"strings"
	"testing"
)

func TestNewLogger_ValidCombinations(t *testing.T) {
	cases := []struct {
		level  string
		format string
	}{
		{"debug", "json"},
		{"info", "json"},
		{"warn", "json"},
		{"error", "json"},
		{"debug", "console"},
		{"info", "console"},
		{"", ""}, // defaults
		{"info", ""},
	}
	for _, tc := range cases {
		t.Run(tc.level+"/"+tc.format, func(t *testing.T) {
			l, err := NewLogger(tc.level, tc.format)
			if err != nil {
				t.Fatalf("NewLogger(%q, %q) error: %v", tc.level, tc.format, err)
			}
			if l == nil {
				t.Fatal("nil logger")
			}
			_ = l.Sync()
		})
	}
}

func TestNewLogger_InvalidLevel(t *testing.T) {
	_, err := NewLogger("notalevel", "json")
	if err == nil {
		t.Fatal("expected error for invalid level")
	}
	if !strings.Contains(err.Error(), "log level") {
		t.Errorf("error %q should mention 'log level'", err)
	}
}

func TestNewLogger_InvalidFormat(t *testing.T) {
	_, err := NewLogger("info", "notaformat")
	if err == nil {
		t.Fatal("expected error for invalid format")
	}
	if !strings.Contains(err.Error(), "log format") {
		t.Errorf("error %q should mention 'log format'", err)
	}
}
