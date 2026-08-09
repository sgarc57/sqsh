package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"sqsh/internal/output"
)

// Env is the complete environment of every executed command. Nothing is
// inherited from the agent, and LANG=C keeps output parsing locale-stable.
var Env = []string{
	"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
	"LANG=C",
	"HOME=/",
}

// DefaultKillGrace is how long a timed-out process has to exit after SIGTERM
// before it is sent SIGKILL (§9.3).
const DefaultKillGrace = 5 * time.Second

// Spec describes one process invocation.
type Spec struct {
	// Path is the absolute path of the program. It is also argv[0].
	Path string
	// Args are passed to the program verbatim, one argv element each.
	Args []string
	// SuccessCodes are the exit codes that count as success. Empty means {0}.
	SuccessCodes []int
}

// Executor runs processes with a controlled environment, bounded output, and
// a time limit. It never invokes a shell.
type Executor struct {
	MaxOutputBytes int
	KillGrace      time.Duration
	// DryRun logs the exact argv and reports success without spawning (§17).
	DryRun bool
}

type loggerKey struct{}

// WithLogger attaches a request-scoped logger for the executor to use.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

func loggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// Run executes s. The request timeout arrives as ctx's deadline: reaching it
// kills the process and reports TimedOut. Any other cancellation (agent
// shutdown) stops the process the same way but is reported as Err.
func (e *Executor) Run(ctx context.Context, s Spec) Outcome {
	start := time.Now()
	out := Outcome{StartedAt: start}
	if !filepath.IsAbs(s.Path) {
		// A programming error in a command implementation, never request data.
		out.Err = fmt.Errorf("refusing to execute non-absolute path %q", s.Path)
		return out
	}
	argv := append([]string{s.Path}, s.Args...)
	if e.DryRun {
		loggerFrom(ctx).InfoContext(ctx, "dry run: not executing", "argv", argv)
		zero := 0
		out.ExitCode, out.Success = &zero, true
		out.Stdout = fmt.Sprintf("dry run: would execute %q\n", argv)
		return out
	}

	stdout := output.NewTailBuffer(e.MaxOutputBytes)
	stderr := output.NewTailBuffer(e.MaxOutputBytes)
	grace := e.KillGrace
	if grace <= 0 {
		grace = DefaultKillGrace
	}

	// argv[0] is absolute, so exec does no PATH lookup; there is no shell.
	cmd := exec.CommandContext(ctx, s.Path, s.Args...)
	cmd.Env = slices.Clone(Env)
	cmd.Stdin = nil // os/exec connects a nil Stdin to the null device
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Own process group, so the escalation reaches any children too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var killTimer *time.Timer
	cmd.Cancel = func() error {
		pgid := cmd.Process.Pid
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		killTimer = time.AfterFunc(grace, func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
		return nil
	}
	// Backstop: Wait returns even if a grandchild holds the output pipes open.
	cmd.WaitDelay = grace + 2*time.Second

	err := cmd.Start()
	if err != nil {
		out.Err = fmt.Errorf("start: %w", err)
		out.Duration = time.Since(start)
		return out
	}
	err = cmd.Wait()
	if killTimer != nil {
		killTimer.Stop()
	}
	out.Duration = time.Since(start)
	out.Stdout, out.Stderr = stdout.String(), stderr.String()
	out.Truncated = stdout.Truncated() || stderr.Truncated()

	switch err := ctx.Err(); {
	case errors.Is(err, context.DeadlineExceeded):
		out.TimedOut = true
	case err != nil:
		out.Err = fmt.Errorf("stopped before completion: %w", context.Cause(ctx))
	}
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		code := cmd.ProcessState.ExitCode()
		out.ExitCode = &code
		codes := s.SuccessCodes
		if len(codes) == 0 {
			codes = []int{0}
		}
		out.Success = !out.TimedOut && out.Err == nil && slices.Contains(codes, code)
	} else if err != nil && out.Err == nil && !out.TimedOut {
		out.Err = err
	}
	return out
}
