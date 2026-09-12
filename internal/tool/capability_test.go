package tool

import (
	"context"
	"testing"
)

func TestCapabilityOf_DefaultsToRead(t *testing.T) {
	// An undeclared tool must not be assumed harmless-to-expose.
	if got := CapabilityOf(&stubTool{name: "x"}); got != CapRead {
		t.Errorf("undeclared capability = %q, want read", got)
	}
}

func TestWithCapability(t *testing.T) {
	base := &stubTool{name: "write_file"}
	tagged := WithCapability(base, CapWrite)
	if got := CapabilityOf(tagged); got != CapWrite {
		t.Errorf("capability = %q, want write", got)
	}
	// The wrapped tool must still work as a Tool.
	if _, err := tagged.Info(context.Background()); err != nil {
		t.Errorf("Info: %v", err)
	}
	// An empty capability leaves the tool alone.
	if got := WithCapability(base, ""); got != Tool(base) {
		t.Error("an empty capability should return the tool unchanged")
	}
	if got := WithCapability(nil, CapWrite); got != nil {
		t.Error("a nil tool should stay nil")
	}
}

func TestFilterByCapabilities(t *testing.T) {
	src := NewRegistry()
	for _, tc := range []struct {
		name string
		cap  Capability
	}{
		{"read_file", CapRead},
		{"glob", CapRead},
		{"write_file", CapWrite},
		{"edit_file", CapWrite},
		{"bash", CapExec},
	} {
		if err := src.Register(WithCapability(&stubTool{name: tc.name}, tc.cap)); err != nil {
			t.Fatalf("register %s: %v", tc.name, err)
		}
	}

	t.Run("no allow list means no restriction", func(t *testing.T) {
		got, err := FilterByCapabilities(src)
		if err != nil {
			t.Fatalf("filter: %v", err)
		}
		if len(got.Names()) != 5 {
			t.Errorf("names = %v, want all 5", got.Names())
		}
	})

	t.Run("read only", func(t *testing.T) {
		got, err := FilterByCapabilities(src, CapRead)
		if err != nil {
			t.Fatalf("filter: %v", err)
		}
		names := got.Names()
		if len(names) != 2 {
			t.Fatalf("names = %v, want the 2 read tools", names)
		}
		for _, n := range names {
			if n == "bash" || n == "write_file" {
				t.Errorf("a dangerous tool leaked into a read-only registry: %s", n)
			}
		}
	})

	t.Run("read and write but not exec", func(t *testing.T) {
		got, err := FilterByCapabilities(src, CapRead, CapWrite)
		if err != nil {
			t.Fatalf("filter: %v", err)
		}
		if len(got.Names()) != 4 {
			t.Errorf("names = %v, want 4 (no bash)", got.Names())
		}
		if _, ok := got.Get("bash"); ok {
			t.Error("bash must not be present when exec is not allowed")
		}
	})

	t.Run("nil source", func(t *testing.T) {
		got, err := FilterByCapabilities(nil, CapRead)
		if err != nil {
			t.Fatalf("filter: %v", err)
		}
		if len(got.Names()) != 0 {
			t.Errorf("names = %v, want empty", got.Names())
		}
	})
}

func TestFilterByCapabilities_RespectsAllowList(t *testing.T) {
	// A filtered registry must not resurrect a tool the source had restricted.
	src := NewRegistry()
	if err := src.Register(WithCapability(&stubTool{name: "read_file"}, CapRead)); err != nil {
		t.Fatal(err)
	}
	if err := src.Register(WithCapability(&stubTool{name: "bash"}, CapExec)); err != nil {
		t.Fatal(err)
	}
	if err := src.SetAllowList([]string{"read_file"}); err != nil {
		t.Fatal(err)
	}

	got, err := FilterByCapabilities(src, CapExec)
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(got.Names()) != 0 {
		t.Errorf("names = %v, want empty (bash was excluded by the source allow-list)", got.Names())
	}
}

func TestDescribeCapabilities(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(WithCapability(&stubTool{name: "read_file"}, CapRead)); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(WithCapability(&stubTool{name: "bash"}, CapExec)); err != nil {
		t.Fatal(err)
	}
	got := DescribeCapabilities(context.Background(), reg)
	if got["read_file"] != CapRead || got["bash"] != CapExec {
		t.Errorf("describe = %v", got)
	}
	if len(DescribeCapabilities(context.Background(), nil)) != 0 {
		t.Error("a nil registry should describe nothing")
	}
}
