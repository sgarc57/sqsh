package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"sqsh/internal/awsx"
	"sqsh/internal/awsx/awsxfake"
	"sqsh/internal/commands"
	"sqsh/internal/config"
	"sqsh/internal/dispatcher"
	"sqsh/internal/observability"
	"sqsh/internal/protocol"
	"sqsh/internal/results"
)

const stubScript = `#!/bin/sh
echo "$@" >> "%LOG%"
case "$4" in
  slow.service) sleep 2.5 ;;
  hang.service) trap "" TERM; sleep 30 ;;
esac
echo "active SECRET-OUTPUT"
exit 0
`

type harness struct {
	t       *testing.T
	agent   *Agent
	queue   *awsxfake.Queue
	store   *results.Memory
	metrics *observability.Metrics
	logs    *syncBuffer
	execLog string
	cfg     *config.Config
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type option func(*config.Config)

func newHarness(t *testing.T, opts ...option) *harness {
	t.Helper()
	dir := t.TempDir()
	execLog := filepath.Join(dir, "exec.log")
	stub := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(stub, []byte(strings.ReplaceAll(stubScript, "%LOG%", execLog)), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		HostID: "web01", Region: "eu-central-1", QueueURL: "fake://web01", ResultsTable: "t",
		AllowedCommands: []string{"systemctl"},
		Commands: config.Commands{Systemctl: config.SystemctlConfig{
			Verbs: []string{"status", "is-active"},
			Units: []string{"nginx.service", "slow.service", "hang.service"},
		}},
		WaitTimeSeconds: 1,
	}
	cfg.ApplyDefaults()
	for _, o := range opts {
		o(cfg)
	}

	exec := &dispatcher.Executor{MaxOutputBytes: int(cfg.MaxOutputBytes), KillGrace: 200 * time.Millisecond, DryRun: !cfg.ShouldExecute()}
	reg := dispatcher.NewRegistry(cfg.AllowedCommands)
	reg.Register(&commands.Systemctl{Path: stub, Exec: exec}, true)
	reg.Register(&commands.Puppet{Exec: exec}, false)

	h := &harness{t: t, queue: awsxfake.NewQueue(), store: results.NewMemory(time.Hour),
		metrics: observability.NewMetrics(), logs: &syncBuffer{}, execLog: execLog, cfg: cfg}
	h.agent = New(Deps{
		Config: cfg, Queue: h.queue, Store: h.store, Registry: reg,
		Logger: observability.NewLogger(h.logs, "debug"), Metrics: h.metrics, Version: "test",
	})
	h.agent.Backoff = &awsx.Backoff{Base: 5 * time.Millisecond, Max: 20 * time.Millisecond}
	return h
}

// run starts the agent and returns a function that stops it and waits for
// Run to return.
func (h *harness) run() (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = h.agent.Run(ctx); close(done) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			h.t.Fatal("agent did not stop")
		}
	}
}

func (h *harness) executions() int {
	b, err := os.ReadFile(h.execLog)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "\n")
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s\nlogs:\n%s", what, h.logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *harness) result(id string) protocol.Result {
	h.t.Helper()
	r, err := h.store.Get(context.Background(), id)
	if err != nil {
		h.t.Fatalf("no result for %s: %v\nlogs:\n%s", id, err, h.logs.String())
	}
	return r
}

