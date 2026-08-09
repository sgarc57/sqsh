package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"sqsh/internal/awsx"
	"sqsh/internal/protocol"
	"sqsh/internal/results"
	"sqsh/internal/version"
)

// Exit codes are a stable contract (SPEC.md §16). Exit 5, "host unknown or
// stale", needs the deferred inventory and is not used in this release.
const (
	exitSucceeded   = 0
	exitFailed      = 1
	exitUsage       = 2
	exitRejected    = 3
	exitGaveUp      = 4
	exitLocalOrAWS  = 6
	pollInitial     = 250 * time.Millisecond
	pollMax         = 2 * time.Second
	defaultTimeout  = 30 * time.Second
	defaultExpiry   = 5 * time.Minute
	defaultWaitTime = 2 * time.Minute
)

// resultGetter is the read half of the result store.
type resultGetter interface {
	Get(ctx context.Context, requestID string) (protocol.Result, error)
}

// backend is everything the CLI needs from AWS. It is built only after
// argument parsing succeeds, so usage errors never touch the network.
type backend struct {
	publisher awsx.Publisher
	results   resultGetter
	issuer    func(ctx context.Context) (string, error)
}

// options are the global flags plus connection settings.
type options struct {
	wait         bool
	waitTimeout  time.Duration
	timeout      time.Duration
	expiresIn    time.Duration
	json         bool
	topicARN     string
	resultsTable string
	region       string
}

type cli struct {
	stdout, stderr io.Writer
	connect        func(ctx context.Context, o options, needPublish bool) (*backend, error)
	now            func() time.Time
	sleep          func(ctx context.Context, d time.Duration) error
	jsonMode       bool
}

const usage = `usage:
  sqsh [flags] systemctl <host> <verb> <unit>   run a read-only systemctl query on <host>
  sqsh [flags] status <request_id>              fetch a recorded result
  sqsh version

flags (accepted before or after the subcommand):
  --wait / --no-wait      wait for the result (default) or print the request_id and exit
  --wait-timeout D        stop waiting after D (default 2m); the command may still run
  --timeout D             timeout_seconds for the remote command (default 30s)
  --expires-in D          the request expires if not started within D (default 5m)
  --json                  print the raw result record as JSON
  --topic-arn ARN         command topic (env SQSH_TOPIC_ARN)
  --results-table NAME    results table (env SQSH_RESULTS_TABLE, default sqsh-results)
  --region REGION         AWS region (env AWS_REGION)

exit codes:
  0 succeeded   1 failed or timed out   2 usage error   3 rejected or expired
  4 gave up waiting / no result yet     6 local or AWS error
`

func (c *cli) run(ctx context.Context, args []string) int {
	o, pos, err := c.parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(c.stdout, usage)
		return exitSucceeded
	}
	if err != nil {
		fmt.Fprintf(c.stderr, "sqsh: %v\n\n%s", err, usage)
		return exitUsage
	}
	c.jsonMode = o.json
	if len(pos) == 0 {
		fmt.Fprint(c.stderr, usage)
		return exitUsage
	}
	switch sub, rest := pos[0], pos[1:]; sub {
	case "version":
		fmt.Fprintln(c.stdout, "sqsh", version.String())
		return exitSucceeded
	case "help":
		fmt.Fprint(c.stdout, usage)
		return exitSucceeded
	case "systemctl":
		if len(rest) != 3 || rest[0] == "" {
			return c.usageError("systemctl takes exactly <host> <verb> <unit>")
		}
		return c.invoke(ctx, o, rest[0], "systemctl", rest[1:])
	case "status":
		if len(rest) != 1 {
			return c.usageError("status takes exactly one <request_id>")
		}
		if !protocol.IsUUIDv7(rest[0]) {
			return c.usageError(fmt.Sprintf("%q is not a request_id (UUIDv7)", rest[0]))
		}
		return c.status(ctx, o, rest[0])
	default:
		return c.usageError(fmt.Sprintf("unknown command %q", sub))
	}
}

func (c *cli) usageError(msg string) int {
	fmt.Fprintf(c.stderr, "sqsh: %s\n\n%s", msg, usage)
	return exitUsage
}

