// Package output bounds captured command output (SPEC.md §12).
//
// S3 offload is deferred: output beyond the limit is discarded and the result
// is flagged as truncated.
package output

import "strings"

// TailBuffer is an io.Writer that retains at most Limit bytes, keeping the
// most recent ones — the error is usually at the end. Its memory use is fixed
// at Limit no matter how much is written to it.
type TailBuffer struct {
	buf       []byte // ring storage, allocated once at Limit
	start     int    // index of the oldest byte once the ring has wrapped
	full      bool
	truncated bool
	limit     int
}

// NewTailBuffer returns a buffer retaining at most limit bytes.
func NewTailBuffer(limit int) *TailBuffer {
	return &TailBuffer{buf: make([]byte, 0, limit), limit: limit}
}

// Write implements io.Writer. It never fails and always reports len(p).
func (t *TailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if t.limit == 0 {
		t.truncated = t.truncated || n > 0
		return n, nil
	}
	if len(p) >= t.limit {
		// Only the last limit bytes of p can survive.
		t.truncated = t.truncated || t.full || len(t.buf) > 0 || len(p) > t.limit
		t.buf = append(t.buf[:0], p[len(p)-t.limit:]...)
		t.start, t.full = 0, true
		return n, nil
	}
	if !t.full {
		room := t.limit - len(t.buf)
		if len(p) <= room {
			t.buf = append(t.buf, p...)
			t.full = len(t.buf) == t.limit
			return n, nil
		}
		t.buf = append(t.buf, p[:room]...)
		p = p[room:]
		t.full = true
	}
	t.truncated = true
	for len(p) > 0 {
		c := copy(t.buf[t.start:], p)
		p = p[c:]
		t.start = (t.start + c) % t.limit
	}
	return n, nil
}

// Bytes returns the retained output in order.
func (t *TailBuffer) Bytes() []byte {
	if !t.full || t.start == 0 {
		return append([]byte(nil), t.buf...)
	}
	out := make([]byte, 0, t.limit)
	out = append(out, t.buf[t.start:]...)
	return append(out, t.buf[:t.start]...)
}

// String returns the retained output as valid UTF-8. Invalid sequences —
// including a multi-byte rune split by truncation — become U+FFFD, since
// DynamoDB string attributes must be valid UTF-8.
func (t *TailBuffer) String() string { return strings.ToValidUTF8(string(t.Bytes()), "�") }

// Truncated reports whether any written byte was discarded.
func (t *TailBuffer) Truncated() bool { return t.truncated }

// Cap returns the capacity of the underlying storage, for memory-bound tests.
func (t *TailBuffer) Cap() int { return cap(t.buf) }
