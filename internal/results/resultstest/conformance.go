// Package resultstest is the ResultStore conformance suite, run against the
// in-memory fake in unit tests and against DynamoDB in integration tests.
package resultstest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sqsh/internal/protocol"
	"sqsh/internal/results"
)

// Retention is the retention every store under test must be built with.
const Retention = 14 * 24 * time.Hour

// Run executes the conformance suite. newStore must return an empty store
// configured with Retention.
func Run(t *testing.T, newStore func(t *testing.T) results.ResultStore) {
	ctx := context.Background()

	t.Run("second claim returns ErrAlreadyClaimed", func(t *testing.T) {
		s := newStore(t)
		r := running(t)
		if err := s.Claim(ctx, r); err != nil {
			t.Fatalf("first claim: %v", err)
		}
		if err := s.Claim(ctx, r); !errors.Is(err, results.ErrAlreadyClaimed) {
			t.Fatalf("second claim: got %v, want ErrAlreadyClaimed", err)
		}
	})

	t.Run("concurrent claims: exactly one wins", func(t *testing.T) {
		s := newStore(t)
		r := running(t)
		var wins, dupes atomic.Int32
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				switch err := s.Claim(ctx, r); {
				case err == nil:
					wins.Add(1)
				case errors.Is(err, results.ErrAlreadyClaimed):
					dupes.Add(1)
				default:
					t.Errorf("claim: %v", err)
				}
			})
		}
		wg.Wait()
		if wins.Load() != 1 || dupes.Load() != 7 {
			t.Fatalf("wins=%d dupes=%d, want 1/7", wins.Load(), dupes.Load())
		}
	})

	t.Run("claim of a terminal record never overwrites", func(t *testing.T) {
		s := newStore(t)
		r := running(t)
		must(t, s.Claim(ctx, r))
		rej := r
		rej.Status, rej.RejectionReason = protocol.StatusRejected, ptr(protocol.ReasonInvalidTarget)
		if err := s.Claim(ctx, rej); !errors.Is(err, results.ErrAlreadyClaimed) {
			t.Fatalf("got %v", err)
		}
		if got := get(t, s, r.RequestID); got.Status != protocol.StatusRunning {
			t.Fatalf("record was overwritten: %+v", got)
		}
	})

	t.Run("complete moves running forward", func(t *testing.T) {
		s := newStore(t)
		r := running(t)
		must(t, s.Claim(ctx, r))
		done := succeeded(r)
		must(t, s.Complete(ctx, done))
		got := get(t, s, r.RequestID)
		if got.Status != protocol.StatusSucceeded || got.ExitCode == nil || *got.ExitCode != 0 || got.Stdout != "active\n" {
			t.Fatalf("read-after-write mismatch: %+v", got)
		}
	})

	t.Run("complete refuses backward and final-to-final writes", func(t *testing.T) {
		for _, next := range []protocol.Status{
			protocol.StatusRunning, protocol.StatusFailed, protocol.StatusSucceeded,
			protocol.StatusTimedOut, protocol.StatusRejected, protocol.StatusExpired,
		} {
			s := newStore(t)
			r := running(t)
			must(t, s.Claim(ctx, r))
			must(t, s.Complete(ctx, succeeded(r)))
			w := r
			w.Status = next
			if err := s.Complete(ctx, w); !errors.Is(err, results.ErrStaleWrite) {
				t.Fatalf("succeeded -> %s: got %v, want ErrStaleWrite", next, err)
			}
			if got := get(t, s, r.RequestID); got.Status != protocol.StatusSucceeded {
				t.Fatalf("succeeded -> %s was applied: %+v", next, got)
			}
		}
	})

	t.Run("complete never writes running", func(t *testing.T) {
		s := newStore(t)
		r := running(t)
		if err := s.Complete(ctx, r); !errors.Is(err, results.ErrStaleWrite) {
			t.Fatalf("got %v, want ErrStaleWrite", err)
		}
		if _, err := s.Get(ctx, r.RequestID); !errors.Is(err, results.ErrNotFound) {
			t.Fatalf("running must not have been written: %v", err)
		}
	})

	t.Run("complete creates a missing record", func(t *testing.T) {
		s := newStore(t)
		r := running(t)
		r.Status, r.RejectionReason = protocol.StatusRejected, ptr(protocol.ReasonConcurrencyLimit)
		must(t, s.Complete(ctx, r))
		if got := get(t, s, r.RequestID); *got.RejectionReason != protocol.ReasonConcurrencyLimit {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("get unknown id is ErrNotFound", func(t *testing.T) {
		s := newStore(t)
		id, _ := protocol.NewRequestID()
		if _, err := s.Get(ctx, id); !errors.Is(err, results.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("records carry ttl from retention", func(t *testing.T) {
		s := newStore(t)
		r := running(t)
		before := time.Now()
		must(t, s.Claim(ctx, r))
		got := get(t, s, r.RequestID)
		lo, hi := before.Add(Retention).Unix()-1, time.Now().Add(Retention).Unix()+1
		if got.TTL < lo || got.TTL > hi {
			t.Fatalf("ttl %d not within [%d,%d]", got.TTL, lo, hi)
		}
	})

	t.Run("round trips every field", func(t *testing.T) {
		s := newStore(t)
		r := running(t)
		must(t, s.Claim(ctx, r))
		w := succeeded(r)
		w.Stderr, w.OutputTruncated, w.Error = "warn\n", true, ptr("boom")
		w.Attempt, w.DurationMS = 2, 1234
		must(t, s.Complete(ctx, w))
		got := get(t, s, r.RequestID)
		w.TTL = got.TTL
		if *got.Error != "boom" || got.Stderr != w.Stderr || !got.OutputTruncated || got.Attempt != 2 ||
			got.DurationMS != 1234 || got.Issuer != r.Issuer || got.Host != r.Host || got.Command != r.Command ||
			got.StartedAt != r.StartedAt || got.CompletedAt != w.CompletedAt || got.AgentVersion != r.AgentVersion {
			t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, w)
		}
	})
}

func running(t *testing.T) protocol.Result {
	t.Helper()
	id, err := protocol.NewRequestID()
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Result{
		RequestID: id, Host: "web01", Command: "systemctl", Issuer: "arn:aws:iam::123456789012:user/op",
		Status: protocol.StatusRunning, StartedAt: protocol.FormatTimeMillis(time.Now()),
		Attempt: 1, AgentVersion: "test",
	}
}

func succeeded(r protocol.Result) protocol.Result {
	zero := 0
	r.Status, r.ExitCode, r.Stdout = protocol.StatusSucceeded, &zero, "active\n"
	r.CompletedAt = protocol.FormatTimeMillis(time.Now())
	return r
}

func get(t *testing.T, s results.ResultStore, id string) protocol.Result {
	t.Helper()
	r, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return r
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func ptr[T any](v T) *T { return &v }
