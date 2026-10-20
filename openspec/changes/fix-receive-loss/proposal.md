# Proposal

## Why

The remote-node run of `validate-remote-node` (run 2) found requests that the server had
handed to a receive but no daemon ever handled. They stayed invisible for the 90 s
visibility timeout, were then executed late, and in one case broke the dry-run guarantee.

The confirmed mechanism is in `internal/awsx.SQSQueue.Receive`. When `ReceiveMessage` fails,
including because its context was cancelled, the function returns `(nil, err)` and drops
any messages SQS has already returned. The agent cancels the receive on shutdown, so a
restart with a long poll in flight loses whatever that poll received.

SPEC §9.3 already requires that unstarted `AtLeastOnce` messages go back to the queue
immediately on shutdown. The current code does not meet that for messages whose receive was
in flight. This change implements the existing requirement; it adds no new behaviour.

## What Changes

- The receive loop no longer cancels `ReceiveMessage` when shutdown starts. The poll runs
  under its own deadline, the wait time plus 10 s. Shutdown waits for it to return.
- Messages returned by a receive that completes after shutdown has started are returned to
  the queue with `ChangeMessageVisibility(0)`. They are not claimed, executed, or recorded.
- Each received message is logged at info with its `message_id` and `receive_count`, so a
  redelivery can be seen in the journal.
- The sqshd unit sets `TimeoutStopSec=120`, because shutdown can now take up to one extra
  poll (about 30 s) before the drain grace starts.

**Not fixed here.** A response lost after SQS has delivered a message (a network failure
between the server and the daemon) still hides the message until the visibility timeout. That
is a property of SQS, not of the code path, and it is out of scope. The new log lines make it
visible if it happens.

**Unconfirmed.** The `is-active` request from the same run had no receive error logged and no
restart near it. This change does not claim to fix it. The new per-message log is intended to
show whether it recurs and where the message went.

## Capabilities

No requirement changes. SPEC §9.3 already requires this behaviour, so no delta specs are
created. `.openspec.yaml` declares `skip_specs: true`.

## Impact

- `internal/agent/agent.go`: receive loop context, post-receive shutdown handling, per-message log.
- `internal/agent/agent_test.go`: a test that delivers a message from a receive in flight at shutdown.
- `deploy/sqshd.service`: `TimeoutStopSec=120`.
- No change to `internal/awsx`, the wire protocol, the result store, or the CLI.