func request(t *testing.T, mutate func(m map[string]any)) (id string, body string) {
	t.Helper()
	id, _ = protocol.NewRequestID()
	now := time.Now().UTC()
	m := map[string]any{
		"version": 1, "request_id": id, "target": map[string]any{"host": "web01"},
		"command": "systemctl", "args": []string{"status", "nginx.service"},
		"issued_at": protocol.FormatTime(now), "not_before": nil,
		"expires_at": protocol.FormatTime(now.Add(5 * time.Minute)), "timeout_seconds": 30,
		"issuer": "arn:aws:iam::123456789012:user/operator",
	}
	if mutate != nil {
		mutate(m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return id, string(b)
}

func TestSuccessfulExecution(t *testing.T) {
	h := newHarness(t)
	id, body := request(t, nil)
	h.queue.Send(body)
	stop := h.run()
	h.waitFor("message deleted", func() bool { return h.queue.Len() == 0 })
	stop()

	r := h.result(id)
	if r.Status != protocol.StatusSucceeded || r.ExitCode == nil || *r.ExitCode != 0 ||
		!strings.Contains(r.Stdout, "active") || r.Issuer != "arn:aws:iam::123456789012:user/operator" ||
		r.Host != "web01" || r.Attempt != 1 || r.AgentVersion != "test" || r.TTL == 0 || r.CompletedAt == "" {
		t.Fatalf("unexpected result: %+v", r)
	}
	if h.executions() != 1 {
		t.Fatalf("executions = %d", h.executions())
	}
	if got := testutil.ToFloat64(h.metrics.Commands.WithLabelValues("systemctl", "succeeded")); got != 1 {
		t.Fatalf("commands metric = %v", got)
	}
	if testutil.ToFloat64(h.metrics.MessagesReceived) != 1 || testutil.ToFloat64(h.metrics.LastSuccessfulPoll) == 0 {
		t.Fatal("received/poll metrics not updated")
	}
}

func TestEveryRejectionIsRecordedAndDeleted(t *testing.T) {
	past := protocol.FormatTime(time.Now().Add(-time.Minute))
	future := protocol.FormatTime(time.Now().Add(time.Hour))
	cases := []struct {
		name   string
		mutate func(m map[string]any)
		raw    string
		reason protocol.Reason
		status protocol.Status
	}{
		{name: "unsupported_version", mutate: func(m map[string]any) { m["version"] = 2 }, reason: protocol.ReasonUnsupportedVersion},
		{name: "malformed: missing expires_at", mutate: func(m map[string]any) { delete(m, "expires_at") }, reason: protocol.ReasonMalformed},
		{name: "malformed: clock skew", mutate: func(m map[string]any) { m["issued_at"] = future }, reason: protocol.ReasonMalformed},
		{name: "invalid_target", mutate: func(m map[string]any) { m["target"] = map[string]any{"host": "web02"} }, reason: protocol.ReasonInvalidTarget},
		{name: "unknown_command", mutate: func(m map[string]any) { m["command"] = "rm" }, reason: protocol.ReasonUnknownCommand},
		{name: "command_not_allowed: disabled puppet", mutate: func(m map[string]any) {
			m["command"] = "puppet"
			m["args"] = []string{}
		}, reason: protocol.ReasonCommandNotAllowed},
		{name: "arg_not_allowed", mutate: func(m map[string]any) { m["args"] = []string{"restart", "nginx.service"} }, reason: protocol.ReasonArgNotAllowed},
		{name: "invalid_timeout", mutate: func(m map[string]any) { m["timeout_seconds"] = 0 }, reason: protocol.ReasonInvalidTimeout},
		{name: "not_yet_valid", mutate: func(m map[string]any) { m["not_before"] = future; m["expires_at"] = future }, reason: protocol.ReasonNotYetValid},
		{name: "expired", mutate: func(m map[string]any) { m["expires_at"] = past }, reason: protocol.ReasonExpired, status: protocol.StatusExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			id, body := request(t, tc.mutate)
			msgID := h.queue.Send(body)
			stop := h.run()
			h.waitFor("message deleted", func() bool { return h.queue.Len() == 0 })
			stop()

			r := h.result(id)
			want := tc.status
			if want == "" {
				want = protocol.StatusRejected
			}
			if r.Status != want || r.RejectionReason == nil || *r.RejectionReason != tc.reason {
				t.Fatalf("got status %s reason %v, want %s/%s", r.Status, r.RejectionReason, want, tc.reason)
			}
			if r.Issuer != "arn:aws:iam::123456789012:user/operator" {
				t.Fatalf("issuer must be recorded unchanged: %q", r.Issuer)
			}
			if h.executions() != 0 {
				t.Fatal("a rejected request spawned a process")
			}
			if _, deleted, _ := h.queue.Snapshot(); len(deleted) != 1 || deleted[0] != msgID {
				t.Fatalf("rejected message must be deleted exactly once: %v", deleted)
			}
			if testutil.ToFloat64(h.metrics.Rejections.WithLabelValues(string(tc.reason))) != 1 {
				t.Fatal("rejection not counted")
			}
		})
	}
}

func TestInvalidTargetFiresEvenWhenTheBrokerDeliveredIt(t *testing.T) {
	// A misconfigured subscription: the bus routes web02's traffic to web01's queue.
	h := newHarness(t)
	bus := &awsxfake.Bus{}
	bus.Subscribe(h.queue, "web01", "web02")
	id, body := request(t, func(m map[string]any) { m["target"] = map[string]any{"host": "web02"} })
	if _, err := bus.Publish(context.Background(), []byte(body), awsx.Routing{Host: "web02", Command: "systemctl", ProtocolVersion: 1}); err != nil {
		t.Fatal(err)
	}
	stop := h.run()
	h.waitFor("message deleted", func() bool { return h.queue.Len() == 0 })
	stop()
	if r := h.result(id); *r.RejectionReason != protocol.ReasonInvalidTarget || h.executions() != 0 {
		t.Fatalf("misrouted command must be refused: %+v", r)
	}
}

func TestMalformedWithoutRequestIDIsStillRecorded(t *testing.T) {
	h := newHarness(t)
	msgID := h.queue.Send("{not json")
	stop := h.run()
	h.waitFor("message deleted", func() bool { return h.queue.Len() == 0 })
	stop()
	r := h.result("sqs-message:" + msgID)
	if *r.RejectionReason != protocol.ReasonMalformed {
		t.Fatalf("got %+v", r)
	}
}

func TestDuplicateDeliverySequential(t *testing.T) {
	h := newHarness(t)
	id, body := request(t, nil)
	h.queue.Send(body)
	stop := h.run()
	h.waitFor("first delivery", func() bool { return h.queue.Len() == 0 })
	h.queue.Send(body)
	h.waitFor("second delivery", func() bool { return h.queue.Len() == 0 })
	stop()

	if h.executions() != 1 {
		t.Fatalf("executions = %d, want exactly 1", h.executions())
	}
	if r := h.result(id); r.Status != protocol.StatusSucceeded {
		t.Fatalf("the duplicate must not overwrite the real result: %+v", r)
	}
	if testutil.ToFloat64(h.metrics.Rejections.WithLabelValues("duplicate")) != 1 {
		t.Fatal("duplicate not counted")
	}
}

func TestDuplicateDeliveryConcurrent(t *testing.T) {
	h := newHarness(t)
	id, body := request(t, func(m map[string]any) { m["args"] = []string{"status", "slow.service"} })
	for range 4 {
		h.queue.Send(body)
	}
	stop := h.run()
	h.waitFor("all deliveries handled", func() bool { return h.queue.Len() == 0 })
	stop()
	if h.executions() != 1 {
		t.Fatalf("executions = %d, want exactly 1", h.executions())
	}
	if r := h.result(id); r.Status != protocol.StatusSucceeded {
		t.Fatalf("got %+v", r)
	}
	if got := testutil.ToFloat64(h.metrics.Rejections.WithLabelValues("duplicate")); got != 3 {
		t.Fatalf("duplicates = %v, want 3", got)
	}
}

func TestReceiveIsBoundedByFreeSlots(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxConcurrentCommands = 2 })
	for range 3 {
		_, body := request(t, func(m map[string]any) { m["args"] = []string{"status", "slow.service"} })
		h.queue.Send(body)
	}
	stop := h.run()
	h.waitFor("all handled", func() bool { return h.queue.Len() == 0 })
	stop()
	requested, _, _ := h.queue.Snapshot()
	for i, n := range requested {
		if n > 2 || n < 1 {
			t.Fatalf("receive %d requested %d messages with 2 slots", i, n)
		}
	}
	if h.executions() != 3 {
		t.Fatalf("executions = %d", h.executions())
	}
}

