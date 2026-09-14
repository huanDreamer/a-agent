package jobs

// byteWindow is the bounded in-memory tail of one job's output.
//
// It exists so that reading a job's output never depends on how long the job has
// been running: a server that has printed a gigabyte since breakfast is still
// served from the last 256 KiB, in constant memory, and the bytes it can no
// longer show are counted rather than forgotten. That count is what lets a
// reader be told "you are missing the beginning of this stream" instead of
// silently receiving a stream that looks complete.
//
// It is not safe for concurrent use: the caller (process) holds its lock.
type byteWindow struct {
	buf   []byte // ring: len(buf) is the capacity
	head  int    // index of the oldest stored byte
	size  int    // bytes stored, size <= len(buf)
	total int64  // bytes ever written, including those discarded
}

// newByteWindow returns a window that keeps at most capacity bytes.
func newByteWindow(capacity int) *byteWindow {
	if capacity <= 0 {
		capacity = DefaultWindowBytes
	}
	return &byteWindow{buf: make([]byte, capacity)}
}

// Write appends p, discarding the oldest bytes to stay within capacity. It
// always accepts the whole slice: a short write would make the caller (and, one
// level up, os/exec's copy) treat the output as lost.
func (w *byteWindow) Write(p []byte) {
	w.total += int64(len(p))
	if len(p) == 0 {
		return
	}
	if len(p) >= len(w.buf) {
		// The chunk alone is bigger than the window: keep its tail and start
		// over, which is also the cheapest way to express "everything older is
		// gone".
		tail := p[len(p)-len(w.buf):]
		copy(w.buf, tail)
		w.head = 0
		w.size = len(w.buf)
		return
	}
	if overflow := w.size + len(p) - len(w.buf); overflow > 0 {
		w.head = (w.head + overflow) % len(w.buf)
		w.size -= overflow
	}
	// Two chunks at most: from the write position to the end of the ring, then
	// whatever is left at the front.
	at := (w.head + w.size) % len(w.buf)
	n := copy(w.buf[at:], p)
	copy(w.buf, p[n:])
	w.size += len(p)
}

// Total is every byte ever written, including what has been discarded.
func (w *byteWindow) Total() int64 { return w.total }

// Len is how many bytes the window currently holds.
func (w *byteWindow) Len() int { return w.size }

// Start is the absolute offset of the window's first stored byte: everything
// below it has been discarded.
func (w *byteWindow) Start() int64 { return w.total - int64(w.size) }

// Slice returns up to max bytes starting at the absolute offset from, as a copy
// so the caller can hold it without the lock. Offsets outside what the window
// holds are clamped, and an offset at or past Total returns nothing — the caller
// is expected to have clamped already, and clamping again here keeps this
// function total.
func (w *byteWindow) Slice(from int64, max int) []byte {
	if max <= 0 || w.size == 0 {
		return nil
	}
	start := w.Start()
	if from < start {
		from = start
	}
	if from >= w.total {
		return nil
	}
	off := int(from - start)
	n := w.size - off
	if n > max {
		n = max
	}
	out := make([]byte, 0, n)
	at := (w.head + off) % len(w.buf)
	if first := len(w.buf) - at; first >= n {
		return append(out, w.buf[at:at+n]...)
	} else {
		out = append(out, w.buf[at:]...)
		return append(out, w.buf[:n-first]...)
	}
}
