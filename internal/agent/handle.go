package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"sqsh/internal/awsx"
	"sqsh/internal/dispatcher"
	"sqsh/internal/observability"
	"sqsh/internal/protocol"
	"sqsh/internal/results"
)

// handle runs one message through the §8.2 path for AtLeastOnce commands:
// parse → validate → target check → claim → execute → complete → delete.
// Every rejection is recorded and its message deleted.
func (a *Agent) handle(m awsx.Message) {
	a.metrics.MessagesReceived.Inc()
	a.metrics.InFlight.Inc()
	defer a.metrics.InFlight.Dec()

	req, rej := protocol.Parse([]byte(m.Body))
	log := a.log.With("request_id", req.RequestID, "message_id", m.ID, "receive_count", m.ReceiveCount)

	// The filter policy is an optimization, not the authorization boundary:
	// the target is re-checked here whatever the broker delivered (§6.1).
	if rej == nil && req.Target.Host != a.cfg.HostID {
		rej = &protocol.Rejection{Reason: protocol.ReasonInvalidTarget,
			Detail: "addressed to " + req.Target.Host + " but received by " + a.cfg.HostID}
	}
	if rej == nil {
		rej = protocol.Validate(req, a.Now(), protocol.Limits{
			MaxCommandTimeout: a.cfg.MaxCommandTimeout.D(), MaxClockSkew: a.cfg.MaxClockSkew.D(),
		})
	}
	var cmd dispatcher.Command
	if rej == nil {
		cmd, rej = a.registry.Prepare(a.cfg, req.Command, req.Args)
	}
	if rej != nil {
		a.recordRejection(log, m, req, rej)
		return
	}
	log = log.With("issuer", req.Issuer, "command", req.Command, "args", req.Args)

	if a.stopping.Load() && cmd.Semantics() == dispatcher.AtLeastOnce {
		// Not started: hand it straight back so a restarted agent picks it up
		// without waiting out the visibility timeout (§9.3 step 4).
		ctx, cancel := awsCtx()
		defer cancel()
		if err := a.queue.ChangeVisibility(ctx, m.ReceiptHandle, 0); err != nil {
			a.metrics.AWSErrors.WithLabelValues("ChangeMessageVisibility").Inc()
			log.Warn("could not return unstarted message to the queue", "err", err)
		} else {
			log.Info("shutting down; returned unstarted message to the queue")
		}
		return
	}

	start := a.Now()
	base := protocol.Result{
		RequestID: req.RequestID, Host: a.cfg.HostID, Command: req.Command, Issuer: req.Issuer,
		StartedAt: protocol.FormatTimeMillis(start), Attempt: max(m.ReceiveCount, 1), AgentVersion: a.version,
	}
	claim := base
	claim.Status = protocol.StatusRunning
	if err := a.claim(claim); err != nil {
		if errors.Is(err, results.ErrAlreadyClaimed) {
			// The existing record belongs to whichever delivery won; writing a
			// `duplicate` result over it would destroy the real outcome. The
			// rejection is logged and counted, and the message is deleted.
			a.metrics.Rejections.WithLabelValues(string(protocol.ReasonDuplicate)).Inc()
			log.Warn("request rejected", "reason", protocol.ReasonDuplicate,
				"detail", "request_id already claimed; another delivery owns the result")
			a.deleteMessage(log, m)
			return
		}
		// Without a claim there is no duplicate guard, so nothing runs. The
		// message is left for redelivery (and, eventually, the DLQ).
		log.Error("could not claim request; not executing, leaving message for redelivery", "err", err)
		return
	}

	outcome := a.execute(log, m, req, cmd)

	res := base
	res.Status = outcome.Status()
	res.ExitCode = outcome.ExitCode
	res.Stdout, res.Stderr, res.OutputTruncated = outcome.Stdout, outcome.Stderr, outcome.Truncated
	res.DurationMS = outcome.Duration.Milliseconds()
	res.CompletedAt = protocol.FormatTimeMillis(a.Now())
	switch {
	case outcome.Err != nil:
		res.Error = ptr(outcome.Err.Error())
	case outcome.TimedOut:
		res.Error = ptr("command exceeded timeout_seconds and was killed")
	}
	a.complete(log, res)
	a.deleteMessage(log, m)

	a.metrics.Commands.WithLabelValues(req.Command, string(res.Status)).Inc()
	a.metrics.CommandDuration.WithLabelValues(req.Command).Observe(outcome.Duration.Seconds())
	attrs := []any{"status", res.Status, "duration_ms", res.DurationMS, "attempt", res.Attempt,
		"output_truncated", res.OutputTruncated}
	if res.ExitCode != nil {
		attrs = append(attrs, "exit_code", *res.ExitCode)
	}
	if res.Error != nil {
		attrs = append(attrs, "error", *res.Error)
	}
	log.Info("command completed", attrs...)
	// Output may be large and may contain secrets: debug only, truncated.
	log.Debug("command output",
		"stdout", truncate(res.Stdout, observability.DebugOutputLimit),
		"stderr", truncate(res.Stderr, observability.DebugOutputLimit))
}