// parse accepts flags anywhere among the positional arguments. Everything
// after a literal `--` is positional.
func (c *cli) parse(args []string) (options, []string, error) {
	o := options{
		wait: true, waitTimeout: defaultWaitTime, timeout: defaultTimeout, expiresIn: defaultExpiry,
		topicARN: os.Getenv("SQSH_TOPIC_ARN"), resultsTable: os.Getenv("SQSH_RESULTS_TABLE"),
		region: os.Getenv("AWS_REGION"),
	}
	if o.resultsTable == "" {
		o.resultsTable = "sqsh-results"
	}
	fs := flag.NewFlagSet("sqsh", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&o.wait, "wait", o.wait, "")
	noWait := fs.Bool("no-wait", false, "")
	fs.DurationVar(&o.waitTimeout, "wait-timeout", o.waitTimeout, "")
	fs.Func("timeout", "", func(s string) error { return parseSeconds(s, &o.timeout) })
	fs.DurationVar(&o.expiresIn, "expires-in", o.expiresIn, "")
	fs.BoolVar(&o.json, "json", false, "")
	fs.StringVar(&o.topicARN, "topic-arn", o.topicARN, "")
	fs.StringVar(&o.resultsTable, "results-table", o.resultsTable, "")
	fs.StringVar(&o.region, "region", o.region, "")
	showVersion := false
	fs.Var(boolFunc(func(b bool) { showVersion = b }), "version", "")

	var tail []string
	for i, a := range args {
		if a == "--" {
			args, tail = args[:i], args[i+1:]
			break
		}
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return o, nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	pos = append(pos, tail...)
	if showVersion {
		pos = []string{"version"}
	}
	if *noWait {
		o.wait = false
	}
	switch {
	case o.waitTimeout <= 0:
		return o, nil, errors.New("--wait-timeout must be positive")
	case o.timeout < time.Second || o.timeout%time.Second != 0:
		return o, nil, errors.New("--timeout must be a whole number of seconds, at least 1s")
	case o.expiresIn <= 0:
		return o, nil, errors.New("--expires-in must be positive")
	}
	return o, pos, nil
}

// parseSeconds accepts a Go duration ("45s") or a bare number of seconds.
func parseSeconds(s string, d *time.Duration) error {
	if n, err := strconv.Atoi(s); err == nil {
		*d = time.Duration(n) * time.Second
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = v
	return nil
}

type boolFunc func(bool)

func (f boolFunc) String() string   { return "false" }
func (f boolFunc) IsBoolFlag() bool { return true }
func (f boolFunc) Set(s string) error {
	b, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	f(b)
	return nil
}

// invoke publishes a request and, unless --no-wait, waits for its result.
func (c *cli) invoke(ctx context.Context, o options, host, command string, args []string) int {
	if o.topicARN == "" {
		return c.usageError("no topic: set --topic-arn or SQSH_TOPIC_ARN")
	}
	be, err := c.connect(ctx, o, true)
	if err != nil {
		fmt.Fprintf(c.stderr, "sqsh: %v\n", err)
		return exitLocalOrAWS
	}
	issuer, err := be.issuer(ctx)
	if err != nil {
		fmt.Fprintf(c.stderr, "sqsh: cannot determine caller identity; nothing was sent: %v\n", err)
		return exitLocalOrAWS
	}
	id, err := protocol.NewRequestID()
	if err != nil {
		fmt.Fprintf(c.stderr, "sqsh: cannot generate request_id: %v\n", err)
		return exitLocalOrAWS
	}
	now := c.now().UTC()
	req := protocol.Request{
		Version: protocol.Version, RequestID: id, Target: protocol.Target{Host: host},
		Command: command, Args: args, IssuedAt: now, ExpiresAt: now.Add(o.expiresIn),
		TimeoutSeconds: int(o.timeout / time.Second), Issuer: issuer,
	}
	body, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(c.stderr, "sqsh: cannot encode request: %v\n", err)
		return exitLocalOrAWS
	}
	rt := awsx.Routing{Host: host, Command: command, ProtocolVersion: protocol.Version}
	if _, err := be.publisher.Publish(ctx, body, rt); err != nil {
		fmt.Fprintf(c.stderr, "sqsh: publish failed; the command was NOT sent: %v\n", err)
		return exitLocalOrAWS
	}

	if !o.wait {
		if o.json {
			c.printJSON(map[string]string{"request_id": id, "host": host, "status": "published"})
		} else {
			fmt.Fprintln(c.stdout, id)
		}
		return exitSucceeded
	}
	fmt.Fprintf(c.stderr, "request_id: %s (published to %s; waiting up to %s)\n", id, host, o.waitTimeout)
	return c.wait(ctx, o, be, id)
}

// wait polls for a terminal result with backoff from 250ms to 2s.
func (c *cli) wait(ctx context.Context, o options, be *backend, id string) int {
	deadline := c.now().Add(o.waitTimeout)
	delay := pollInitial
	var lastErr error
	for {
		r, err := be.results.Get(ctx, id)
		switch {
		case err == nil && r.Status.Terminal():
			return c.render(r)
		case err == nil, errors.Is(err, results.ErrNotFound):
			lastErr = nil
		default:
			lastErr = err
		}
		remaining := deadline.Sub(c.now())
		if remaining <= 0 {
			break
		}
		if c.sleep(ctx, min(delay, remaining)) != nil {
			break // interrupted: the operator stopped waiting, not a backend failure
		}
		delay = min(delay*2, pollMax)
	}
	return c.gaveUp(o, id, lastErr)
}

// gaveUp reports that the CLI stopped waiting. It must never imply the
// command was stopped: nothing in this system can recall an in-flight command.
func (c *cli) gaveUp(o options, id string, lastErr error) int {
	if lastErr != nil {
		fmt.Fprintf(c.stderr, "sqsh: could not read the result store: %v\n", lastErr)
	}
	msg := fmt.Sprintf("stopped waiting after %s. The command was NOT cancelled and may still be running "+
		"(nothing can recall an in-flight command).", o.waitTimeout)
	if o.json {
		c.printJSON(map[string]string{"request_id": id, "status": "unknown", "message": msg,
			"resume": "sqsh status " + id})
	}
	fmt.Fprintf(c.stderr, "sqsh: %s\n  request_id: %s\n  check later: sqsh status %s\n", msg, id, id)
	if lastErr != nil {
		return exitLocalOrAWS
	}
	return exitGaveUp
}

// status prints a recorded result without waiting.
func (c *cli) status(ctx context.Context, o options, id string) int {
	be, err := c.connect(ctx, o, false)
	if err != nil {
		fmt.Fprintf(c.stderr, "sqsh: %v\n", err)
		return exitLocalOrAWS
	}
	r, err := be.results.Get(ctx, id)
	switch {
	case errors.Is(err, results.ErrNotFound):
		fmt.Fprintf(c.stderr, "sqsh: no result recorded for %s. It may not have been received yet, "+
			"or its record may have expired.\n", id)
		return exitGaveUp
	case err != nil:
		fmt.Fprintf(c.stderr, "sqsh: could not read the result store: %v\n", err)
		return exitLocalOrAWS
	}
	code := c.render(r)
	if !r.Status.Terminal() {
		fmt.Fprintf(c.stderr, "sqsh: %s is still %s; its outcome is not known yet.\n", id, r.Status)
		return exitGaveUp
	}
	return code
}

// exitCode maps a terminal status onto the exit-code contract.
func exitCode(s protocol.Status) int {
	switch s {
	case protocol.StatusSucceeded:
		return exitSucceeded
	case protocol.StatusFailed, protocol.StatusTimedOut:
		return exitFailed
	case protocol.StatusRejected, protocol.StatusExpired:
		return exitRejected
	}
	return exitGaveUp // running or unrecognised: outcome not known
}

func (c *cli) printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Fprintln(c.stdout, string(b))
}

