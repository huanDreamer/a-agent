package store

import (
	"errors"
	"strings"
	"testing"
)

func TestFmtErr(t *testing.T) {
	base := errors.New("disk full")

	// No format args: msg is used verbatim.
	got := fmtErr("create migrations table", base)
	if !strings.Contains(got.Error(), "create migrations table") {
		t.Errorf("missing message: %q", got)
	}
	if !errors.Is(got, base) {
		t.Error("wrapped error chain broken")
	}

	// Format args: msg is run through Sprintf.
	got = fmtErr("apply migration %d (%s)", base, 7, "create_foo")
	if !strings.Contains(got.Error(), "apply migration 7 (create_foo)") {
		t.Errorf("format not applied: %q", got)
	}
	if !errors.Is(got, base) {
		t.Error("wrapped error chain broken (formatted)")
	}
}
