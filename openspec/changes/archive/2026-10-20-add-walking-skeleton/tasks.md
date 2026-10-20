## 1. Repository foundation

- [x] 1.1 Initialise the Go module as `sqsh` (private module, served from the internal git host — no public domain path) and commit a `.gitignore` covering build output and local credentials
- [x] 1.2 Create the package skeleton from `SPEC.md` §5, limited to this slice: `cmd/sqshd`, `cmd/sqsh`, and `internal/{protocol,config,dispatcher,commands,awsx,results,agent,output,observability,version}`
- [x] 1.3 Implement `internal/version` with build-time metadata (version, commit) wired through `-ldflags`
- [x] 1.4 Add a `Makefile` or equivalent with `build`, `test`, `lint`, and `integration` targets so later tasks have one entry point

## 2. Configuration

- [x] 2.1 Define the config struct covering only this slice's keys: `host_id`, `region`, `topic_arn`, `queue_url`, `results_table`, `wait_time_seconds`, `visibility_timeout`, `max_concurrent_commands`, `default_command_timeout`, `max_command_timeout`, `max_clock_skew`, `shutdown_grace`, `max_output_bytes`, `allowed_commands`, and the per-command allowlists
- [x] 2.2 Implement YAML loading with **unknown keys as a startup error** (§13) — a typo'd `allowed_commands` must not silently allow nothing
- [x] 2.3 Implement defaults and total validation, including the derived constraint `visibility_timeout >= max_command_timeout + 30s` (§8.5); fail startup with exit 78 on any violation
- [x] 2.4 Write `deploy/config.example.yaml` documenting every key this slice reads
- [x] 2.5 Table-driven tests: unknown key rejected, derived timeout constraint enforced, defaults applied, each required key missing

## 3. Wire protocol

- [x] 3.1 Define request and result types per §7.1–7.2, with the status and rejection-reason enums restricted to this slice's values (no `accepted`)
- [x] 3.2 Implement request validation: required fields, UUIDv7 `request_id`, RFC 3339 UTC timestamps, `1 <= timeout_seconds <= max_command_timeout`
- [x] 3.3 Implement version handling — reject unknown `version` as `unsupported_version`, ignore unknown fields at every level (§7.3)
- [x] 3.4 Implement time-window checks: `max_clock_skew` on `issued_at`, `not_before` → `not_yet_valid`, `expires_at` → `expired`
- [x] 3.5 Implement the canonical signing string of §7.4 exactly — length-prefixed arguments, `-` for absent `not_before`, second-precision `Z` timestamps. Compute only; do not verify
- [x] 3.6 Golden-file test locking the signing string byte for byte, plus a test asserting `["a","bc"]` and `["ab","c"]` produce different strings
- [x] 3.7 Round-trip tests: unknown fields preserved-ignored, unknown version rejected, each validation failure maps to its documented rejection reason

## 4. Command dispatch

- [x] 4.1 Define the `Command` interface per §10 with `Name`, `Semantics`, `Validate`, and `Execute`; define the `Semantics` enum with both values even though only `AtLeastOnce` ships (see design.md)
- [x] 4.2 Implement the registry with enabled/disabled state, resolving `unknown_command` versus `command_not_allowed` correctly
- [x] 4.3 Implement the execution harness: absolute `argv[0]`, empty environment plus explicit `PATH`/`LANG`/`HOME`, stdin from `/dev/null`, no shell on any path
- [x] 4.4 Implement bounded output capture with a truncation flag; assert memory does not grow with command output volume
- [x] 4.5 Implement per-request timeout with escalation from graceful signal to forced kill, recording `timed_out`
- [x] 4.6 Implement the `systemctl` command restricted to `status`, `is-active`, `is-enabled`, `show`, validating verb and unit against **separate** allowlists (§11.3)
- [x] 4.7 Register `puppet` as a disabled placeholder with its validator and semantics declared, rejected as `command_not_allowed` (see design.md)
- [x] 4.8 Table-driven validator tests covering the full §17 rejection corpus: `../../evil`, `..%2f`, `nginx.service\x00`, `nginx.service;reboot`, `-nginx`, `--all`, `nginx@1.service`, absolute paths, 64 KiB argument, empty args, 3 args
- [x] 4.9 Test that validation runs over the whole argument list before any process is spawned, and that the disabled-command path is exercised

## 5. Result store

- [x] 5.1 Define the `ResultStore` interface from §7.5 (`Claim`, `Complete`, `Get`) with the `ErrAlreadyClaimed` sentinel
- [x] 5.2 Implement `Claim` as a conditional write on `attribute_not_exists(request_id)`, mapping `ConditionalCheckFailedException` to `ErrAlreadyClaimed`
- [x] 5.3 Implement `Complete` enforcing §7.2.1 status monotonicity — backward writes dropped, counted, never applied (see design.md)
- [x] 5.4 Implement `Get` as a point read distinguishing "no such record" from an error, and set per-item TTL from the configured retention
- [x] 5.5 Build an in-memory `ResultStore` fake for use by every non-integration test
- [x] 5.6 Write the store conformance suite — run it against both the fake and the DynamoDB implementation; the load-bearing assertions are `ErrAlreadyClaimed` on second claim and refusal of a backward status write

