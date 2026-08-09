// Package results records and reads command outcomes (SPEC.md §7.5).
package results

import (
	"context"
	"errors"
	"sync"
	"time"

	"sqsh/internal/protocol"
)

var (
	// ErrAlreadyClaimed is returned by Claim when a record already exists for
	// the request_id. It is an ordinary outcome: a `duplicate` rejection.
	ErrAlreadyClaimed = errors.New("results: request already claimed")
	// ErrStaleWrite is returned by Complete when the write would move a
	// result's status backwards (§7.2.1). The write was dropped, not applied.
	ErrStaleWrite = errors.New("results: status regression prevented")
	// ErrNotFound is returned by Get when no record exists.
	ErrNotFound = errors.New("results: no such result")
)

// ResultStore is the §7.5 result contract.
type ResultStore interface {
	// Claim writes r only if no record exists for r.RequestID, and must fail
	// with ErrAlreadyClaimed otherwise. Claim with a `running` record reserves
	// a request for execution; Claim with a terminal record records a
	// rejection without ever overwriting another agent's record.
	Claim(ctx context.Context, r protocol.Result) error
	// Complete writes a terminal result. It enforces §7.2.1 status ordering:
	// a write that would move the stored result backwards is dropped and
	// reported as ErrStaleWrite, not applied.
	Complete(ctx context.Context, r protocol.Result) error
	// Get is a strongly consistent point read. It returns ErrNotFound when no
	// record exists.
	Get(ctx context.Context, requestID string) (protocol.Result, error)
}

// CompleteAllowed is the §7.2.1 rule as this release implements it: a
// terminal write may create a record or replace a `running` one, and nothing
// else. Every terminal status is final, and `running` is never written by
// Complete.
func CompleteAllowed(existing *protocol.Status, next protocol.Status) bool {
	if !next.Terminal() {
		return false
	}
	return existing == nil || *existing == protocol.StatusRunning
}

// Memory is an in-memory ResultStore for tests.
type Memory struct {
	Retention time.Duration
	Now       func() time.Time

	mu      sync.Mutex
	records map[string]protocol.Result
	// Writes counts every applied write, for assertions.
	Writes int
}

// NewMemory returns an empty in-memory store.
func NewMemory(retention time.Duration) *Memory {
	return &Memory{Retention: retention, Now: time.Now, records: map[string]protocol.Result{}}
}

func (m *Memory) Claim(_ context.Context, r protocol.Result) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.records[r.RequestID]; ok {
		return ErrAlreadyClaimed
	}
	m.put(r)
	return nil
}

func (m *Memory) Complete(_ context.Context, r protocol.Result) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var existing *protocol.Status
	if cur, ok := m.records[r.RequestID]; ok {
		existing = &cur.Status
	}
	if !CompleteAllowed(existing, r.Status) {
		return ErrStaleWrite
	}
	m.put(r)
	return nil
}

func (m *Memory) Get(_ context.Context, id string) (protocol.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[id]
	if !ok {
		return protocol.Result{}, ErrNotFound
	}
	return r, nil
}

func (m *Memory) put(r protocol.Result) {
	r.TTL = m.Now().Add(m.Retention).Unix()
	m.records[r.RequestID] = r
	m.Writes++
}

// All returns a snapshot of every record, for assertions.
func (m *Memory) All() map[string]protocol.Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]protocol.Result, len(m.records))
	for k, v := range m.records {
		out[k] = v
	}
	return out
}