// overDeliveringQueue returns one more message than requested, which SQS
// never does; the agent must refuse the extra rather than exceed its limit.
type overDeliveringQueue struct {
	*awsxfake.Queue
	once sync.Once
}

func (q *overDeliveringQueue) Receive(ctx context.Context, n int, wait, vis time.Duration) ([]awsx.Message, error) {
	var out []awsx.Message
	var err error
	q.once.Do(func() { out, err = q.Queue.Receive(ctx, n+1, wait, vis) })
	if out == nil && err == nil {
		return q.Queue.Receive(ctx, n, wait, vis)
	}
	return out, err
}

func TestConcurrencyLimitRejection(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxConcurrentCommands = 1 })
	q := &overDeliveringQueue{Queue: h.queue}
	h.agent.queue = q
	first, b1 := request(t, func(m map[string]any) { m["args"] = []string{"status", "slow.service"} })
	second, b2 := request(t, nil)
	h.queue.Send(b1)
	h.queue.Send(b2)
	stop := h.run()
	h.waitFor("both handled", func() bool { return h.queue.Len() == 0 })
	stop()
	if r := h.result(first); r.Status != protocol.StatusSucceeded {
		t.Fatalf("first: %+v", r)
	}
	if r := h.result(second); r.Status != protocol.StatusRejected || *r.RejectionReason != protocol.ReasonConcurrencyLimit {
		t.Fatalf("second: %+v", r)
	}
}