## 6. AWS transport

- [x] 6.1 Define narrow interfaces for publish, receive, delete, and change-visibility so no call site depends on the SDK directly (§5)
- [x] 6.2 Implement the SNS publisher attaching the `host`, `command`, and `protocol_version` message attributes (§6.1)
- [x] 6.3 Implement the SQS consumer with long polling, `ApproximateReceiveCount` requested, and exponential backoff with full jitter on receive errors — errors counted, never fatal
- [x] 6.4 Implement visibility extension for running `AtLeastOnce` commands per §8.5, capped so a wedged process cannot hold a message indefinitely
- [x] 6.5 Fakes for all four operations, sufficient to drive the agent's handling path with no account

## 7. Agent

- [x] 7.1 Implement the worker pool and the receive loop of §9.2, never requesting more messages than there are free slots
- [x] 7.2 Implement the message handling path: parse → validate → re-check `target.host` against `host_id` → claim → dispatch → complete → delete, with every rejection producing a recorded result and a deleted message
- [x] 7.3 Implement `invalid_target` rejection and assert it fires even when the filter policy delivered the message — the filter policy is not the authorization boundary (§6.1)
- [x] 7.4 Implement `concurrency_limit` rejection for the case where no slot is available at dispatch time
- [x] 7.5 Implement the startup sequence for this slice: load config → validate → verify AWS identity and log the ARN → resolve queue URL → start metrics, receive loop; fail fast with exit 78 on any config or identity error
- [x] 7.6 Implement graceful shutdown per §9.3: stop receiving, drain within `shutdown_grace`, escalate to kill, return unstarted `AtLeastOnce` messages with `ChangeMessageVisibility(0)`
- [x] 7.7 Implement structured JSON logging with `request_id` on every line, and command output logged only at debug and truncated (§15.1)
- [x] 7.8 Implement the minimal metric set decided in design.md: messages received, commands by command and status, rejections by reason, command duration, AWS errors by API, last successful poll timestamp
- [x] 7.9 Test the full handling path against fakes: each rejection reason, successful execution, duplicate delivery, and that a rejected message is deleted rather than redelivered

## 8. Operator CLI

- [x] 8.1 Implement `sqsh systemctl <host> <verb> <unit>` generating a fresh UUIDv7 `request_id` per invocation and publishing with the routing attributes
- [x] 8.2 Implement `sqsh status <request_id>` fetching a stored result
- [x] 8.3 Implement the global flags this slice supports: `--wait`/`--no-wait`, `--wait-timeout`, `--timeout`, `--expires-in`, `--json`
- [x] 8.4 Implement result polling with backoff from 250 ms to 2 s, terminating on any terminal status
- [x] 8.5 Implement the exit-code contract: `0` succeeded, `1` failed or timed out, `2` usage, `3` rejected or expired, `4` gave up waiting, `6` local or AWS error. Exit code `5` is **not** used in this slice — it means "host unknown or stale", which requires the deferred inventory
- [x] 8.6 Implement wait-timeout output that states the command may still be running, prints the `request_id` and the `sqsh status` command to resume, and never implies cancellation
- [x] 8.7 Implement human-readable and `--json` output, signalling truncation explicitly in both
- [x] 8.8 Tests for exit-code mapping from every terminal status, and for wait-timeout wording and behaviour

## 9. Verification

- [x] 9.1 Add a LocalStack harness bringing up SNS, SQS, and DynamoDB with a topic, three per-host queues, three subscriptions with filter policies, and the results table
- [x] 9.2 Write the isolation test: three agents running, a command published for each in turn, asserting every agent receives only its own messages and no other agent's queue is touched
- [x] 9.3 Write the end-to-end test: publish → route → dispatch → result → CLI reports it, asserting the CLI's exit code matches the recorded status
- [x] 9.4 Write the duplicate-delivery test: same `request_id` delivered twice, concurrently and sequentially, asserting exactly one execution
- [x] 9.5 Write the visibility test: a command outliving one visibility timeout, asserting extension occurs and no second delivery happens
- [x] 9.6 Add a `execute: false` dry-run mode logging the exact `argv` without spawning, so command wiring is testable on a dev box (§17)
- [x] 9.7 **Run the isolation test once against a real AWS account** and record the outcome in the change. LocalStack reimplements filter-policy matching; a green LocalStack run does not by itself validate this change's premise (see design.md — Risks)
- [x] 9.8 Update `README.md` with the manual resource setup, how to run the daemon in the foreground, and an explicit statement of what is not yet implemented
