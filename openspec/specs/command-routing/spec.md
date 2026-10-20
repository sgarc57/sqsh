# command-routing Specification

## Purpose

Defines how a command published by an operator reaches exactly one agent and no others,
and why an agent's own target check — not the message broker's routing — is the boundary
that decides whether a command runs.

## Requirements

### Requirement: Published requests carry routing attributes

When publishing a request, the operator tool SHALL attach a `host` attribute naming the
target, a `command` attribute naming the command, and a numeric `protocol_version`
attribute. The message body SHALL be the request JSON verbatim, with no broker-added
envelope.

#### Scenario: Publish attaches routing attributes
- **WHEN** an operator issues a command for host `web01`
- **THEN** the published message carries `host=web01`, the command name, and the protocol version as attributes

#### Scenario: Body is the request verbatim
- **WHEN** an agent receives a published message
- **THEN** the message body parses directly as a request with no unwrapping step

### Requirement: An agent consumes only its own queue

An agent SHALL consume from exactly one queue, identified by its configuration. It MUST
NOT read from queues belonging to other hosts.

Server-side routing SHALL deliver a request only to the queue of the host named in the
`host` attribute, so that an agent is never woken by traffic addressed elsewhere.

#### Scenario: Command reaches only its target
- **WHEN** three agents are running and a command is published for the second
- **THEN** only the second agent's queue receives the message
- **AND** the other two agents receive nothing

#### Scenario: Non-matching traffic does not consume delivery attempts
- **WHEN** commands are published for other hosts
- **THEN** this agent's queue receives no messages
- **AND** no message's delivery count is incremented on its behalf

### Requirement: Agent re-validates the target host

After parsing a request body, an agent SHALL compare `target.host` against its own
configured host identifier and SHALL reject any mismatch with reason `invalid_target`.

Server-side routing is an optimization, not the authorization boundary. A misconfigured
subscription MUST NOT cause a command to execute on the wrong host.

#### Scenario: Misrouted command is refused
- **WHEN** a message addressed to `web02` is delivered to the agent configured as `web01`
- **THEN** it is rejected with reason `invalid_target`
- **AND** no process is spawned
- **AND** a result is recorded

### Requirement: Bounded long-poll receive

An agent SHALL long-poll for messages and SHALL NOT request more messages than it has
free execution capacity, so that it never holds a message whose visibility timer is
running while it has no worker to start it.

#### Scenario: Receive is bounded by available capacity
- **WHEN** all execution slots are occupied
- **THEN** the agent requests no further messages until a slot frees

#### Scenario: Long poll returns empty
- **WHEN** no messages are available for the full poll interval
- **THEN** the agent polls again without error

### Requirement: Receive errors are transient, not fatal

An agent SHALL treat a failure to receive messages as transient: it SHALL back off
exponentially with jitter, count the failure, and retry. It MUST NOT exit.

#### Scenario: Broker unreachable
- **WHEN** the message broker returns errors for several consecutive receive attempts
- **THEN** the agent backs off with increasing delay and continues retrying
- **AND** the agent remains running

#### Scenario: Recovery after an outage
- **WHEN** the broker becomes reachable again after repeated failures
- **THEN** the agent resumes receiving and the backoff delay resets

### Requirement: Every message reaches a terminal disposition

An agent SHALL either execute a message or reject it, and SHALL delete the message from
its queue in both cases. A message MUST NOT be silently discarded, and a rejected message
MUST NOT be retried — a malformed message will be malformed on every delivery.

#### Scenario: Rejected message is deleted, not redelivered
- **WHEN** a message is rejected for any reason
- **THEN** a result recording the rejection is written
- **AND** the message is deleted from the queue