func TestReceiveErrorsBackOffAndRecover(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	failures := 4
	h.queue.ReceiveErr = func() error {
		mu.Lock()
		defer mu.Unlock()
		if failures > 0 {
			failures--
			return errors.New("connection refused")
		}
		return nil
	}
	id, body := request(t, nil)
	h.queue.Send(body)
	stop := h.run()
	h.waitFor("message handled after outage", func() bool { return h.queue.Len() == 0 })
	stop()
	if h.result(id).Status != protocol.StatusSucceeded {
		t.Fatal("agent did not resume after receive errors")
	}
	if got := testutil.ToFloat64(h.metrics.AWSErrors.WithLabelValues("ReceiveMessage")); got != 4 {
		t.Fatalf("receive errors counted = %v, want 4", got)
	}
	if !strings.Contains(h.logs.String(), "backing off") {
		t.Fatal("backoff not logged")
	}
}

func TestClaimFailureDoesNotExecuteOrDelete(t *testing.T) {
	h := newHarness(t)
	h.agent.store = failingStore{h.store}
	_, body := request(t, nil)
	h.queue.Send(body)
	stop := h.run()
	h.waitFor("claim attempted", func() bool { return strings.Contains(h.logs.String(), "could not claim") })
	stop()
	if h.executions() != 0 || h.queue.Len() != 1 {
		t.Fatalf("unclaimed request must not run and must stay queued: exec=%d len=%d", h.executions(), h.queue.Len())
	}
	if testutil.ToFloat64(h.metrics.ResultWriteFailures.WithLabelValues("claim")) == 0 {
		t.Fatal("claim failure not counted")
	}
}

type failingStore struct{ results.ResultStore }

func (failingStore) Claim(context.Context, protocol.Result) error {
	return errors.New("dynamodb unreachable")
}

func TestLogsCarryRequestIDAndNoOutputAtInfo(t *testing.T) {
	h := newHarness(t)
	id, body := request(t, nil)
	h.queue.Send(body)
	stop := h.run()
	h.waitFor("handled", func() bool { return h.queue.Len() == 0 })
	stop()

	var sawCompleted, sawDebugOutput bool
	for _, line := range strings.Split(strings.TrimSpace(h.logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		msg := rec["msg"].(string)
		if strings.HasPrefix(msg, "receive loop") || strings.HasPrefix(msg, "shutting down") || strings.HasPrefix(msg, "drained") {
			continue
		}
		if rec["request_id"] != id {
			t.Errorf("log line without request_id: %s", line)
		}
		if rec["level"] != "DEBUG" && strings.Contains(line, "SECRET-OUTPUT") {
			t.Errorf("command output logged above debug: %s", line)
		}
		if msg == "command completed" {
			sawCompleted = rec["status"] == "succeeded" && rec["issuer"] != nil
		}
		if rec["level"] == "DEBUG" && strings.Contains(line, "SECRET-OUTPUT") {
			sawDebugOutput = true
		}
	}
	if !sawCompleted || !sawDebugOutput {
		t.Fatalf("missing completion or debug output line:\n%s", h.logs.String())
	}
}

func TestShutdownKillsAfterGraceAndRecordsFailed(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ShutdownGrace = config.Duration(300 * time.Millisecond) })
	id, body := request(t, func(m map[string]any) { m["args"] = []string{"status", "hang.service"} })
	h.queue.Send(body)
	stop := h.run()
	h.waitFor("command running", func() bool { return h.executions() == 1 })
	start := time.Now()
	stop()
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("shutdown took %s", el)
	}
	r := h.result(id)
	if r.Status != protocol.StatusFailed || r.Error == nil || !strings.Contains(*r.Error, "shutting down") {
		t.Fatalf("a command killed at shutdown must be recorded failed: %+v", r)
	}
}

