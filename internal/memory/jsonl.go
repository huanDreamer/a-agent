package memory

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// appendJSONL appends one JSON-encoded value as a new line to path.
func appendJSONL(ctx context.Context, path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("memory: marshal: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("memory: open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("memory: write %s: %w", path, err)
	}
	return nil
}

// appendRaw appends an already-constructed Entry to the namespace file.
func appendRaw(ctx context.Context, path string, e Entry) error {
	return appendJSONL(ctx, path, e)
}

// readJSONL reads all decoded values of type T from a JSONL file, in order.
func readJSONL[T any](ctx context.Context, path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("memory: open %s: %w", path, err)
	}
	defer f.Close()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var v T
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			return nil, fmt.Errorf("memory: unmarshal line: %w", err)
		}
		out = append(out, v)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("memory: scan %s: %w", path, err)
	}
	return out, nil
}

// readFile is a trivial wrapper used by SearchFacts to re-read entries.
func readFile(dir, ns string) ([]Entry, error) {
	return readJSONL[Entry](context.Background(), dir+"/"+ns+".jsonl")
}

// keywords lower-cases, trims punctuation, and returns the distinct tokens
// of a text that are at least minWordLen characters long.
func keywords(text string) []string {
	const minWordLen = 2
	seen := map[string]bool{}
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= '\u4e00' && r <= '\u9fff')
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len([]rune(f)) < minWordLen {
			continue
		}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// matchesAny reports whether any keyword of q intersects f's keyword set.
func matchesAny(f Fact, q []string) bool {
	fs := map[string]bool{}
	for _, k := range f.Keywords {
		fs[strings.ToLower(k)] = true
	}
	for _, k := range q {
		if fs[k] {
			return true
		}
	}
	// Also match against the raw content text.
	for _, k := range q {
		if strings.Contains(strings.ToLower(f.Content()), k) {
			return true
		}
	}
	return false
}

// appendIfMissing appends s to ss unless already present.
func appendIfMissing(ss []string, s string) []string {
	for _, x := range ss {
		if x == s {
			return ss
		}
	}
	return append(ss, s)
}
