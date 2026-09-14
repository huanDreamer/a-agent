package jobs

import (
	"strings"
	"testing"
)

// whole returns everything the window still holds, which is what a reader gets
// when it asks from the window's start.
func whole(w *byteWindow) string {
	return string(w.Slice(w.Start(), w.Len()+1))
}

func TestByteWindow_KeepsTheTailAndCountsWhatItDropped(t *testing.T) {
	w := newByteWindow(8)
	w.Write([]byte("abc"))
	if got := whole(w); got != "abc" {
		t.Errorf("window = %q, want abc", got)
	}
	if w.Total() != 3 || w.Len() != 3 || w.Start() != 0 {
		t.Errorf("total=%d len=%d start=%d, want 3/3/0", w.Total(), w.Len(), w.Start())
	}

	w.Write([]byte("defghij")) // 10 bytes in, 8 kept
	if got := whole(w); got != "cdefghij" {
		t.Errorf("window = %q, want cdefghij", got)
	}
	if w.Total() != 10 || w.Len() != 8 || w.Start() != 2 {
		t.Errorf("total=%d len=%d start=%d, want 10/8/2", w.Total(), w.Len(), w.Start())
	}

	// An offset older than the window is clamped forward, never faked.
	if got := string(w.Slice(0, 100)); got != "cdefghij" {
		t.Errorf("clamped read = %q, want cdefghij", got)
	}
	if got := string(w.Slice(2, 3)); got != "cde" {
		t.Errorf("read = %q, want cde", got)
	}
	if got := string(w.Slice(9, 10)); got != "j" {
		t.Errorf("read = %q, want j", got)
	}
	if got := w.Slice(10, 10); got != nil {
		t.Errorf("read past the end = %q, want nothing", got)
	}
	if got := w.Slice(11, 10); got != nil {
		t.Errorf("read beyond the end = %q, want nothing", got)
	}
}

func TestByteWindow_WrapsAround(t *testing.T) {
	w := newByteWindow(5)
	w.Write([]byte("abcde"))
	if got := whole(w); got != "abcde" {
		t.Fatalf("window = %q, want abcde", got)
	}
	w.Write([]byte("fg")) // the ring wraps here
	if got := whole(w); got != "cdefg" {
		t.Errorf("window = %q, want cdefg", got)
	}
	if w.Start() != 2 {
		t.Errorf("start = %d, want 2", w.Start())
	}
	// A read that spans the wrap point must be contiguous.
	if got := string(w.Slice(3, 4)); got != "defg" {
		t.Errorf("spanning read = %q, want defg", got)
	}
}

func TestByteWindow_ChunkLargerThanTheWindow(t *testing.T) {
	w := newByteWindow(4)
	w.Write([]byte("0123456789"))
	if got := whole(w); got != "6789" {
		t.Errorf("window = %q, want 6789", got)
	}
	if w.Total() != 10 || w.Start() != 6 {
		t.Errorf("total=%d start=%d, want 10/6", w.Total(), w.Start())
	}

	// A chunk exactly the size of the window replaces it entirely.
	w.Write([]byte("abcd"))
	if got := whole(w); got != "abcd" {
		t.Errorf("window = %q, want abcd", got)
	}
}

func TestByteWindow_EmptyAndEdgeReads(t *testing.T) {
	w := newByteWindow(4)
	if w.Len() != 0 || w.Total() != 0 || w.Start() != 0 {
		t.Errorf("a fresh window is not empty: len=%d total=%d start=%d", w.Len(), w.Total(), w.Start())
	}
	if got := w.Slice(0, 4); got != nil {
		t.Errorf("read of an empty window = %q, want nothing", got)
	}

	w.Write(nil)
	if w.Total() != 0 {
		t.Errorf("an empty write counted as %d bytes", w.Total())
	}

	w.Write([]byte("ab"))
	if got := w.Slice(0, 0); got != nil {
		t.Errorf("a read with max 0 returned %q, want nothing", got)
	}
	if got := w.Slice(0, -1); got != nil {
		t.Errorf("a read with a negative max returned %q, want nothing", got)
	}
}

func TestNewByteWindow_UsesTheDefaultCapacity(t *testing.T) {
	for _, capacity := range []int{0, -1} {
		w := newByteWindow(capacity)
		if len(w.buf) != DefaultWindowBytes {
			t.Errorf("capacity %d produced a %d-byte window, want the default %d",
				capacity, len(w.buf), DefaultWindowBytes)
		}
	}
}

// TestByteWindow_IsBoundedUnderSustainedWriting is the property that makes it
// safe to attach to a server that runs for weeks.
func TestByteWindow_IsBoundedUnderSustainedWriting(t *testing.T) {
	w := newByteWindow(64)
	chunk := []byte(strings.Repeat("x", 10))
	for i := 0; i < 100; i++ {
		w.Write(chunk)
	}
	if w.Len() != 64 {
		t.Errorf("window holds %d bytes after 1000 written, want the 64-byte cap", w.Len())
	}
	if w.Total() != 1000 {
		t.Errorf("total = %d, want 1000: every byte is counted even when it is discarded", w.Total())
	}
	if w.Total()-int64(w.Len()) != 936 {
		t.Errorf("discarded = %d, want 936", w.Total()-int64(w.Len()))
	}
}
