//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"sqsh/internal/config"
	"sqsh/internal/protocol"
	"sqsh/internal/results"
	"sqsh/internal/results/resultstest"
)

func resultWait() time.Duration {
	if realAWS() {
		return 90 * time.Second
	}
	return 30 * time.Second
}

// TestIsolation validates this change's premise (§3, §6.1): server-side
// filter policies deliver a command only to its target's queue. Each agent
// must receive exactly its own requests and nothing else.
func TestIsolation(t *testing.T) {
	hosts := make([]string, FleetSize)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("web%02d", i+1)
	}
	e := newEnv(t, hosts...)
	agents := map[string]*runningAgent{}
	for _, h := range hosts {
		agents[h] = e.startAgent(h, nil)
	}

	want := map[string][]string{}
	for round := range 2 {
		for _, h := range hosts {
			r, body := request(t, h, "status", "nginx.service")
			e.publish(r, body)
			res := e.waitResult(r.RequestID, resultWait())
			if res.Status != protocol.StatusSucceeded || res.Host != h || !strings.Contains(res.Stdout, h+" active") {
				t.Fatalf("round %d: request for %s produced %+v", round, h, res)
			}
			want[h] = append(want[h], r.RequestID)
		}
	}

	// A host with no subscription: nobody may receive it.
	orphan, body := request(t, "web99", "status", "nginx.service")
	e.publish(orphan, body)
	time.Sleep(5 * time.Second)

	for _, h := range hosts {
		got := agents[h].queue.requestIDs()
		slices.Sort(got)
		w := slices.Clone(want[h])
		slices.Sort(w)
		if !slices.Equal(got, w) {
			t.Errorf("%s received %v, want exactly its own %v", h, got, w)
		}
		if n := agents[h].executions(); n != len(w) {
			t.Errorf("%s executed %d commands, want %d", h, n, len(w))
		}
		if slices.Contains(got, orphan.RequestID) {
			t.Errorf("%s received a command addressed to an unsubscribed host", h)
		}
		if got := testutil.ToFloat64(agents[h].metrics.Rejections.WithLabelValues(string(protocol.ReasonInvalidTarget))); got != 0 {
			t.Errorf("%s saw %v misrouted messages; the filter policy let foreign traffic through", h, got)
		}
		if d := e.queueDepth(h); d != 0 {
			t.Errorf("%s queue still holds %d messages", h, d)
		}
	}
	if _, err := e.store().Get(context.Background(), orphan.RequestID); !errors.Is(err, results.ErrNotFound) {
		t.Errorf("unrouted request produced a result: %v", err)
	}
}

// buildCLI compiles the sqsh binary once per test run.
func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sqsh")
	cmd := exec.Command("go", "build", "-o", bin, "sqsh/cmd/sqsh")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sqsh: %v\n%s", err, out)
	}
	return bin
}

// TestEndToEnd drives the real CLI binary: publish → route → dispatch →
// result → CLI report, with the exit code matching the recorded status.
func TestEndToEnd(t *testing.T) {
	e := newEnv(t, "web01")
	e.startAgent("web01", nil)
	bin := buildCLI(t)

	cases := []struct {
		name     string
		args     []string
		status   protocol.Status
		exitCode int
	}{
		{"succeeded", []string{"status", "nginx"}, protocol.StatusSucceeded, 0},
		{"failed", []string{"status", "broken.service"}, protocol.StatusFailed, 1},
		{"rejected", []string{"restart", "nginx"}, protocol.StatusRejected, 3},
		{"rejected unit", []string{"status", "docker.service"}, protocol.StatusRejected, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--json", "--wait-timeout", resultWait().String(), "--topic-arn", e.topicARN,
				"--results-table", e.table, "systemctl", "web01"}, tc.args...)
			cmd := exec.Command(bin, args...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			code := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if code != tc.exitCode {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, tc.exitCode, &stdout, &stderr)
			}
			var res protocol.Result
			if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
				t.Fatalf("--json output: %v\n%s", err, &stdout)
			}
			if res.Status != tc.status {
				t.Fatalf("status %s, want %s", res.Status, tc.status)
			}
			stored := e.waitResult(res.RequestID, 5*time.Second)
			if stored.Status != res.Status {
				t.Fatalf("CLI reported %s but the store holds %s", res.Status, stored.Status)
			}

			// `sqsh status` reports the same record with the same exit code.
			st := exec.Command(bin, "--results-table", e.table, "status", res.RequestID)
			st.Stdout, st.Stderr = os.Stderr, os.Stderr
			err = st.Run()
			code = 0
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			}
			if code != tc.exitCode {
				t.Fatalf("sqsh status: exit %d, want %d", code, tc.exitCode)
			}
		})
	}
}