// execute runs cmd under the request timeout while keeping the message
// invisible for as long as the command may legitimately run.
func (a *Agent) execute(log *slog.Logger, m awsx.Message, req protocol.Request, cmd dispatcher.Command) dispatcher.Outcome {
	ctx, cancel := context.WithTimeout(dispatcher.WithLogger(a.execCtx, log), time.Duration(req.TimeoutSeconds)*time.Second)
	defer cancel()

	if cmd.Semantics() == dispatcher.AtLeastOnce {
		vctx, stop := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			awsx.KeepVisible(vctx, a.queue, m.ReceiptHandle, a.cfg.VisibilityTimeout.D(),
				a.cfg.MaxCommandTimeout.D()+visibilityHeadroom, func(err error) {
					if err != nil {
						a.metrics.AWSErrors.WithLabelValues("ChangeMessageVisibility").Inc()
						log.Warn("visibility extension failed", "err", err)
						return
					}
					a.metrics.VisibilityExtensions.WithLabelValues(req.Command).Inc()
				})
		}()
		defer func() { stop(); <-done }()
	}
	log.Info("executing command", "timeout_seconds", req.TimeoutSeconds)
	return cmd.Execute(ctx, req.Args)
}

// recordRejection writes a rejection result and deletes the message. The
// write is conditional on no record existing, so a rejection never
// overwrites a result another delivery owns.
func (a *Agent) recordRejection(log *slog.Logger, m awsx.Message, req protocol.Request, rej *protocol.Rejection) {
	key := req.RequestID
	if !protocol.IsUUIDv7(key) {
		// Without a usable request_id there is nothing an operator could look
		// the result up by; record it under the SQS message ID instead so the
		// rejection is still not a silent drop.
		key = "sqs-message:" + m.ID
	}
	host := req.Target.Host
	if host == "" {
		host = a.cfg.HostID
	}
	now := a.Now()
	res := protocol.Result{
		RequestID: key, Host: host, Command: req.Command, Issuer: req.Issuer,
		Status: rej.Status(), RejectionReason: ptr(rej.Reason), Error: ptr(rej.Detail),
		StartedAt: protocol.FormatTimeMillis(now), CompletedAt: protocol.FormatTimeMillis(now),
		Attempt: max(m.ReceiveCount, 1), AgentVersion: a.version,
	}
	a.metrics.Rejections.WithLabelValues(string(rej.Reason)).Inc()
	a.metrics.Commands.WithLabelValues(a.commandLabel(req.Command), string(res.Status)).Inc()
	log.Warn("request rejected", "reason", rej.Reason, "detail", rej.Detail, "issuer", req.Issuer,
		"command", req.Command, "result_key", key)

	switch err := a.claim(res); {
	case errors.Is(err, results.ErrAlreadyClaimed):
		log.Info("a result already exists for this request; rejection not recorded over it")
	case err != nil:
		log.Error("rejection NOT recorded: result store write failed", "err", err)
	}
	// A rejected message is never retried: it will be rejected on every delivery.
	a.deleteMessage(log, m)
}

// rejectOverCapacity refuses a message that arrived with no execution slot.
func (a *Agent) rejectOverCapacity(m awsx.Message) {
	a.metrics.MessagesReceived.Inc()
	req, _ := protocol.Parse([]byte(m.Body))
	log := a.log.With("request_id", req.RequestID, "message_id", m.ID, "receive_count", m.ReceiveCount)
	a.recordRejection(log, m, req, &protocol.Rejection{Reason: protocol.ReasonConcurrencyLimit,
		Detail: "no execution slot available"})
}

func (a *Agent) claim(r protocol.Result) error {
	ctx, cancel := awsCtx()
	defer cancel()
	err := a.store.Claim(ctx, r)
	if err != nil && !errors.Is(err, results.ErrAlreadyClaimed) {
		a.metrics.AWSErrors.WithLabelValues("PutItem").Inc()
		a.metrics.ResultWriteFailures.WithLabelValues("claim").Inc()
	}
	return err
}

func (a *Agent) complete(log *slog.Logger, r protocol.Result) {
	ctx, cancel := awsCtx()
	defer cancel()
	switch err := a.store.Complete(ctx, r); {
	case errors.Is(err, results.ErrStaleWrite):
		a.metrics.DowngradesPrevented.Inc()
		log.Warn("result write dropped: stored status is already final", "status", r.Status)
	case err != nil:
		a.metrics.AWSErrors.WithLabelValues("PutItem").Inc()
		a.metrics.ResultWriteFailures.WithLabelValues("complete").Inc()
		// No spool in this release: the outcome is in this log line only.
		log.Error("result NOT recorded: result store write failed", "err", err, "status", r.Status)
	}
}

func (a *Agent) deleteMessage(log *slog.Logger, m awsx.Message) {
	ctx, cancel := awsCtx()
	defer cancel()
	if err := a.queue.Delete(ctx, m.ReceiptHandle); err != nil {
		a.metrics.AWSErrors.WithLabelValues("DeleteMessage").Inc()
		log.Error("could not delete message; it will be redelivered", "err", err)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func ptr[T any](v T) *T { return &v }
