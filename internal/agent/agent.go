// Package agent is the sqshd receive loop, worker pool, message handling
// path, and shutdown sequence (SPEC.md §8.2, §9.2–9.3).
package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"sqsh/internal/awsx"
	"sqsh/internal/config"
	"sqsh/internal/dispatcher"
	"sqsh/internal/observability"
	"sqsh/internal/protocol"
	"sqsh/internal/results"
)

// awsCallTimeout bounds each store or queue call made while handling a
// message. These calls use a context detached from the receive loop so that
// shutdown does not abandon a result write halfway.
const awsCallTimeout = 15 * time.Second

// visibilityHeadroom is added to max_command_timeout to cap how long a
// running command may keep its message invisible (§8.5).
const visibilityHeadroom = 60 * time.Second

// Agent consumes one queue and executes the commands addressed to it.
type Agent struct {
	cfg      *config.Config
	queue    awsx.Queue
	store    results.ResultStore
	registry *dispatcher.Registry
	log      *slog.Logger
	metrics  *observability.Metrics
	version  string

	// Now is the clock used for request time-window checks.
	Now func() time.Time
	// Backoff is the receive-error retry policy.
	Backoff *awsx.Backoff
	// PostShutdownWait bounds how long shutdown waits for killed commands to
	// record their results after shutdown_grace has elapsed.
	PostShutdownWait time.Duration

	slots    chan struct{}
	handlers sync.WaitGroup
	stopping atomic.Bool

	execCtx    context.Context
	cancelExec context.CancelCauseFunc
}

// Deps are the collaborators an Agent is built from.
type Deps struct {
	Config   *config.Config
	Queue    awsx.Queue
	Store    results.ResultStore
	Registry *dispatcher.Registry
	Logger   *slog.Logger
	Metrics  *observability.Metrics
	Version  string
}

// New returns an agent. It does not start receiving; call Run.
func New(d Deps) *Agent {
	execCtx, cancel := context.WithCancelCause(context.Background())
	return &Agent{
		cfg: d.Config, queue: d.Queue, store: d.Store, registry: d.Registry,
		log: d.Logger.With("host", d.Config.HostID), metrics: d.Metrics, version: d.Version,
		Now:              time.Now,
		Backoff:          awsx.NewReceiveBackoff(),
		PostShutdownWait: dispatcher.DefaultKillGrace + 2*awsCallTimeout,
		slots:            make(chan struct{}, d.Config.MaxConcurrentCommands),
		execCtx:          execCtx,
		cancelExec:       cancel,
	}
}

// Run receives and handles messages until ctx is cancelled, then performs
// the §9.3 graceful shutdown. It returns only after shutdown completes.
func (a *Agent) Run(ctx context.Context) error {
	a.log.Info("receive loop starting", "queue_url", a.cfg.QueueURL,
		"max_concurrent_commands", a.cfg.MaxConcurrentCommands, "dry_run", !a.cfg.ShouldExecute())
	for ctx.Err() == nil {
		n := a.reserve(ctx)
		if n == 0 {
			break // ctx cancelled while waiting for a slot
		}
		msgs, err := a.queue.Receive(ctx, n,
			time.Duration(a.cfg.WaitTimeSeconds)*time.Second, a.cfg.VisibilityTimeout.D())
		if err != nil {
			a.release(n)
			if ctx.Err() != nil {
				break
			}
			a.metrics.AWSErrors.WithLabelValues("ReceiveMessage").Inc()
			delay := a.Backoff.Next()
			a.log.Warn("receive failed; backing off", "err", err, "retry_in", delay.String())
			select {
			case <-ctx.Done():
			case <-time.After(delay):
			}
			continue
		}
		a.Backoff.Reset()
		a.metrics.LastSuccessfulPoll.SetToCurrentTime()

		// Each received message takes one reserved slot; unused reservations
		// are returned. SQS never returns more than requested, but if it did
		// the extras are refused rather than run over the limit.
		for i, m := range msgs {
			a.handlers.Add(1)
			if i < n {
				go func() {
					defer a.handlers.Done()
					defer a.release(1)
					a.handle(m)
				}()
			} else {
				go func() {
					defer a.handlers.Done()
					a.rejectOverCapacity(m)
				}()
			}
		}
		if len(msgs) < n {
			a.release(n - len(msgs))
		}
	}
	a.shutdown()
	return nil
}

// reserve blocks until at least one execution slot is free, then takes as
// many free slots as are available, up to the SQS batch maximum of 10.
func (a *Agent) reserve(ctx context.Context) int {
	select {
	case a.slots <- struct{}{}:
	case <-ctx.Done():
		return 0
	}
	n := 1
	for n < 10 {
		select {
		case a.slots <- struct{}{}:
			n++
		default:
			return n
		}
	}
	return n
}

func (a *Agent) release(n int) {
	for range n {
		<-a.slots
	}
}

// shutdown implements §9.3 for this slice: stop receiving (already done by
// the caller), wait up to shutdown_grace for in-flight commands, then kill
// what remains and wait for its results to be recorded.
func (a *Agent) shutdown() {
	a.stopping.Store(true)
	a.log.Info("shutting down; draining in-flight commands", "grace", a.cfg.ShutdownGrace.String())
	done := make(chan struct{})
	go func() { a.handlers.Wait(); close(done) }()

	select {
	case <-done:
		a.log.Info("drained cleanly")
		return
	case <-time.After(a.cfg.ShutdownGrace.D()):
	}
	a.log.Warn("shutdown grace elapsed; terminating running commands")
	a.cancelExec(errors.New("agent shutting down"))
	select {
	case <-done:
		a.log.Info("terminated commands recorded")
	case <-time.After(a.PostShutdownWait):
		a.log.Error("gave up waiting for terminated commands to record results")
	}
}

// awsCtx returns a context for one store or queue call, detached from
// receive-loop cancellation.
func awsCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), awsCallTimeout)
}

// commandLabel bounds metric label cardinality: request-supplied command
// names that are not registered collapse to "unknown".
func (a *Agent) commandLabel(name string) string {
	if _, rej := a.registry.Resolve(name); rej == nil || rej.Reason != protocol.ReasonUnknownCommand {
		return name
	}
	return "unknown"
}