// TestDuplicateDelivery delivers one request_id twice, sequentially and
// concurrently, and asserts exactly one execution each time.
//
// Each subtest gets its own queue: a stopped agent's abandoned long poll can
// still claim a message server-side, which would hide it from the next agent
// for a full visibility timeout.
func TestDuplicateDelivery(t *testing.T) {
	t.Run("sequential", func(t *testing.T) {
		e := newEnv(t, "web01")
		a := e.startAgent("web01", nil)
		defer a.stop()
		r, body := request(t, "web01", "status", "nginx.service")
		e.publish(r, body)
		first := e.waitResult(r.RequestID, resultWait())
		e.sendDirect("web01", body)
		waitUntil(t, "duplicate handled", resultWait(), func() bool {
			return testutil.ToFloat64(a.metrics.Rejections.WithLabelValues("duplicate")) == 1
		})
		if a.executions() != 1 {
			t.Fatalf("executions = %d, want 1", a.executions())
		}
		if again := e.waitResult(r.RequestID, time.Second); again.Status != first.Status || again.CompletedAt != first.CompletedAt {
			t.Fatalf("duplicate overwrote the result: %+v vs %+v", again, first)
		}
		waitUntil(t, "queue drained", resultWait(), func() bool { return e.queueDepth("web01") == 0 })
	})

	t.Run("concurrent", func(t *testing.T) {
		e := newEnv(t, "web01")
		r, body := request(t, "web01", "status", "slow.service")
		for range 2 {
			e.sendDirect("web01", body)
		}
		// Both copies are queued before the agent starts, so its first
		// receive batch can hold both and handle them in parallel.
		a := e.startAgent("web01", nil)
		defer a.stop()
		e.waitResult(r.RequestID, resultWait())
		waitUntil(t, "duplicate handled", resultWait(), func() bool {
			return testutil.ToFloat64(a.metrics.Rejections.WithLabelValues("duplicate")) == 1
		})
		if a.executions() != 1 {
			t.Fatalf("executions = %d, want 1", a.executions())
		}
		waitUntil(t, "queue drained", resultWait(), func() bool { return e.queueDepth("web01") == 0 })
	})
}

// TestVisibilityExtension runs a command longer than one visibility timeout
// and asserts the message was extended rather than redelivered.
func TestVisibilityExtension(t *testing.T) {
	e := newEnv(t, "web01")
	a := e.startAgent("web01", func(c *config.Config) {
		// Violates the derived constraint on purpose: in production a command
		// can never outlive one visibility timeout.
		c.VisibilityTimeout = config.Duration(2 * time.Second)
		c.MaxCommandTimeout = config.Duration(10 * time.Second)
	})
	r, body := request(t, "web01", "status", "slow.service") // runs 5s
	e.publish(r, body)
	res := e.waitResult(r.RequestID, resultWait())
	if res.Status != protocol.StatusSucceeded || res.Attempt != 1 {
		t.Fatalf("got %+v", res)
	}
	if got := testutil.ToFloat64(a.metrics.VisibilityExtensions.WithLabelValues("systemctl")); got < 2 {
		t.Fatalf("visibility extensions = %v, want >= 2", got)
	}
	time.Sleep(3 * time.Second) // past one more visibility window
	if a.executions() != 1 || len(a.queue.requestIDs()) != 1 {
		t.Fatalf("message was redelivered: executions=%d receives=%d", a.executions(), len(a.queue.requestIDs()))
	}
	if d := e.queueDepth("web01"); d != 0 {
		t.Fatalf("queue depth %d after completion", d)
	}
}

// TestDynamoConformance runs the shared ResultStore suite against DynamoDB.
func TestDynamoConformance(t *testing.T) {
	e := newEnv(t)
	resultstest.Run(t, func(*testing.T) results.ResultStore {
		return results.NewDynamo(e.ddb, e.table, resultstest.Retention)
	})
}
