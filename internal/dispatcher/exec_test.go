package dispatcher

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"sqsh/internal/protocol"
)

func newExec() *Executor { return &Executor{MaxOutputBytes: 4096, KillGrace: 200 * time.Millisecond} }

func withTimeout(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func which(t *testing.T, candidates ...string) string {
	t.Helper()
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	t.Skipf("none of %v present", candidates)
	return ""
}

func TestEnvironmentIsNotInherited(t *testing.T) {
	t.Setenv("SQSH_SECRET_TOKEN", "leak-me")
	env := which(t, "/usr/bin/env", "/bin/env")
	o := newExec().Run(withTimeout(t, 5*time.Second), Spec{Path: env})
	if o.Status() != protocol.StatusSucceeded {
		t.Fatalf("env failed: %+v", o)
	}
	got := strings.Split(strings.TrimSpace(o.Stdout), "\n")
	slices.Sort(got)
	want := slices.Sorted(slices.Values(Env))
	if !slices.Equal(got, want) {
		t.Fatalf("child environment = %q, want exactly %q", got, want)
	}
}

func TestStdinIsEmpty(t *testing.T) {
	cat := which(t, "/usr/bin/cat", "/bin/cat")
	o := newExec().Run(withTimeout(t, 5*time.Second), Spec{Path: cat})
	if o.Status() != protocol.StatusSucceeded || o.Stdout != "" || o.Duration > 2*time.Second {
		t.Fatalf("cat should see EOF immediately: %+v", o)
	}
}

func TestMetacharactersAreLiteral(t *testing.T) {
	echo := which(t, "/usr/bin/echo", "/bin/echo")
	marker := filepath.Join(t.TempDir(), "pwned")
	arg := "x; touch " + marker + " | $(touch " + marker + ")\n`touch " + marker + "`"
	o := newExec().Run(withTimeout(t, 5*time.Second), Spec{Path: echo, Args: []string{arg}})
	if o.Stdout != arg+"\n" {
		t.Fatalf("argument was not passed literally: %q", o.Stdout)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("shell metacharacters caused a process to run")
	}
}

func TestNonAbsolutePathRefused(t *testing.T) {
	o := newExec().Run(context.Background(), Spec{Path: "echo", Args: []string{"hi"}})
	if o.Err == nil || o.Status() != protocol.StatusFailed {
		t.Fatalf("relative argv[0] must be refused: %+v", o)
	}
}

func TestNonZeroExitIsFailedWithCode(t *testing.T) {
	f := which(t, "/usr/bin/false", "/bin/false")
	o := newExec().Run(withTimeout(t, 5*time.Second), Spec{Path: f})
	if o.Status() != protocol.StatusFailed || o.ExitCode == nil || *o.ExitCode != 1 || o.Err != nil {
		t.Fatalf("want failed with exit 1: %+v", o)
	}
}

func TestDeclaredSuccessCodes(t *testing.T) {
	f := which(t, "/usr/bin/false", "/bin/false")
	o := newExec().Run(withTimeout(t, 5*time.Second), Spec{Path: f, SuccessCodes: []int{0, 1}})
	if o.Status() != protocol.StatusSucceeded || *o.ExitCode != 1 {
		t.Fatalf("exit 1 declared as success: %+v", o)
	}
}

func TestTimeoutKillsAndRecordsTimedOut(t *testing.T) {
	sleep := which(t, "/usr/bin/sleep", "/bin/sleep")
	start := time.Now()
	o := newExec().Run(withTimeout(t, 200*time.Millisecond), Spec{Path: sleep, Args: []string{"30"}})
	if o.Status() != protocol.StatusTimedOut || !o.TimedOut {
		t.Fatalf("want timed_out: %+v", o)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("process was not terminated promptly")
	}
}

func TestTimeoutEscalatesToKill(t *testing.T) {
	sh := which(t, "/bin/sh", "/usr/bin/sh")
	// The test fixture itself uses a shell to build a SIGTERM-ignoring process;
	// the executor under test still invokes it by absolute path with fixed argv.
	start := time.Now()
	o := newExec().Run(withTimeout(t, 200*time.Millisecond), Spec{Path: sh, Args: []string{"-c", `trap "" TERM; while :; do sleep 0.05; done`}})
	el := time.Since(start)
	if o.Status() != protocol.StatusTimedOut {
		t.Fatalf("want timed_out: %+v", o)
	}
	if el < 350*time.Millisecond || el > 4*time.Second {
		t.Fatalf("expected SIGKILL after the 200ms grace, took %s", el)
	}
}

func TestShutdownCancelIsNotATimeout(t *testing.T) {
	sleep := which(t, "/usr/bin/sleep", "/bin/sleep")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	o := newExec().Run(ctx, Spec{Path: sleep, Args: []string{"30"}})
	if o.TimedOut || o.Err == nil || o.Status() != protocol.StatusFailed {
		t.Fatalf("shutdown cancellation must record failed, not timed_out: %+v", o)
	}
}

func TestOutputIsBoundedAndFlagged(t *testing.T) {
	head := which(t, "/usr/bin/head", "/bin/head")
	o := newExec().Run(withTimeout(t, 10*time.Second), Spec{Path: head, Args: []string{"-c", "20000000", "/dev/zero"}})
	if o.Status() != protocol.StatusSucceeded {
		t.Fatalf("head failed: %+v", o.Err)
	}
	if len(o.Stdout) != 4096 || !o.Truncated {
		t.Fatalf("stdout len %d truncated=%v, want 4096/true", len(o.Stdout), o.Truncated)
	}
}

func TestStartFailureIsFailed(t *testing.T) {
	o := newExec().Run(context.Background(), Spec{Path: "/nonexistent/sqsh-test-binary"})
	if o.Err == nil || o.Status() != protocol.StatusFailed {
		t.Fatalf("missing binary must fail: %+v", o)
	}
}

func TestDryRunDoesNotSpawn(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "spawned")
	e := newExec()
	e.DryRun = true
	o := e.Run(context.Background(), Spec{Path: "/usr/bin/touch", Args: []string{marker}})
	if o.Status() != protocol.StatusSucceeded || !strings.Contains(o.Stdout, marker) {
		t.Fatalf("dry run should report the argv: %+v", o)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("dry run spawned a process")
	}
}
