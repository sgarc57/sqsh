## 1. Diagnostics

- [x] 1.1 Log each received message at info with `message_id` and `receive_count`, in `Run` after a receive returns. The log-correlation test exempts this line, since it precedes parsing

## 2. Receive path

- [x] 2.1 Run each `ReceiveMessage` under a context bounded by `WaitTimeSeconds + 10 s`, not the Run context (design D1)
- [x] 2.2 When a receive returns after shutdown has started, call `ChangeMessageVisibility(0)` on its messages, release their slots, and do not handle them (design D2). This applies to a successful receive and to one that returns an error
- [x] 2.3 Keep the existing backoff for receive errors when the Run context is still live

## 3. Tests

- [x] 3.1 Test: a receive in flight when shutdown starts returns a message; the message is returned with visibility 0, not executed, not claimed, and receivable again. The test fails on the current code. It was run against the unfixed loop and failed on the context check
- [x] 3.2 Test: a receive that completes before shutdown is handled as today (existing tests cover this; they pass)
- [x] 3.3 Run `go test ./...` and `go vet ./...`. Both pass. The race detector is clean on the receive and shutdown tests

## 4. Deploy and verify

- [x] 4.1 Add `TimeoutStopSec=120` to `deploy/sqshd.service`
- [x] 4.2 Build for arm64 with `make build-linux-arm64`, and install and restart as part of `make e2e`
- [x] 4.3 Rerun `make e2e`. Run 4 passed every check, and every stored record shows `attempt: 1`
- [x] 4.4 If a request is still stranded, read its `message received` lines in the journal and record the finding in this change before changing code

## 5. Investigation: the unexplained `is-active` loss

Recorded because run 2 of `validate-remote-node` showed an `is-active` request with
`attempt: 2` and no trace of its first delivery. Its mechanism is still unconfirmed.

- [x] 5.1 Run 5 of `make e2e`, on the build with the fix and SDK retry logging, passed every
  check. Its records show `attempt: 1`.
- [x] 5.2 Added SDK retry logging to `cmd/sqshd/main.go` (`aws.LogRetries` routed to the daemon
  log as `aws sdk retry`). Uncommitted. The first receive of a message whose response was lost
  would show up there as a retried `ReceiveMessage`.
- [x] 5.3 Stress test: 40 `is-active` requests published back to back, the same pattern as run 2.
  Waited 120 s, past the 90 s visibility timeout. Results:
  - 40 of 40 records `succeeded` with `attempt: 1`
  - 40 `message received`, 40 `executing command`, and 0 `aws sdk retry` or `receive failed`
    lines in the journal. Each message ID appears exactly three times: receive, execute, complete.
  - Conclusion: no loss in this sample. The run-2 loss did not reproduce, and no retry occurred.
- [ ] 5.4 Not established: whether the run-2 loss was an SDK retry. The run-2 binary had no retry
  logging, so the journal cannot show it. 40 clean requests make a rare loss less likely but do
  not rule it out. If `aws sdk retry` appears in a future journal next to a message that never
  reaches `message received`, that is the cause to act on.
- [ ] 5.5 Open: a response lost after SQS delivers a message still hides it for the visibility
  timeout. The fix does not address this (see proposal "Not fixed here").
