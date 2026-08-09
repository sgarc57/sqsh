package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"sqsh/internal/awsx"
	"sqsh/internal/awsx/awsxfake"
	"sqsh/internal/protocol"
	"sqsh/internal/results"
)

type testEnv struct {
	cli      *cli
	bus      *awsxfake.Bus
	queue    *awsxfake.Queue
	store    *results.Memory
	out, err *bytes.Buffer
	sleeps   []time.Duration
	clock    time.Time
	connects int
	getErr   error
	// onPoll is called on every result read, to script the agent side.
	onPoll func(id string)
}

type scriptedStore struct{ e *testEnv }

func (s scriptedStore) Get(ctx context.Context, id string) (protocol.Result, error) {
	if s.e.onPoll != nil {
		s.e.onPoll(id)
	}
	if s.e.getErr != nil {
		return protocol.Result{}, s.e.getErr
	}
	return s.e.store.Get(ctx, id)
}

func newEnv(t *testing.T) *testEnv {
	t.Setenv("SQSH_TOPIC_ARN", "arn:aws:sns:eu-central-1:123456789012:sqsh-commands")
	e := &testEnv{bus: &awsxfake.Bus{}, queue: awsxfake.NewQueue(), store: results.NewMemory(time.Hour),
		out: &bytes.Buffer{}, err: &bytes.Buffer{}, clock: time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)}
	e.bus.Subscribe(e.queue, "web01")
	e.cli = &cli{
		stdout: e.out, stderr: e.err, now: func() time.Time { return e.clock },
		sleep: func(_ context.Context, d time.Duration) error {
			e.sleeps = append(e.sleeps, d)
			e.clock = e.clock.Add(d)
			return nil
		},
		connect: func(context.Context, options, bool) (*backend, error) {
			e.connects++
			return &backend{publisher: e.bus, results: scriptedStore{e},
				issuer: func(context.Context) (string, error) { return "arn:aws:iam::123456789012:user/op", nil }}, nil
		},
	}
	return e
}

func (e *testEnv) run(args ...string) int { return e.cli.run(context.Background(), args) }

// published returns the requests delivered to web01's queue.
func (e *testEnv) published(t *testing.T) []protocol.Request {
	t.Helper()
	msgs, _ := e.queue.Receive(context.Background(), 10, 0, time.Hour)
	var out []protocol.Request
	for _, m := range msgs {
		r, rej := protocol.Parse([]byte(m.Body))
		if rej != nil {
			t.Fatalf("CLI published an invalid request: %v", rej)
		}
		out = append(out, r)
	}
	return out
}

// completeWith makes the next poll find a terminal result for the request.
func (e *testEnv) completeWith(r protocol.Result) {
	e.onPoll = func(id string) {
		r.RequestID = id
		_ = e.store.Complete(context.Background(), r)
	}
}

