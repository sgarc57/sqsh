// Package awsxfake provides in-memory SNS and SQS fakes implementing the
// awsx interfaces, with enough fidelity — visibility timeouts, receive
// counts, filter-policy routing — to drive the agent with no AWS account.
package awsxfake

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"sqsh/internal/awsx"
)

var seq atomic.Int64

func next(prefix string) string { return fmt.Sprintf("%s-%d", prefix, seq.Add(1)) }

type message struct {
	id, body, handle string
	visibleAt        time.Time
	receives         int
}

// Queue is an in-memory SQS queue.
type Queue struct {
	mu   sync.Mutex
	msgs []*message

	// ReceiveErr, if set, is consulted before every receive; a non-nil
	// return is returned as the receive error.
	ReceiveErr func() error

	// Recorded calls.
	ReceiveCalls      int
	RequestedMax      []int
	Deleted           []string // message IDs
	VisibilityChanges []time.Duration
}

// NewQueue returns an empty queue.
func NewQueue() *Queue { return &Queue{} }

// Send enqueues body as a new, immediately visible message and returns its ID.
func (q *Queue) Send(body string) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	m := &message{id: next("msg"), body: body}
	q.msgs = append(q.msgs, m)
	return m.id
}

// Receive implements awsx.Receiver, long-polling up to wait.
func (q *Queue) Receive(ctx context.Context, n int, wait, visibility time.Duration) ([]awsx.Message, error) {
	q.mu.Lock()
	q.ReceiveCalls++
	q.RequestedMax = append(q.RequestedMax, n)
	errFn := q.ReceiveErr
	q.mu.Unlock()
	if errFn != nil {
		if err := errFn(); err != nil {
			return nil, err
		}
	}
	deadline := time.Now().Add(wait)
	for {
		if out := q.take(n, visibility); len(out) > 0 {
			return out, nil
		}
		if !time.Now().Before(deadline) {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (q *Queue) take(n int, visibility time.Duration) []awsx.Message {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	var out []awsx.Message
	for _, m := range q.msgs {
		if len(out) >= n {
			break
		}
		if now.Before(m.visibleAt) {
			continue
		}
		m.visibleAt = now.Add(visibility)
		m.handle = next("rh")
		m.receives++
		out = append(out, awsx.Message{ID: m.id, ReceiptHandle: m.handle, Body: m.body, ReceiveCount: m.receives})
	}
	return out
}

// Delete implements awsx.Deleter. A stale receipt handle fails, as it does
// once SQS has redelivered the message to someone else.
func (q *Queue) Delete(_ context.Context, handle string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, m := range q.msgs {
		if m.handle == handle {
			q.msgs = append(q.msgs[:i], q.msgs[i+1:]...)
			q.Deleted = append(q.Deleted, m.id)
			return nil
		}
	}
	return errors.New("fake sqs: receipt handle is invalid or stale")
}

// ChangeVisibility implements awsx.VisibilityChanger.
func (q *Queue) ChangeVisibility(_ context.Context, handle string, timeout time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, m := range q.msgs {
		if m.handle == handle {
			m.visibleAt = time.Now().Add(timeout)
			q.VisibilityChanges = append(q.VisibilityChanges, timeout)
			return nil
		}
	}
	return errors.New("fake sqs: receipt handle is invalid or stale")
}

// Len returns the number of messages not yet deleted.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.msgs)
}

// Receives returns how many times the message with id has been delivered.
func (q *Queue) Receives(id string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, m := range q.msgs {
		if m.id == id {
			return m.receives
		}
	}
	return -1
}

// Snapshot returns copies of the recorded call slices under the lock.
func (q *Queue) Snapshot() (requested []int, deleted []string, vis []time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]int(nil), q.RequestedMax...), append([]string(nil), q.Deleted...),
		append([]time.Duration(nil), q.VisibilityChanges...)
}

// Bus is an in-memory SNS topic delivering to subscribed queues by a
// `{"host": [...]}` filter policy, with raw message delivery.
type Bus struct {
	mu        sync.Mutex
	subs      []sub
	Published []awsx.Routing
	// PublishErr, if set, fails every publish.
	PublishErr error
}

type sub struct {
	q     *Queue
	hosts map[string]bool
}

// Subscribe delivers messages whose host attribute is one of hosts to q.
func (b *Bus) Subscribe(q *Queue, hosts ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := sub{q: q, hosts: map[string]bool{}}
	for _, h := range hosts {
		s.hosts[h] = true
	}
	b.subs = append(b.subs, s)
}

// Publish implements awsx.Publisher.
func (b *Bus) Publish(_ context.Context, body []byte, rt awsx.Routing) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.PublishErr != nil {
		return "", b.PublishErr
	}
	b.Published = append(b.Published, rt)
	for _, s := range b.subs {
		if s.hosts[rt.Host] {
			s.q.Send(string(body))
		}
	}
	return next("sns"), nil
}
