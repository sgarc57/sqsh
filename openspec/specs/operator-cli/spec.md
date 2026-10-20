# operator-cli Specification

## Purpose

Defines the operator-facing interface for issuing a command to a host and learning what
happened: how invocations are expressed, how results are awaited, and what each exit code
commits the tool to meaning.

## Requirements

### Requirement: Command invocation names a host and a command

The operator tool SHALL accept a target host and a command with its arguments, publish a
corresponding request, and report the assigned `request_id`.

Each invocation SHALL generate a fresh `request_id`. The tool MUST NOT reuse an identifier
across invocations.

#### Scenario: Issuing a command
- **WHEN** an operator runs the tool naming a host and an allowed command
- **THEN** a request is published for that host
- **AND** the assigned `request_id` is reported

#### Scenario: Repeated invocations are distinct requests
- **WHEN** the same command is issued twice against the same host
- **THEN** two distinct `request_id` values are produced

### Requirement: Waiting for a result is the default

The tool SHALL wait for a terminal result by default, polling until one is recorded or a
wait timeout elapses. It SHALL provide a mode that publishes and returns immediately
without waiting.

An explicit expiry SHALL default to a short interval suited to interactive use, and SHALL
be overridable per invocation.

#### Scenario: Waiting until completion
- **WHEN** an operator issues a command in the default mode
- **THEN** the tool polls until a terminal result is recorded
- **AND** prints the outcome

#### Scenario: Fire and forget
- **WHEN** an operator issues a command in no-wait mode
- **THEN** the tool prints the `request_id` and returns without polling

#### Scenario: Fetching a result later
- **WHEN** an operator requests the status of a previously issued `request_id`
- **THEN** the recorded result is printed if one exists

### Requirement: Exit codes are a stable contract

The tool SHALL exit `0` when the remote command succeeded, `1` when it ran and failed or
timed out, `2` on a usage error, `3` when the request was rejected, `4` when it gave up
waiting, and `6` on a local or infrastructure error.

Exit codes MUST distinguish "the remote command failed" from "this tool could not
determine what happened".

#### Scenario: Remote command succeeds
- **WHEN** the awaited result has status `succeeded`
- **THEN** the tool exits 0

#### Scenario: Remote command fails
- **WHEN** the awaited result has status `failed` or `timed_out`
- **THEN** the tool exits 1

#### Scenario: Request rejected
- **WHEN** the awaited result has status `rejected` or `expired`
- **THEN** the tool exits 3
- **AND** the rejection reason is shown

#### Scenario: Local failure is distinguishable
- **WHEN** the tool cannot reach the messaging or storage backend
- **THEN** it exits 6
- **AND** does not report the remote command as failed

### Requirement: Giving up waiting is not cancellation

When the wait timeout elapses, the tool SHALL state that it stopped waiting and that the
command may still be running. It MUST NOT imply the command was stopped or cancelled —
nothing in this system can recall an in-flight command.

The tool SHALL print the `request_id` and how to retrieve the result later.

#### Scenario: Wait timeout wording and exit
- **WHEN** the wait timeout elapses before a terminal result is recorded
- **THEN** the tool exits 4
- **AND** reports that the command may still be running
- **AND** prints the `request_id` and the means to retrieve the result later

### Requirement: Machine-readable output is available

The tool SHALL provide a mode that emits the raw result record in a machine-readable form
suitable for scripting, in addition to its default human-readable output.

#### Scenario: Structured output
- **WHEN** an operator requests machine-readable output
- **THEN** the result record is emitted in a parseable structured form

### Requirement: Truncated output is signalled

When a result's captured output was truncated, the tool SHALL indicate this to the
operator rather than presenting the partial output as complete.

#### Scenario: Truncation is visible
- **WHEN** a result marked as truncated is displayed
- **THEN** the operator is told the output is incomplete
