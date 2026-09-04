package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

func newTestStore(t *testing.T) Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "mem"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAppendReadRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Append(ctx, "alice", NewEntry(KindUser, "hello")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Append(ctx, "alice", NewEntry(KindAssistant, "hi there")); err != nil {
		t.Fatalf("append: %v", err)
	}

	entries, err := s.Read(ctx, "alice")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	// Newest first.
	if entries[0].Content != "hi there" || entries[1].Content != "hello" {
		t.Fatalf("unexpected order: %+v", entries)
	}
}

func TestReadFilterByKind(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.Append(ctx, "ns", NewEntry(KindUser, "u1"))
	_ = s.Append(ctx, "ns", NewEntry(KindAssistant, "a1"))
	_ = s.Append(ctx, "ns", NewEntry(KindTool, "t1"))

	onlyUser, err := s.Read(ctx, "ns", KindUser)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(onlyUser) != 1 || onlyUser[0].Kind != KindUser {
		t.Fatalf("expected 1 user entry, got %+v", onlyUser)
	}
}

func TestNamespaceIsolation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.Append(ctx, "alice", NewEntry(KindUser, "alice message"))
	_ = s.Append(ctx, "bob", NewEntry(KindUser, "bob message"))

	a, _ := s.Read(ctx, "alice")
	b, _ := s.Read(ctx, "bob")
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("isolation broken: alice=%d bob=%d", len(a), len(b))
	}
	if a[0].Content != "alice message" || b[0].Content != "bob message" {
		t.Fatalf("wrong isolation content")
	}
}

func TestInvalidNamespaceRejected(t *testing.T) {
	s := newTestStore(t)
	for _, ns := range []string{"", "a/b", "..", "../etc", ".hidden"} {
		if err := s.Append(context.Background(), ns, NewEntry(KindUser, "x")); err == nil {
			t.Fatalf("expected error for namespace %q", ns)
		}
	}
}

func TestReadMissingNamespace(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Read(context.Background(), "nobody"); err == nil {
		t.Fatal("expected ErrNotFound for missing namespace")
	}
}

func TestBufferEviction(t *testing.T) {
	b := NewBuffer(3)
	for i := 0; i < 5; i++ {
		b.Append(&schema.Message{Role: schema.User, Content: string(rune('a' + i))})
	}
	if b.Len() != 3 {
		t.Fatalf("expected cap 3, got %d", b.Len())
	}
	list := b.List()
	if list[0].Content != "c" || list[1].Content != "d" || list[2].Content != "e" {
		t.Fatalf("unexpected eviction: %+v", list)
	}
}

func TestBufferReset(t *testing.T) {
	b := NewBuffer(4)
	b.Append(&schema.Message{Role: schema.User, Content: "x"})
	b.Reset()
	if b.Len() != 0 {
		t.Fatalf("expected empty after reset, got %d", b.Len())
	}
}

func TestAddFactAndSearch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	fut := time.Now()
	if err := s.AddFact(ctx, "alice", Fact{
		Key:       "favorite_color",
		Value:     "blue",
		Keywords:  []string{"color"},
		CreatedAt: fut,
	}); err != nil {
		t.Fatalf("add fact: %v", err)
	}
	if err := s.AddFact(ctx, "alice", Fact{
		Key:   "city",
		Value: "beijing",
	}); err != nil {
		t.Fatalf("add fact2: %v", err)
	}

	res, err := s.SearchFacts(ctx, "color", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 1 || res[0].Value != "blue" {
		t.Fatalf("expected 1 color fact, got %+v", res)
	}

	// city should be keyword-indexed from its text.
	cities, _ := s.SearchFacts(ctx, "beijing", 10)
	if len(cities) != 1 {
		t.Fatalf("expected beijing fact, got %+v", cities)
	}

	facts, err := s.Facts(ctx, "alice")
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(facts))
	}
}

func TestKeywordsFunc(t *testing.T) {
	kws := keywords("Hello, World! 你好 world 123")
	found := map[string]bool{}
	for _, k := range kws {
		found[k] = true
	}
	for _, want := range []string{"hello", "world", "你好", "123"} {
		if !found[want] {
			t.Fatalf("keyword %q not found in %v", want, kws)
		}
	}
}