func TestShutdownDrainsWithinGrace(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.ShutdownGrace = config.Duration(10 * time.Second) })
	id, body := request(t, func(m map[string]any) { m["args"] = []string{"status", "slow.service"} })
	h.queue.Send(body)
	stop := h.run()
	h.waitFor("command running", func() bool { return h.executions() == 1 })
	stop()
	if r := h.result(id); r.Status != protocol.StatusSucceeded {
		t.Fatalf("in-flight command within grace must finish normally: %+v", r)
	}
}

func TestUnstartedMessageReturnedOnShutdown(t *testing.T) {
	h := newHarness(t)
	id, body := request(t, nil)
	h.queue.Send(body)
	msgs, _ := h.queue.Receive(context.Background(), 1, 0, time.Minute)
	h.agent.stopping.Store(true)
	h.agent.handle(msgs[0])

	if _, err := h.store.Get(context.Background(), id); !errors.Is(err, results.ErrNotFound) {
		t.Fatal("an unstarted message must not be claimed during shutdown")
	}
	_, _, vis := h.queue.Snapshot()
	if len(vis) != 1 || vis[0] != 0 || h.queue.Len() != 1 || h.executions() != 0 {
		t.Fatalf("expected ChangeMessageVisibility(0) and no execution: vis=%v len=%d", vis, h.queue.Len())
	}
	// And it is immediately receivable again.
	if again, _ := h.queue.Receive(context.Background(), 1, 0, time.Minute); len(again) != 1 {
		t.Fatal("returned message is not visible")
	}
}

func TestVisibilityExtendedForLongCommand(t *testing.T) {
	// Deliberately violates the derived constraint so a command can outlive
	// one visibility timeout; config validation forbids this in production.
	h := newHarness(t, func(c *config.Config) {
		c.VisibilityTimeout = config.Duration(1500 * time.Millisecond)
		c.MaxCommandTimeout = config.Duration(10 * time.Second)
	})
	id, body := request(t, func(m map[string]any) {
		m["args"] = []string{"status", "slow.service"} // runs 2.5s
		m["timeout_seconds"] = 5
	})
	msgID := h.queue.Send(body)
	stop := h.run()
	h.waitFor("handled", func() bool { return h.queue.Len() == 0 })
	stop()
	_, deleted, vis := h.queue.Snapshot()
	if len(vis) == 0 {
		t.Fatal("no visibility extension for a command outliving the visibility timeout")
	}
	if h.executions() != 1 || len(deleted) != 1 || deleted[0] != msgID {
		t.Fatalf("expected one execution and one delete, got exec=%d deleted=%v", h.executions(), deleted)
	}
	if r := h.result(id); r.Status != protocol.StatusSucceeded || r.Attempt != 1 {
		t.Fatalf("got %+v", r)
	}
}

func TestDryRunRecordsWithoutSpawning(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { f := false; c.Execute = &f })
	id, body := request(t, nil)
	h.queue.Send(body)
	stop := h.run()
	h.waitFor("handled", func() bool { return h.queue.Len() == 0 })
	stop()
	r := h.result(id)
	if r.Status != protocol.StatusSucceeded || !strings.Contains(r.Stdout, "nginx.service") || h.executions() != 0 {
		t.Fatalf("dry run must record the argv and spawn nothing: %+v exec=%d", r, h.executions())
	}
	if !strings.Contains(h.logs.String(), "dry run: not executing") {
		t.Fatal("dry run must log the argv")
	}
}
