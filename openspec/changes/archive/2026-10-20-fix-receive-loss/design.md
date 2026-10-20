# Design

## Context

`agent.Run` takes a slot with `reserve`, then calls `queue.Receive(ctx, n, wait, visibility)`.
On shutdown the context is cancelled, which cancels the in-flight `ReceiveMessage`. The AWS
SDK returns an error with no messages, and `awsx.SQSQueue.Receive` drops anything SQS had
already handed over (see proposal.md "Why").

`handle` already returns unstarted messages during shutdown (it checks `a.stopping`), but
`stopping` is set only after the loop exits. A message returned by a receive that completes
during shutdown therefore reaches `handle` with `stopping` still false, and would be executed.

## Goals / Non-Goals

**Goals:**
- No message the server has handed to a poll is left invisible because shutdown cancelled the poll.
- A message received after shutdown has begun is returned immediately, not executed.
- The redelivery pattern is visible in logs.

**Non-Goals:**
- Recovering messages whose response was lost in transit (SQS-level, see proposal).
- Changing the drain grace, kill, or result-recording behaviour of §9.3.

## Decisions

**D1. Do not cancel the receive on shutdown; bound it by time instead.** The poll gets a
context with a deadline of `WaitTimeSeconds + 10 s`, unrelated to the Run context. Shutdown
waits for it, so the loop cannot exit while the server still holds messages for this process.
*Alternative rejected:* cancel the receive and reconcile lost messages by polling again. That
cannot recover messages SQS has already hidden without an identifier we do not have.

**D2. Handle messages from a completed receive according to the Run context, at the point the
receive returns.** If the Run context is done, call `ChangeVisibility(0)` on each message and
release the slots. Otherwise proceed as today. This does not depend on `stopping`, which is set
later.

**D3. Log each received message at info.** Volume is one line per message, which is small at
this scale. Debug would hide the case that needs diagnosing on the production log level.

**D4. `TimeoutStopSec=120` in the unit.** Worst-case stop is now one poll (≤30 s) plus the
grace (45 s) plus the post-shutdown wait. systemd's default of 90 s is too short for that.

## Risks / Trade-offs

- **Shutdown takes longer** → up to ~30 s more before the grace starts. Mitigated by
  `TimeoutStopSec=120`. The poll is bounded, so shutdown cannot hang.
- **A poll that returns after its deadline** → if the deadline fires first, the SDK returns an
  error and drops messages. The margin is 10 s above the wait time, so this only happens if the
  network is already failing. The residual loss is logged by the existing receive-error warning.
- **Test realism** → the fake queue never drops messages. The new test wraps it so the receive
  returns messages after cancellation, which is the case that matters. The real-SQS behaviour is
  confirmed by the remote e2e run, not by the unit test.

## Migration Plan

1. Land the code and tests; `go test ./...` must pass.
2. Rebuild for arm64, reinstall with `make deploy ACTION=install`, restart.
3. Rerun `make e2e`. A pass means dry run never executes, and every request has `attempt: 1`.
   If the `is-active` loss recurs, read its `message received` line before changing anything else.
