## Purpose

Defines how an agent turns a validated request into a running process: which commands
exist, how their arguments are accepted or refused, and the execution environment they
run in. The controlling property is that no operator-supplied text ever reaches a shell.

## ADDED Requirements

### Requirement: No shell on any execution path

An agent SHALL execute commands by direct process invocation with an explicit argument
vector. It MUST NOT pass any part of a request to a shell, and MUST NOT construct a
command line by string concatenation.

#### Scenario: Shell metacharacters are inert
- **WHEN** a request supplies an argument containing `;`, `|`, `$(...)`, or a newline
- **THEN** the argument is either refused by validation or passed as a single literal argument value
- **AND** no additional process is created as a result of its content

#### Scenario: Executable is resolved absolutely
- **WHEN** any command is executed
- **THEN** the invoked program is named by an absolute path
- **AND** resolution does not depend on the `PATH` environment variable

### Requirement: Allowlist by construction

Argument validation SHALL accept only values that exactly match a known-good set. It MUST
NOT operate by scanning for dangerous patterns and accepting whatever remains.

Validation SHALL run over the complete argument list before any process is spawned.

#### Scenario: Traversal attempt is refused by non-membership
- **WHEN** a request supplies an argument such as `../../evil` or `..%2f`
- **THEN** it is rejected with reason `arg_not_allowed` because it is not in the allowed set

#### Scenario: Embedded null is refused
- **WHEN** a request supplies an argument containing a null byte
- **THEN** it is rejected with reason `arg_not_allowed`

#### Scenario: Validation precedes execution
- **WHEN** a request supplies a valid first argument and an invalid second argument
- **THEN** the request is rejected and no process is spawned

### Requirement: Unknown and disallowed commands are refused

An agent SHALL reject a request naming a command it does not implement with reason
`unknown_command`, and SHALL reject a request naming an implemented command that is
absent from its configured allowlist with reason `command_not_allowed`.

A command MAY be registered but disabled. A disabled command SHALL behave as though it
were absent from the allowlist.

#### Scenario: Unimplemented command
- **WHEN** a request names a command the agent does not implement
- **THEN** it is rejected with reason `unknown_command`

#### Scenario: Implemented command absent from the allowlist
- **WHEN** a request names an implemented command that this agent's configuration does not permit
- **THEN** it is rejected with reason `command_not_allowed`

#### Scenario: Registered but disabled placeholder
- **WHEN** a request names a command that is registered as a disabled placeholder
- **THEN** it is rejected with reason `command_not_allowed`
- **AND** no process is spawned

### Requirement: Read-only systemctl command

The agent SHALL provide a `systemctl` command supporting only the read-only verbs
`status`, `is-active`, `is-enabled`, and `show`.

The verb and the unit SHALL be validated independently against separate allowlists, so
that permitting a verb does not permit it against every unit and permitting a unit does
not permit every verb against it.

State-changing verbs SHALL be refused in this release.

#### Scenario: Permitted verb against permitted unit
- **WHEN** a request invokes `systemctl status` against an allowlisted unit
- **THEN** the command executes and its output is captured

#### Scenario: State-changing verb is refused
- **WHEN** a request invokes `systemctl restart` against an allowlisted unit
- **THEN** it is rejected with reason `arg_not_allowed`

#### Scenario: Permitted verb against a non-allowlisted unit
- **WHEN** a request invokes `systemctl status` against a unit absent from the unit allowlist
- **THEN** it is rejected with reason `arg_not_allowed`

#### Scenario: Unit name variants are not accepted by prefix
- **WHEN** a request supplies a unit such as `nginx@1.service` or `-nginx` while only `nginx.service` is allowlisted
- **THEN** it is rejected with reason `arg_not_allowed`

### Requirement: Controlled execution environment

An executed command SHALL run with an empty environment plus an explicit, fixed set of
variables, with standard input connected to an empty source, and with a locale that makes
output parsing stable.

#### Scenario: Environment is not inherited
- **WHEN** a command executes while the agent's own environment contains arbitrary variables
- **THEN** the child process sees only the explicitly configured variables

#### Scenario: Standard input is empty
- **WHEN** a command that would read standard input executes
- **THEN** it observes end-of-input immediately and does not block

### Requirement: Execution is time-bounded

An agent SHALL terminate a command that exceeds its request's `timeout_seconds` and SHALL
record the outcome as `timed_out`. Termination SHALL escalate from a graceful signal to a
forced kill if the process does not exit.

#### Scenario: Command exceeds its timeout
- **WHEN** a command runs longer than `timeout_seconds`
- **THEN** the process is terminated
- **AND** the result status is `timed_out`

#### Scenario: Process ignores graceful termination
- **WHEN** a timed-out process does not exit after a graceful signal
- **THEN** it is forcibly killed

### Requirement: Output is bounded

An agent SHALL capture standard output and standard error into bounded buffers and SHALL
mark a result as truncated when a command produces more output than the configured limit.
A command producing unbounded output MUST NOT exhaust agent memory.

Command output SHALL NOT be written to logs at informational level, because it may be
large and may contain secrets.

#### Scenario: Output exceeds the limit
- **WHEN** a command emits more output than the configured maximum
- **THEN** the recorded output is truncated to the limit
- **AND** the result is marked as truncated

#### Scenario: Output is absent from normal logs
- **WHEN** a command completes successfully with output
- **THEN** the informational log entry records the outcome without the output body

### Requirement: Bounded concurrency

An agent SHALL execute no more than its configured maximum number of commands
concurrently. A request arriving when no slot is available and none can be awaited SHALL
be rejected with reason `concurrency_limit` rather than queued indefinitely.

#### Scenario: Concurrency ceiling is respected
- **WHEN** more commands are dispatched than the configured maximum
- **THEN** the number executing at any moment never exceeds the maximum
