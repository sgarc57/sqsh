// Package dispatcher turns a validated request into a process: the command
// registry, argument validation, and shell-free execution (SPEC.md §10).
package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"time"

	"sqsh/internal/config"
	"sqsh/internal/protocol"
)

// Semantics declares whether a command may be retried by redelivery (§8.1).
type Semantics int

const (
	// AtLeastOnce commands are safe to retry: idempotent, read-only, or converging.
	AtLeastOnce Semantics = iota
	// AtMostOnce commands must never be retried automatically. No command in
	// this release is both AtMostOnce and enabled; the value exists so the
	// handling path can branch on it when terminal commands ship.
	AtMostOnce
)

func (s Semantics) String() string {
	if s == AtMostOnce {
		return "at_most_once"
	}
	return "at_least_once"
}

// Command is a built-in command (§10).
type Command interface {
	Name() string
	Semantics() Semantics
	// Validate rejects disallowed arguments before anything is executed. It
	// must be pure: no filesystem or network access beyond the allowlist config.
	Validate(cfg *config.Config, args []string) error
	// Execute runs the command. Implementations go through an Executor, which
	// uses an absolute argv[0] and never invokes a shell.
	Execute(ctx context.Context, args []string) Outcome
}

// Outcome is the observable result of executing a command.
type Outcome struct {
	ExitCode  *int
	Stdout    string
	Stderr    string
	Truncated bool
	TimedOut  bool
	// Err is set when the process could not be started or was stopped for a
	// reason other than its own timeout, such as agent shutdown.
	Err       error
	StartedAt time.Time
	Duration  time.Duration
	// Success is true when the process exited with one of the command's
	// declared success codes.
	Success bool
}

// Status maps the outcome onto a terminal result status.
func (o Outcome) Status() protocol.Status {
	switch {
	case o.TimedOut:
		return protocol.StatusTimedOut
	case o.Err != nil:
		return protocol.StatusFailed
	case o.Success:
		return protocol.StatusSucceeded
	default:
		return protocol.StatusFailed
	}
}

// Registry holds the commands this agent implements and which are permitted.
type Registry struct {
	cmds    map[string]Command
	enabled map[string]bool
	allowed map[string]bool
}

// NewRegistry builds an empty registry permitting the given command names.
// Call Register for each implementation, then Check.
func NewRegistry(allowed []string) *Registry {
	r := &Registry{cmds: map[string]Command{}, enabled: map[string]bool{}, allowed: map[string]bool{}}
	for _, name := range allowed {
		r.allowed[name] = true
	}
	return r
}

// Register adds a command. A disabled command is known to the registry — so
// it is refused as command_not_allowed rather than unknown_command — but is
// never resolved for execution.
func (r *Registry) Register(c Command, enabled bool) {
	r.cmds[c.Name()] = c
	r.enabled[c.Name()] = enabled
}

// Check reports allowlist entries that name no registered command; the
// daemon treats this as a configuration error so a typo cannot silently
// permit nothing.
func (r *Registry) Check() error {
	var errs []error
	for name := range r.allowed {
		if _, ok := r.cmds[name]; !ok {
			errs = append(errs, fmt.Errorf("allowed_commands: %q is not an implemented command", name))
		}
	}
	return errors.Join(errs...)
}

// Disabled returns allowlisted commands that are registered but disabled.
func (r *Registry) Disabled() []string {
	var out []string
	for name := range r.allowed {
		if c, ok := r.cmds[name]; ok && !r.enabled[c.Name()] {
			out = append(out, name)
		}
	}
	return out
}

// Resolve returns the command named name, or the rejection that applies.
func (r *Registry) Resolve(name string) (Command, *protocol.Rejection) {
	c, ok := r.cmds[name]
	if !ok {
		return nil, &protocol.Rejection{Reason: protocol.ReasonUnknownCommand, Detail: fmt.Sprintf("%q is not implemented", name)}
	}
	if !r.allowed[name] {
		return nil, &protocol.Rejection{Reason: protocol.ReasonCommandNotAllowed, Detail: fmt.Sprintf("%q is not in allowed_commands", name)}
	}
	if !r.enabled[name] {
		return nil, &protocol.Rejection{Reason: protocol.ReasonCommandNotAllowed, Detail: fmt.Sprintf("%q is disabled in this release", name)}
	}
	return c, nil
}

// Prepare resolves a command and validates its complete argument list. No
// process is spawned unless Prepare succeeds.
func (r *Registry) Prepare(cfg *config.Config, name string, args []string) (Command, *protocol.Rejection) {
	c, rej := r.Resolve(name)
	if rej != nil {
		return nil, rej
	}
	if err := c.Validate(cfg, args); err != nil {
		return nil, &protocol.Rejection{Reason: protocol.ReasonArgNotAllowed, Detail: err.Error()}
	}
	return c, nil
}
