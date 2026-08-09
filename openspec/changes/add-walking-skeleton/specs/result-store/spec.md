## Purpose

Defines how the outcome of every request is recorded and read back: the record's contents,
the guarantee that a request runs at most once, and how long outcomes remain available to
operators and audit.

## ADDED Requirements

### Requirement: Every request produces exactly one result record

Every request an agent accepts from its queue SHALL produce a result record keyed by
`request_id`, whether it executed or was rejected. A request MUST NOT complete without a
recorded outcome.

A result SHALL record the request identifier, target host, status, exit code where one is
observable, captured output, timing, and the requesting `issuer`.

#### Scenario: Successful execution is recorded
- **WHEN** a command runs and exits zero
- **THEN** a result is recorded with status `succeeded`, exit code 0, and the captured output

#### Scenario: Rejection is recorded
- **WHEN** a request is rejected during validation
- **THEN** a result is recorded with status `rejected` and the specific rejection reason
- **AND** the rejection is not a silent drop

### Requirement: Result status values

A result SHALL carry exactly one status. This release defines `running` for a claimed
in-progress request, and the terminal values `succeeded`, `failed`, `timed_out`,
`rejected`, and `expired`.

A rejected result SHALL carry a rejection reason drawn from a closed set:
`unsupported_version`, `malformed`, `invalid_target`, `unknown_command`,
`command_not_allowed`, `arg_not_allowed`, `invalid_timeout`, `not_yet_valid`, `expired`,
`duplicate`, and `concurrency_limit`.

#### Scenario: Non-zero exit is a failure, not an error
- **WHEN** a command runs to completion and exits non-zero
- **THEN** the result status is `failed` and the exit code is recorded

#### Scenario: Rejection reason is from the closed set
- **WHEN** any rejection is recorded
- **THEN** its reason is one of the defined values

### Requirement: Claiming a request is conditional and detects duplicates

Before executing, an agent SHALL claim the request by writing a `running` record
conditional on no record already existing for that `request_id`.

A claim that fails because a record already exists SHALL cause the request to be rejected
with reason `duplicate`, and the command MUST NOT execute a second time.

#### Scenario: Duplicate delivery executes once
- **WHEN** the same request is delivered twice
- **THEN** exactly one execution occurs
- **AND** the second delivery is rejected with reason `duplicate`

#### Scenario: Concurrent duplicate delivery
- **WHEN** the same request is delivered twice concurrently
- **THEN** exactly one claim succeeds and exactly one execution occurs

### Requirement: Results are readable by request identifier

The store SHALL support retrieving a single result by its `request_id` as a point read,
returning the current record or indicating that none exists. A read of a just-written
record SHALL observe that write.

#### Scenario: Result is readable immediately after completion
- **WHEN** a result is written and then read by its `request_id`
- **THEN** the read returns the written record

#### Scenario: Unknown request identifier
- **WHEN** a result is requested for a `request_id` that was never issued
- **THEN** the store reports that no such record exists

### Requirement: Results expire on a defined retention

Each result SHALL carry an expiry so that records are removed automatically after the
configured retention period without operator action.

#### Scenario: Result carries an expiry
- **WHEN** a result is written
- **THEN** it carries an expiry consistent with the configured retention period

### Requirement: Store failures are surfaced, never silently swallowed

A failure to record a result SHALL be counted and logged. This release does not provide
local durability for unrecorded results, so an agent that cannot reach the store SHALL
report the condition rather than presenting the command as recorded.

#### Scenario: Store write fails
- **WHEN** recording a result fails
- **THEN** the failure is logged and counted
- **AND** the outcome is not reported as successfully recorded
