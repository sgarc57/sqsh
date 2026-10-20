## Why

`SPEC.md` specifies the whole system to depth — identity claiming, run-once semantics,
degraded-mode spooling, infrastructure ownership tiers — and none of it is load-bearing
yet, because there is no code. Specifying every subsystem simultaneously means no single
assumption has been tested.

The assumption most worth testing first is the routing bet in §3: that SNS subscription
filter policies push host selection server-side, so an agent only ever sees messages
addressed to it. Everything downstream depends on it — per-host queues, meaningful
`maxReceiveCount`, the two-identity split in §6.2, the entire IaC ownership model in §18.
If server-side filtering does not behave as specified, that is worth discovering now
rather than after the run-once machinery is built on top of it.

This change builds the thinnest end-to-end slice that exercises that path and nothing
else. Everything difficult in §8 exists to make `reboot` safe; none of it is needed to
prove that a command reaches the right host and its result comes back.

## What Changes

- New Go module with two binaries: `sqshd` (daemon) and `sqsh` (CLI), per the §21.1
  naming decision resolved in draft 5.
- Wire protocol: request and result types, field validation, version rejection, and the
  §7.3 unknown-field-ignoring rule. The canonical signing string of §7.4 is implemented
  and golden-file tested but **not verified** — `require_signature` stays false.
- Publish path: `sqsh` publishes to the SNS topic with the `host`, `command`, and
  `protocol_version` message attributes that drive filter policies.
- Receive path: `sqshd` long-polls one SQS queue, re-validates `target.host` against its
  configured `host_id`, and rejects mismatches with `invalid_target` — the filter policy
  is an optimization, not the authorization boundary (§6.1).
- Dispatcher: command registry with per-command argument validators, absolute `argv[0]`,
  empty-plus-explicit environment, `/dev/null` stdin, bounded output buffers, and no
  shell on any path.
- One built-in command: `systemctl`, restricted to the read-only verbs `status`,
  `is-active`, `is-enabled`, and `show`, with verb and unit validated against separate
  allowlists (§11.3).
- Result store: DynamoDB `ResultStore` behind the §7.5 interface, with the conditional
  `Claim` returning `ErrAlreadyClaimed` and per-item TTL.
- Rejections are recorded outcomes, never silent drops (§7.2).

**Deliberately out of scope**, each deferred to its own later change:

| Deferred | Section | Why it can wait |
| --- | --- | --- |
| Identity claiming, inventory, heartbeat | §6.3–6.4 | `host_id` and `queue_url` come from config here. Claiming is a distinct problem with its own conflict semantics. |
| Dedupe store, startup reconciliation | §8.3–8.4 | Only `AtLeastOnce` commands ship in this slice, and they are safe to retry by definition. |
| `reboot`, `shutdown`, `AtMostOnce` | §8.1, §11.1 | The entire run-once apparatus exists for these. Adding them is what makes §8.3 mandatory. |
| Result spool, degraded mode | §8.7 | Meaningful only once execution must survive a store outage; read-only `systemctl` does not. |
| `script` | §11.5 | Needs the `os.Root` path resolution and scripts-dir validation to be correct first. |
| `puppet` | §11.4 | Registered as a **disabled placeholder** so the dispatcher shape supports it, but absent from the default allowlist. Not used in the target deployments. |
| systemd hardening, generated sudoers | §14.1–14.2 | The daemon runs in the foreground for this slice. |
| OpenTofu infrastructure | §18 | Separate repository by §5. Skeleton resources are created out of band. |
| Ed25519 verification | §7.4 | Reserved. The signing string is fixed; key distribution is a project of its own. |
| `store: s3` | §21.5 | The interface preserves the option; a second implementation means two conformance matrices forever. |

## Capabilities

### New Capabilities

- `command-protocol`: The wire format between operator and agent — request and result
  schemas, required-field and timestamp validation, version handling, and the canonical
  signing string reserved for future verification.
- `command-routing`: How a request reaches exactly one agent — SNS publish with routing
  attributes, per-host queue consumption, long-poll receive, and agent-side target
  re-validation.
- `command-dispatch`: How an agent turns a validated request into a process — command
  registry, allowlist-by-construction argument validation, and shell-free execution with
  a controlled environment.
- `result-store`: How outcomes are recorded and read — the claim/complete/get contract,
  status semantics, conditional-write duplicate detection, and retention.
- `operator-cli`: The `sqsh` operator interface — command invocation, result polling,
  output formatting, and exit-code contract.

### Modified Capabilities

None. This is the first change in the repository; `openspec/specs/` is empty.

## Impact

- **New code**: `cmd/sqshd`, `cmd/sqsh`, and `internal/{protocol,awsx,dispatcher,commands,results,config,output,observability,version}`. No existing code is touched — the repository currently contains only `SPEC.md` and OpenSpec scaffolding.
- **New dependencies**: AWS SDK for Go v2 (SNS, SQS, DynamoDB), a YAML parser, and a Prometheus client. Every AWS call site sits behind a narrow interface per §5, so tests need no live account.
- **AWS resources**, created out of band for this slice and later owned by the §18 repository: one SNS topic, one SQS queue per host with its queue policy and redrive policy, one subscription with a filter policy, and the `sqsh-results` DynamoDB table.
- **Testing**: LocalStack for the integration layer (§17), with the multi-agent isolation test — three agents, each asserting it receives only its own messages — as the assertion that validates this change's premise.
- **Spec drift risk**: `SPEC.md` describes the full v1. This slice implements a subset, so the two will disagree on what exists until later changes close the gap. `SPEC.md` remains the design reference; these specs describe shipped behavior.