// render prints r and returns the exit code its status maps to.
func (c *cli) render(r protocol.Result) int {
	if c.jsonMode {
		c.printJSON(r)
		if r.OutputTruncated {
			fmt.Fprintln(c.stderr, "sqsh: output_truncated is true: stdout/stderr hold only the tail of the output")
		}
		return exitCode(r.Status)
	}
	w := c.stdout
	fmt.Fprintf(w, "request_id: %s\nhost:       %s\n", r.RequestID, r.Host)
	status := string(r.Status)
	if r.ExitCode != nil {
		status += fmt.Sprintf(" (exit %d)", *r.ExitCode)
	}
	fmt.Fprintf(w, "status:     %s\n", status)
	if r.RejectionReason != nil {
		fmt.Fprintf(w, "reason:     %s\n", *r.RejectionReason)
	}
	if r.Error != nil {
		fmt.Fprintf(w, "error:      %s\n", *r.Error)
	}
	if r.Status != protocol.StatusRejected && r.Status != protocol.StatusExpired {
		fmt.Fprintf(w, "duration:   %s\n", time.Duration(r.DurationMS)*time.Millisecond)
	}
	if r.Stdout != "" {
		fmt.Fprintf(w, "--- stdout ---\n%s", ensureNewline(r.Stdout))
	}
	if r.Stderr != "" {
		fmt.Fprintf(w, "--- stderr ---\n%s", ensureNewline(r.Stderr))
	}
	if r.OutputTruncated {
		fmt.Fprintln(w, "--- OUTPUT TRUNCATED: only the last part of each stream was kept; the output above is incomplete ---")
	}
	return exitCode(r.Status)
}

func ensureNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
