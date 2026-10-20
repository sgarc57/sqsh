## Context

See `proposal.md` — Why. The repository contains `SPEC.md` and nothing else; there is no
existing code, no module, and no CI. Every decision here is a first decision.

Two constraints shape the approach:

- **`SPEC.md` is the design reference, not this document.** Where `SPEC.md` already
  decides something (§20's rejected-alternatives table), this design follows it and does
  not relitigate. What this document decides is what a *subset* looks like — which shapes
  must exist now so the deferred parts can be added without restructuring.
- **This slice exists to falsify one assumption.** If SNS filter policies do not route as
  §3 assumes, the per-host queue model, the two-identity split, and the §18 ownership
  tiers are all built on sand. Test design matters more here than production hardening.

## Goals / Non-Goals

**Goals:**

- Prove server-side routing end to end, against real AWS at least once.
- Establish the seams the deferred work plugs into — command semantics, the store
  interfaces, the config shape — so later changes add implementations rather than
  reshaping call sites.
- Make every AWS call site testable without an account.

**Non-Goals:**

- Production readiness. No systemd unit, no packaging, no privilege model. The daemon runs
  in the foreground.
- S3 output offload (§12). Output is truncated at the buffer limit and the result is
  flagged; there is no `--full` retrieval path.
- Any local durability. There is no dedupe store and no result spool, so an agent that
  cannot reach DynamoDB reports the failure and moves on.

## Decisions

### Declare command semantics now, ship only one value

`Command` carries a `Semantics()` returning `AtLeastOnce` or `AtMostOnce`, even though
every command in this slice is `AtLeastOnce` and the `AtMostOnce` branch of §8.2 is not
implemented.

*Why:* §8.1 exists because the previous draft's retry rule would reboot-loop a host. The
per-command declaration is the mechanism that prevents it. Introducing the enum later
means revisiting the message-handling path with the dangerous default already in place;
introducing it now means the `AtMostOnce` change adds a branch to a switch that already
exists. The cost is one unused enum value.

*Alternative rejected:* omit `Semantics` and add it with `reboot`. Cheaper now, but it
makes the highest-risk change in the roadmap also a refactor.

### Enforce result-status monotonicity from the first commit

`Complete` refuses backward status writes per §7.2.1, even though the `accepted` status
that motivates the rule ships with `reboot` in a later change.

*Why:* the rule is what makes replay safe, and it is a store-level invariant. Adding it
later means auditing every existing write path for violations instead of never permitting
one. It also gives the store conformance suite its shape immediately.

*Alternative rejected:* last-write-wins now, monotonic later. This is the kind of
invariant that is cheap to establish and expensive to retrofit.

### Host identity comes from config, and the store is the only collision detector

`host_id` and the queue URL are configuration values. Identity claiming, `instance_ref`,
and inventory (§6.2–6.4) are deferred wholesale.

*Why:* claiming is a distinct problem with its own conflict semantics, and this slice
does not need a name to be *earned* — only to be *checked*. The agent still re-validates
`target.host` (§6.1), which is the property that matters.

*Consequence, accepted:* two hosts misconfigured with the same `host_id` will both
subscribe to the same name and both execute. §6.3's claim protocol is what prevents this
in the full design, and it is absent here. The conditional `Claim` still catches the
duplicate and rejects one side, so the damage is bounded to "an arbitrary one of the two
ran it" — the same window §9.4 describes. Until claiming ships, `host_id` uniqueness is an
operator responsibility, and a `duplicate` rejection should be treated as a
misconfiguration alarm rather than a curiosity.

### Narrow interfaces at every AWS boundary

Each AWS dependency is consumed through an interface declaring only the operations used —
publish, receive, delete, extend visibility, and the `ResultStore` triple from §7.5.

*Why:* §5 requires it, and it is what lets the dispatcher, protocol, and result-handling
tests run with no account and no LocalStack. It also means the `store: s3` option deferred
by §21.5 stays open at the cost of an interface, not an implementation.

### Configuration validation is fail-fast and total

Unknown keys are a startup error, and derived constraints — notably
`visibility_timeout ≥ max_command_timeout + 30s` from §8.5 — are checked at startup, not
at use.

*Why:* §13's reasoning is that a typo'd `allowed_commands` must not silently allow
nothing. The same argument covers the derived timeout: a config that violates it produces
duplicate deliveries under load, which is the hardest possible failure to diagnose from
the outside.

### `puppet` is registered and disabled, not absent

`puppet` exists in the registry with a validator and semantics, marked disabled, and is
rejected as `command_not_allowed`.

*Why:* the operator does not run Puppet, but §1 keeps "trigger Puppet" as a first-class
use case and the shape is worth preserving. Registering it disabled exercises the
disabled-command path in the dispatcher — a path that otherwise ships untested — and
documents the intended integration without implying it works.

### Test the routing premise against real AWS, not only LocalStack

The multi-agent isolation test runs in LocalStack in CI, and is additionally run once
manually against a real account before this change is considered done.

*Why:* the premise under test is a specific behaviour of SNS subscription filter policies.
LocalStack reimplements that behaviour; if its reimplementation is more permissive than
the real service, a green test proves nothing about the assumption the whole architecture
rests on. This is the one place where CI-only verification is insufficient.

## Risks / Trade-offs

**LocalStack's filter-policy semantics may diverge from real SNS** → The premise of the
whole change would appear validated when it is not. Mitigated by the mandatory manual
run against a real account; the task is not optional and its result is recorded.

**`SPEC.md` and shipped behaviour diverge for the duration of the roadmap** → An operator
reading `SPEC.md` sees `reboot`, `script`, claiming, and degraded mode, none of which
exist. Mitigated by the specs in `openspec/specs/` describing shipped behaviour while
`SPEC.md` remains the design target. Accepting the divergence is preferable to editing
`SPEC.md` down to the subset and losing the design.

**No local durability means results can be lost** → A command executes, DynamoDB is
unreachable, the outcome is never recorded, and the operator's wait times out with no way
to learn what happened. This is exactly the gap §8.7 closes, and it is real for as long
as the spool is deferred. Bounded by scope: read-only `systemctl` commands are safe to
re-issue, which is why only `AtLeastOnce` commands ship in this slice.

**Duplicate `host_id` is undetected before execution** → See the identity decision above.
Bounded by the conditional claim, alarmed by `duplicate` rejections, closed by the
claiming change.

**Foreground daemon with no privilege model** → Not deployable. Accepted deliberately;
§14 is a later change and shipping a half-hardened unit is worse than shipping none.

## Migration Plan

Greenfield — there is no deployed version and no data to migrate.

Deployment for validation purposes is manual: create the topic, one queue per test host
with its queue policy and redrive policy, one subscription per queue with a filter policy
naming that host, and the results table. These resources are later replaced by the §18
OpenTofu repository, which is why nothing here should encode resource creation beyond what
the tests need.

Rollback is deletion of those resources; no state outlives them except result records,
which expire on their own retention.

## Open Questions

**Resolved while writing tasks:** §15.2's metric table is mostly about failure modes that
do not exist yet — spool age, claim skips, name conflicts. This slice ships a minimal
counter set covering only what it can actually observe (messages received, commands by
status, rejections by reason, command duration, AWS errors, last successful poll), and
each later change adds the metrics for the subsystem it introduces. Structured logging is
in scope; the full §15.2 table is not.

- **Whether the integration fleet size should exceed three.** Three agents demonstrate
  isolation; a larger fleet would exercise filter-policy behaviour at a scale closer to
  real use. Purely a test-parameter question — it changes a constant, not the breakdown.