func TestExitCodeForEveryTerminalStatus(t *testing.T) {
	zero, one := 0, 1
	reason := protocol.ReasonArgNotAllowed
	cases := []struct {
		r    protocol.Result
		want int
	}{
		{protocol.Result{Status: protocol.StatusSucceeded, ExitCode: &zero}, 0},
		{protocol.Result{Status: protocol.StatusFailed, ExitCode: &one}, 1},
		{protocol.Result{Status: protocol.StatusTimedOut}, 1},
		{protocol.Result{Status: protocol.StatusRejected, RejectionReason: &reason}, 3},
		{protocol.Result{Status: protocol.StatusExpired}, 3},
	}
	for _, tc := range cases {
		t.Run(string(tc.r.Status), func(t *testing.T) {
			e := newEnv(t)
			e.completeWith(tc.r)
			if got := e.run("systemctl", "web01", "status", "nginx"); got != tc.want {
				t.Fatalf("exit %d, want %d\nstdout:%s\nstderr:%s", got, tc.want, e.out, e.err)
			}
			if tc.r.Status == protocol.StatusRejected && !strings.Contains(e.out.String(), "arg_not_allowed") {
				t.Fatalf("rejection reason must be shown:\n%s", e.out)
			}

			// `status` maps the same record to the same code.
			id := e.published(t)[0].RequestID
			e2 := newEnv(t)
			e2.store = e.store
			if got := e2.run("status", id); got != tc.want {
				t.Fatalf("status: exit %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPublishedRequestShapeAndRouting(t *testing.T) {
	e := newEnv(t)
	if code := e.run("--no-wait", "--timeout", "45", "--expires-in", "10m", "systemctl", "web01", "is-active", "sshd.service"); code != 0 {
		t.Fatalf("exit %d: %s", code, e.err)
	}
	reqs := e.published(t)
	if len(reqs) != 1 {
		t.Fatalf("published %d", len(reqs))
	}
	r := reqs[0]
	if r.Target.Host != "web01" || r.Command != "systemctl" || strings.Join(r.Args, " ") != "is-active sshd.service" ||
		r.TimeoutSeconds != 45 || !r.ExpiresAt.Equal(e.clock.Add(10*time.Minute)) || r.Issuer != "arn:aws:iam::123456789012:user/op" {
		t.Fatalf("unexpected request: %+v", r)
	}
	if rt := e.bus.Published[0]; rt != (awsx.Routing{Host: "web01", Command: "systemctl", ProtocolVersion: 1}) {
		t.Fatalf("routing attributes = %+v", rt)
	}
	if strings.TrimSpace(e.out.String()) != r.RequestID {
		t.Fatalf("--no-wait must print the request_id, got %q", e.out)
	}
	if len(e.sleeps) != 0 {
		t.Fatal("--no-wait must not poll")
	}
}

func TestDefaultExpiryIsFiveMinutes(t *testing.T) {
	e := newEnv(t)
	e.run("--no-wait", "systemctl", "web01", "status", "nginx")
	r := e.published(t)[0]
	if !r.ExpiresAt.Equal(e.clock.Add(5*time.Minute)) || r.TimeoutSeconds != 30 {
		t.Fatalf("defaults: expires %s timeout %d", r.ExpiresAt, r.TimeoutSeconds)
	}
}

func TestEachInvocationGetsAFreshRequestID(t *testing.T) {
	e := newEnv(t)
	e.run("--no-wait", "systemctl", "web01", "status", "nginx")
	e.run("--no-wait", "systemctl", "web01", "status", "nginx")
	reqs := e.published(t)
	if len(reqs) != 2 || reqs[0].RequestID == reqs[1].RequestID || !protocol.IsUUIDv7(reqs[0].RequestID) {
		t.Fatalf("ids: %+v", reqs)
	}
}

func TestFlagsAfterSubcommand(t *testing.T) {
	e := newEnv(t)
	if code := e.run("systemctl", "web01", "status", "nginx", "--no-wait", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, e.err)
	}
	var v map[string]string
	if err := json.Unmarshal(e.out.Bytes(), &v); err != nil || v["status"] != "published" || !protocol.IsUUIDv7(v["request_id"]) {
		t.Fatalf("json no-wait output: %q %v", e.out, err)
	}
}

func TestWaitTimeoutWordingAndExit(t *testing.T) {
	e := newEnv(t)
	code := e.run("--wait-timeout", "10s", "systemctl", "web01", "status", "nginx")
	if code != 4 {
		t.Fatalf("exit %d, want 4", code)
	}
	id := e.published(t)[0].RequestID
	msg := e.err.String()
	for _, want := range []string{"stopped waiting after 10s", "may still be running", "NOT cancelled", id, "sqsh status " + id} {
		if !strings.Contains(msg, want) {
			t.Errorf("wait-timeout message missing %q:\n%s", want, msg)
		}
	}
	for _, bad := range []string{"was cancelled", "was stopped", "aborted", "killed"} {
		if strings.Contains(strings.ToLower(msg), bad) {
			t.Errorf("wait-timeout message implies cancellation (%q):\n%s", bad, msg)
		}
	}
	var total time.Duration
	for _, s := range e.sleeps {
		total += s
	}
	if total != 10*time.Second {
		t.Fatalf("waited %s, want exactly the wait timeout", total)
	}
}

func TestWaitTimeoutJSON(t *testing.T) {
	e := newEnv(t)
	if code := e.run("--json", "--wait-timeout", "3s", "systemctl", "web01", "status", "nginx"); code != 4 {
		t.Fatalf("exit %d", code)
	}
	var v map[string]string
	if err := json.Unmarshal(e.out.Bytes(), &v); err != nil || v["status"] != "unknown" || !strings.Contains(v["resume"], "sqsh status") {
		t.Fatalf("json timeout output: %q %v", e.out, err)
	}
}

func TestPollBackoff(t *testing.T) {
	e := newEnv(t)
	e.run("--wait-timeout", "9s", "systemctl", "web01", "status", "nginx")
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second}
	for i, w := range want {
		if i >= len(e.sleeps) || e.sleeps[i] != w {
			t.Fatalf("sleeps = %v, want prefix %v", e.sleeps, want)
		}
	}
	for _, s := range e.sleeps {
		if s > 2*time.Second {
			t.Fatalf("poll interval %s above 2s", s)
		}
	}
}

func TestKeepsPollingWhileRunning(t *testing.T) {
	e := newEnv(t)
	polls := 0
	zero := 0
	e.onPoll = func(id string) {
		polls++
		if polls == 1 {
			_ = e.store.Claim(context.Background(), protocol.Result{RequestID: id, Status: protocol.StatusRunning})
		}
		if polls == 4 {
			_ = e.store.Complete(context.Background(), protocol.Result{RequestID: id, Status: protocol.StatusSucceeded, ExitCode: &zero})
		}
	}
	if code := e.run("systemctl", "web01", "status", "nginx"); code != 0 || polls != 4 {
		t.Fatalf("exit %d after %d polls", code, polls)
	}
}

func TestBackendFailuresExit6(t *testing.T) {
	e := newEnv(t)
	e.bus.PublishErr = errors.New("AccessDenied")
	if code := e.run("systemctl", "web01", "status", "nginx"); code != 6 || !strings.Contains(e.err.String(), "NOT sent") {
		t.Fatalf("publish failure: exit %d: %s", code, e.err)
	}

	e = newEnv(t)
	e.getErr = errors.New("dynamodb unreachable")
	code := e.run("--wait-timeout", "5s", "systemctl", "web01", "status", "nginx")
	if code != 6 || !strings.Contains(e.err.String(), "may still be running") {
		t.Fatalf("store failure must exit 6 without claiming the command failed: exit %d: %s", code, e.err)
	}
	if strings.Contains(e.out.String(), "failed") {
		t.Fatal("must not report the remote command as failed")
	}

	e = newEnv(t)
	e.cli.connect = func(context.Context, options, bool) (*backend, error) { return nil, errors.New("no credentials") }
	if code := e.run("status", "018f3a2c-6c1e-7b3a-9f2d-4a1b6c8e0d55"); code != 6 {
		t.Fatalf("connect failure: exit %d", code)
	}
}

func TestUsageErrorsExit2WithoutTouchingAWS(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"reboot", "web01"},
		{"systemctl", "web01", "status"},
		{"systemctl", "web01", "status", "nginx", "extra"},
		{"status"},
		{"status", "not-a-uuid"},
		{"--bogus-flag", "status", "x"},
		{"--timeout", "1.5s", "systemctl", "web01", "status", "nginx"},
		{"--wait-timeout", "0s", "systemctl", "web01", "status", "nginx"},
	} {
		e := newEnv(t)
		if code := e.run(args...); code != 2 || e.connects != 0 {
			t.Errorf("%q: exit %d, connects %d; want 2, 0", args, code, e.connects)
		}
	}
	t.Setenv("SQSH_TOPIC_ARN", "")
	e := newEnv(t)
	t.Setenv("SQSH_TOPIC_ARN", "")
	if code := e.run("systemctl", "web01", "status", "nginx"); code != 2 {
		t.Errorf("missing topic: exit %d", code)
	}
}

func TestStatusUnknownAndRunning(t *testing.T) {
	e := newEnv(t)
	id := "018f3a2c-6c1e-7b3a-9f2d-4a1b6c8e0d55"
	if code := e.run("status", id); code != 4 || !strings.Contains(e.err.String(), "no result recorded") {
		t.Fatalf("unknown id: exit %d: %s", code, e.err)
	}
	_ = e.store.Claim(context.Background(), protocol.Result{RequestID: id, Status: protocol.StatusRunning})
	if code := e.run("status", id); code != 4 || !strings.Contains(e.err.String(), "still running") {
		t.Fatalf("running: exit %d: %s", code, e.err)
	}
}

func TestTruncationIsSignalled(t *testing.T) {
	zero := 0
	r := protocol.Result{Status: protocol.StatusSucceeded, ExitCode: &zero, Stdout: "...tail\n", OutputTruncated: true}

	e := newEnv(t)
	e.completeWith(r)
	e.run("systemctl", "web01", "status", "nginx")
	if !strings.Contains(e.out.String(), "OUTPUT TRUNCATED") || !strings.Contains(e.out.String(), "incomplete") {
		t.Fatalf("human output must flag truncation:\n%s", e.out)
	}

	e = newEnv(t)
	e.completeWith(r)
	e.run("--json", "systemctl", "web01", "status", "nginx")
	var got protocol.Result
	if err := json.Unmarshal(e.out.Bytes(), &got); err != nil || !got.OutputTruncated {
		t.Fatalf("--json must emit the raw record with output_truncated: %q %v", e.out, err)
	}
	if !strings.Contains(e.err.String(), "output_truncated") {
		t.Fatalf("--json must also warn on stderr: %s", e.err)
	}
}

func TestVersion(t *testing.T) {
	e := newEnv(t)
	if code := e.run("--version"); code != 0 || !strings.HasPrefix(e.out.String(), "sqsh ") {
		t.Fatalf("exit %d %q", code, e.out)
	}
}
