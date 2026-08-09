package output

import (
	"bytes"
	"math/rand/v2"
	"runtime"
	"testing"
)

func TestUnderLimitIsExact(t *testing.T) {
	b := NewTailBuffer(16)
	b.Write([]byte("hello "))
	b.Write([]byte("world"))
	if got := string(b.Bytes()); got != "hello world" || b.Truncated() {
		t.Fatalf("got %q truncated=%v", got, b.Truncated())
	}
}

func TestExactlyAtLimitIsNotTruncated(t *testing.T) {
	b := NewTailBuffer(4)
	b.Write([]byte("ab"))
	b.Write([]byte("cd"))
	if string(b.Bytes()) != "abcd" || b.Truncated() {
		t.Fatalf("got %q truncated=%v", b.Bytes(), b.Truncated())
	}
}

func TestKeepsTail(t *testing.T) {
	b := NewTailBuffer(5)
	b.Write([]byte("abc"))
	b.Write([]byte("defg"))
	b.Write([]byte("hi"))
	if got := string(b.Bytes()); got != "efghi" || !b.Truncated() {
		t.Fatalf("got %q truncated=%v", got, b.Truncated())
	}
}

// Against a naive reference on random write sizes, the ring must always hold
// exactly the last limit bytes.
func TestMatchesReference(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for trial := range 200 {
		limit := 1 + r.IntN(64)
		b := NewTailBuffer(limit)
		var ref []byte
		for range 1 + r.IntN(30) {
			p := make([]byte, r.IntN(3*limit))
			for i := range p {
				p[i] = byte('a' + r.IntN(26))
			}
			b.Write(p)
			ref = append(ref, p...)
		}
		want := ref
		if len(want) > limit {
			want = want[len(want)-limit:]
		}
		if !bytes.Equal(b.Bytes(), want) || b.Truncated() != (len(ref) > limit) {
			t.Fatalf("trial %d limit %d: got %q (trunc %v), want %q (trunc %v)",
				trial, limit, b.Bytes(), b.Truncated(), want, len(ref) > limit)
		}
	}
}

func TestMemoryDoesNotGrowWithOutput(t *testing.T) {
	const limit = 32 * 1024
	b := NewTailBuffer(limit)
	chunk := bytes.Repeat([]byte("x"), 64*1024)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range 1600 { // ~100 MiB
		b.Write(chunk)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)

	if b.Cap() != limit {
		t.Fatalf("buffer storage grew to %d, want %d", b.Cap(), limit)
	}
	if grew := int64(after.HeapInuse) - int64(before.HeapInuse); grew > 4*1024*1024 {
		t.Fatalf("heap grew by %d bytes after writing 100 MiB through a %d-byte buffer", grew, limit)
	}
	if !b.Truncated() || len(b.Bytes()) != limit {
		t.Fatal("expected a full, truncated buffer")
	}
}

func TestStringRepairsSplitRune(t *testing.T) {
	b := NewTailBuffer(3)
	b.Write([]byte("xé!")) // 'é' is two bytes; the tail keeps only its second byte
	b.Write([]byte("é"))
	if s := b.String(); !bytes.Equal([]byte(s), []byte(string([]rune(s)))) {
		t.Fatalf("String must be valid UTF-8, got %q", s)
	}
}
