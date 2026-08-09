## Purpose

Defines the wire contract between operator and agent: what constitutes a valid command
request, what a result record contains, and how both evolve without breaking agents that
are already deployed.

## ADDED Requirements

### Requirement: Request field validation

A request SHALL be rejected unless it carries `version`, `request_id`, `target.host`,
`command`, `args`, `issued_at`, `expires_at`, `timeout_seconds`, and `issuer`. `args` MAY
be empty but MUST be present. `request_id` MUST be a UUIDv7. Timestamps MUST be RFC 3339
with a `Z` suffix.

A request that fails validation SHALL be rejected with reason `malformed` and MUST NOT be
executed.

#### Scenario: Request omits a required field
- **WHEN** a request arrives without `expires_at`
- **THEN** it is rejected with reason `malformed`
- **AND** no process is spawned

#### Scenario: Request carries a non-UUIDv7 request_id
- **WHEN** a request arrives with `request_id` set to `"not-a-uuid"`
- **THEN** it is rejected with reason `malformed`

#### Scenario: Request with empty args is valid
- **WHEN** a request arrives with `args` set to `[]` and all other fields valid
- **THEN** validation succeeds

### Requirement: Version handling

An agent SHALL reject any request whose `version` it does not understand, with reason
`unsupported_version`, rather than attempting a best-effort parse.

An agent SHALL ignore unknown fields at any level of the request, so that additive
protocol changes require no version bump.

#### Scenario: Unknown protocol version
- **WHEN** a request arrives with `version: 2` and the agent understands only version 1
- **THEN** it is rejected with reason `unsupported_version`

#### Scenario: Unknown field is ignored
- **WHEN** a valid version 1 request arrives carrying an additional unrecognised field
- **THEN** the request is accepted and the unknown field is ignored

### Requirement: Time window enforcement

An agent SHALL reject a request whose `issued_at` is further in the future than the
configured maximum clock skew, with reason `malformed`.

An agent SHALL reject a request as `not_yet_valid` when `not_before` is present and the
current time precedes it, and as `expired` when the current time is past `expires_at`.

`expires_at` is mandatory: a command that was published during a network partition MUST
NOT execute when connectivity returns after it has expired.

#### Scenario: Command has expired before it is received
- **WHEN** a request arrives whose `expires_at` is in the past
- **THEN** it is rejected with reason `expired`
- **AND** no process is spawned

#### Scenario: Command is not yet valid
- **WHEN** a request arrives whose `not_before` is in the future
- **THEN** it is rejected with reason `not_yet_valid`

#### Scenario: Clock skew beyond tolerance
- **WHEN** a request arrives whose `issued_at` exceeds the configured maximum clock skew into the future
- **THEN** it is rejected with reason `malformed`

### Requirement: Timeout bounds

An agent SHALL reject a request with reason `invalid_timeout` unless `timeout_seconds` is
at least 1 and no greater than the agent's configured maximum command timeout.

#### Scenario: Timeout exceeds the agent maximum
- **WHEN** a request specifies `timeout_seconds` greater than the configured maximum
- **THEN** it is rejected with reason `invalid_timeout`

#### Scenario: Zero timeout
- **WHEN** a request specifies `timeout_seconds: 0`
- **THEN** it is rejected with reason `invalid_timeout`

### Requirement: Canonical signing string is fixed and stable

The system SHALL compute a canonical signing string over a request using a byte-exact,
length-prefixed encoding of the request's identifying fields, such that no two distinct
argument lists can produce the same signing string.

The encoding SHALL be stable across releases. A change to it is a breaking protocol
change.

Signatures SHALL NOT be verified in this release: a request carrying a `signature` is
neither validated against it nor rejected for it.

#### Scenario: Signing string is byte-stable
- **WHEN** the canonical signing string is computed for a known request
- **THEN** it matches the recorded reference encoding byte for byte

#### Scenario: Argument boundaries are unambiguous
- **WHEN** signing strings are computed for the argument lists `["a", "bc"]` and `["ab", "c"]`
- **THEN** the two signing strings differ

#### Scenario: Signature field is ignored
- **WHEN** a valid request arrives carrying an arbitrary `signature` value
- **THEN** the request is processed normally and the signature is not verified

### Requirement: Issuer is informational only

The `issuer` field SHALL be recorded for audit and MUST NOT be used for any authorization
decision. Authorization is established by the publishing principal's IAM permissions, not
by a self-declared field in the message body.

#### Scenario: Issuer claiming elevated identity gains nothing
- **WHEN** a request arrives whose `issuer` names a principal the sender does not hold
- **THEN** the request is evaluated solely against the agent's allowlist
- **AND** the declared `issuer` is recorded on the result unchanged
