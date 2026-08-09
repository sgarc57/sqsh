# sqsh — SQS Command Agent

**Status:** Draft 5
**Last updated:** 2026-08-09
**Protocol version:** 1

Remote command execution over AWS SQS/SNS, with no inbound network access to managed
hosts.

---

## 1. Goals

Provide a lightweight daemon (`sqshd`) and CLI (`sqsh`) that:

- Execute a **fixed, allowlisted** set of operational commands on remote hosts.
- Require **outbound HTTPS only** — no inbound ports, no bastion, no SSH keys on
  operator machines.
- Authorize operators through **IAM**, not through host-local credentials.
- Survive reboots, network partitions, and duplicate message delivery without
  executing a command twice.
- Run as a `systemd` service with a bounded, auditable blast radius.

### Non-goals

| Not in scope | Rationale |
| --- | --- |
| Interactive shell / PTY | Defeats the allowlist; use SSM Session Manager. |
| SSH replacement | `sqsh` is for *known* operations, not exploration. |
| File transfer | Deferred; see §19. |
| Configuration management | `sqsh` triggers Puppet, it does not replace it. |
| Orchestration / dependency graphs | No cross-host sequencing in v1. |

### Explicit anti-goal

`sqsh` must never grow a code path that passes operator-supplied text to a shell.
Any feature that requires this is out of scope by definition.

---

## 2. Terminology

| Term | Meaning |
| --- | --- |
| **Operator** | Human or CI principal with IAM permission to publish commands. |
| **Agent** | One `sqshd` process on one host. |
| **Host ID** | Stable identifier for an agent (`host_id`). Not necessarily the OS hostname. |
| **Request** | A single command invocation, identified by `request_id`. |
| **Result** | The terminal outcome record for a request. |
| **Terminal command** | A command that kills the agent as a side effect (`reboot`, `shutdown`). |

---

## 3. Architecture

```
  ┌──────────┐
  │  sqsh  │  (operator workstation / CI)
  └────┬─────┘
       │ 1. GetItem  sqsh-inventory  (does host exist? is it alive?)
       │ 2. Publish  attrs{host=web01}
       ▼
  ┌─────────────────────────────┐
  │  SNS topic: sqsh-commands   │
  └──────────────┬──────────────┘
                 │ subscription filter policy: {"host": ["<claimed host_id>"]}
    ┌────────────┼────────────┐
    ▼            ▼            ▼
 sqsh-cmd-   sqsh-cmd-    sqsh-cmd-      ← one SQS queue per host, named after
 i-0abc123   i-0def456    i-0aaa111        instance_ref, not the friendly name (§6.2)
    │            │            │
    │ ReceiveMessage(WaitTimeSeconds=20)
    ▼            ▼            ▼
  sqshd         sqshd         sqshd
 host_id=     host_id=     host_id=
  web01        web02        db01          ← claimed at startup (§6.3)
    │
    │  Dispatcher (no shell)
    ├──────────┬──────────┬──────────┐
    ▼          ▼          ▼          ▼
 reboot   shutdown   systemctl    script
    │
    │ 3. spool to disk (fsync), then Claim / Complete
    ▼
  ┌─────────────────────────────┐
  │  DynamoDB: sqsh-results     │◄─── 4. sqsh polls GetItem(request_id)
  │            sqsh-inventory   │◄─── heartbeat + name claims
  └──────────────┬──────────────┘
                 │ Glue zero-ETL, 15–30 min (§7.5)
                 ▼
  ┌─────────────────────────────┐
  │  S3 Tables (Iceberg)        │◄─── Athena: audit, history, reporting
  └─────────────────────────────┘
```

### Why SNS fan-out instead of one shared queue

SQS has **no selective receive**. A design where every agent polls one shared queue
and calls `ChangeMessageVisibility(0)` on messages that aren't for it has three
defects:

1. Every non-matching receive increments `ApproximateReceiveCount`. With a redrive
   policy, a message can reach `maxReceiveCount` and land in the DLQ **before the
   target host ever polls it**. Disabling the redrive policy to avoid this removes
   the only backstop for genuinely poisonous messages.
2. Delivery latency is unbounded and grows with fleet size — there is no guarantee
   the right agent receives the message on any given round.
3. It is an O(N) thundering herd: N agents wake and do work per message.

SNS subscription filter policies push the routing decision server-side, so an agent
only ever sees messages addressed to it. `maxReceiveCount` then means what it should:
"this host tried and failed N times."

**Cost:** one queue + one subscription per host, created at provisioning time.

---

## 4. Threat model

### Trust boundaries

| Boundary | Enforced by |
| --- | --- |
| Who may issue commands | IAM policy on `sns:Publish` to the topic |
| Which host runs a command | SNS filter policy (server-side) + agent re-checks `target.host` |
| What may be run | Agent-local allowlist in `/etc/sqshd/config.yaml` |
| How long a command stays valid | `expires_at` + clock-skew rejection |
| Run-once | Local dedupe store + conditional DynamoDB write |

### In scope

- **Compromised host cannot command the fleet.** The agent's IAM role grants
  **no `sns:Publish`** on the command topic. This is the single most important IAM
  property in the design; a role that can both read commands and publish them turns
  one compromised host into fleet-wide RCE.
- **Compromised host cannot read other hosts' commands.** Agent SQS permissions are
  scoped to its own queue ARN.
- **Replay.** A captured message cannot be re-published without `sns:Publish`, and
  cannot be re-executed on the target because `request_id` is durably deduped.
- **Stale commands.** `expires_at` is mandatory. A `reboot` that was published during
  a network partition must not execute when connectivity returns two hours later.
- **Argument injection / path traversal.** No shell; per-command argument validators;
  script paths resolved under a rooted directory handle.

### Out of scope (accepted risk)

- **A malicious operator with `sns:Publish`.** They can do anything the allowlist
  permits, on any host. Mitigation is IAM scoping and the audit trail, not the agent.
- **Root-equivalence of the allowlist.** `systemctl restart <anything>` and
  `script <anything in the scripts dir>` are root-equivalent by construction. See §14.1.
- **Denial of service by an authorized operator.** Rate limiting is advisory (§14.4).

---

## 5. Source repository layout

Infrastructure lives in a **separate repository** and is specified in §18. This section
covers the Go source repository only; its CI holds no credentials that can mutate AWS.

```
cmd/
  sqshd/                 daemon entrypoint, flag parsing, signal wiring
  sqsh/                  CLI entrypoint

internal/
  agent/                 receive loop, worker pool, lifecycle, heartbeat
  protocol/              wire types, validation, canonical signing string, versioning
  dispatcher/            command registry, argument validation, execution
  commands/              one file per built-in command
    reboot.go
    shutdown.go
    systemctl.go
    puppet.go
    script.go
  awsx/                  SQS/SNS/DynamoDB/S3 clients behind narrow interfaces
  store/                 dedupe store, result spool (§8.3, §8.7)
  results/               ResultStore + InventoryStore implementations (§7.5)
  identity/              instance_ref resolution, name claiming (§6.3)
  config/                config load, defaults, validation
  auth/                  signature verification (reserved, see §7.4)
  output/                truncation, S3 offload
  observability/         structured logging, metrics
  version/               build metadata

deploy/
  sqshd.service          hardened systemd unit
  sqshd.sudoers.tmpl     generated from the allowlist (§14.1)
  config.example.yaml

packaging/               deb / rpm / container
```

`internal/awsx` is named to avoid colliding with the AWS SDK's own `aws` package.
Every AWS call site takes a narrow interface so tests need no live account.

`deploy/` holds host-level artifacts that version *with the binary* and ship inside the
package — not infrastructure. The sudoers template especially: §14.1 generates it from
the same allowlist the dispatcher enforces, so moving it to the infrastructure repo
would let the two drift into a privilege gap (§18.8).

---

## 6. Message routing

### 6.1 Publish

`sqsh` publishes to a single SNS topic with **raw message delivery enabled** on
every subscription, so the SQS body is the request JSON verbatim.

Required message attributes:

| Attribute | Type | Purpose |
| --- | --- | --- |
| `host` | String | Target host ID. Drives the filter policy. |
| `command` | String | Enables audit-only subscriptions and future per-command filters. |
| `protocol_version` | Number | Allows routing v2 traffic to v2-capable agents later. |

Subscription filter policy per host:

```json
{ "host": ["web01"] }
```

The agent **re-validates** `target.host == config.host_id` after parsing the body and
rejects mismatches with `invalid_target`. The filter policy is an optimization, not
the authorization boundary — a misconfigured subscription must not cause a command to
run on the wrong host.

### 6.2 Two identities per host

Resolving open question 1 in favour of claim-with-suffix (§6.3) creates a conflict: if
the operator-facing name is discovered at runtime, the queue name is not known at
provisioning time, and per-host IAM scoping (§14.3) has nothing stable to bind to.

Resolved by giving each host **two** identifiers:

| Identifier | Value | Stable? | Used for |
| --- | --- | --- | --- |
| `instance_ref` | IMDSv2 instance ID, else `/etc/machine-id` | Yes, for the life of the machine | Queue name `sqsh-cmd-<instance_ref>`, IAM resource conditions, claim ownership |
| `host_id` | Claimed friendly name, e.g. `web01-2` | Reassignable | SNS filter policy, `sqsh` arguments, dedupe namespace, result records |

The queue is named after the machine; the name is just a routing label held in the
subscription's filter policy. Two consequences, both good:

- IAM can still scope an agent to exactly one queue ARN, because `instance_ref` is
  known before the friendly name is claimed.
- **Name takeover cannot deliver a stale command to the wrong host.** A replacement
  host claiming `web01` gets its own fresh queue; messages addressed to the previous
  `web01` sit in that host's now-orphaned queue and expire. No `PurgeQueue` step and no
  window where an inherited `reboot` fires on new hardware.

### 6.3 Name claiming

On first start the agent claims a name; thereafter it reasserts the one it already
holds.

```
/var/lib/sqshd/identity.json   (mode 0600, fsynced)
{
  "host_id": "web01-2",
  "instance_ref": "i-0abc123def456",
  "claimed_at": "2026-07-31T10:00:00Z",
  "claim_generation": 1
}
```

**Reassert path.** If `identity.json` exists, the candidate is the stored `host_id`,
and the agent conditionally rewrites its inventory record requiring
`instance_ref = :me`. If the record is now held by a different `instance_ref`, the
agent **exits 78 and does not rename itself.** A running agent's `host_id` is its
dedupe namespace and its routing label; silently renaming would orphan in-flight
requests and split its own run-once history. This is an operator-visible conflict; it is
logged at `ERROR` naming the winning `instance_ref`, and §15.2 covers why the log and the
winner's metric — not a gauge on the exiting process — are the detectors that survive.

**Continuous reassertion.** The claim is not a startup-only check. Every heartbeat (§6.4)
is the *same* conditional write as the reassert path, carrying the same
`instance_ref = :me` condition, and a failed condition returns `ErrClaimLost` (§7.5).

Without this, the reassert path has no trigger in the case it exists for. A host
partitioned for longer than `stale_claim_ttl` loses its name to a takeover but never
restarts, so it never re-runs step 7. With an unconditional heartbeat it would write its
own `instance_ref`, `queue_url`, and `claim_generation` back over the new owner's record
the moment connectivity returned. Inventory would then name the old host while the SNS
filter policy pointed at the new one, and `sqsh hosts` would report a healthy `web01`
whose commands land on a machine the audit trail does not name — the exact class of
silent lie §15.2 ranks as the worst outcome in the system.

The stale-takeover branch below is what makes an unconditional heartbeat unsafe rather
than merely redundant, so the two must be specified together: **once a name can move,
every writer of that name is conditional.**

**Claim path.** Otherwise, `base = config.host_id` or the OS hostname, lowercased and
sanitised to `^[a-z0-9][a-z0-9-]{0,54}$`. For `n` in `0 .. max_claim_attempts` (default
20), the candidate is `base` when `n == 0` and `base-<n+1>` otherwise:

```
PutItem(sqsh-inventory, { host: candidate, instance_ref: me, ... })
  ConditionExpression:
        attribute_not_exists(host)
     OR instance_ref = :me
     OR last_seen < :stale_cutoff
```

First success wins; `ConditionalCheckFailedException` moves to the next candidate.
Exhausting all attempts exits 78 rather than picking an unbounded suffix.

Ordering invariant: `identity.json` is **fsynced before** the queue is created or the
subscription bound. A crash mid-registration then leaves a claimed name that the same
host re-adopts on restart, rather than an orphaned claim plus a fresh suffix on every
boot.

`instance_ref` is deliberately derived from machine identity rather than a generated
UUID. If `identity.json` is lost, the agent still recognises its own prior claim
through the `instance_ref = :me` branch and recovers its original name instead of
creeping to a new suffix.

**Stale takeover.** `stale_cutoff = now - stale_claim_ttl` (default 24h) lets a name be
recovered after decommission. Takeover increments `claim_generation`, logs at `WARN`,
and increments `sqshd_name_takeovers_total`. The agent is **not** granted
`sns:Unsubscribe` or `sqs:DeleteQueue`, so it cannot clean up the previous occupant's
subscription — that would hand a compromised host a fleet-wide DoS primitive. Orphaned
subscriptions are reaped by `sqsh release <host_id>` or a scheduled janitor. Until
then the previous occupant's queue receives copies of the name's traffic.

That is harmless only while the previous occupant is genuinely gone — nothing polls the
queue and the copies expire, showing up as DLQ-free queue depth. If the occupant is alive
and merely was unreachable, it polls those copies and **executes them**, because its own
subscription still matches the name. Takeover therefore does not evict the loser; only the
loser noticing does (§9.4), and until it notices, one name has two executors.

**Suffix creep** is the known weakness: reimaging a host produces a new
`instance_ref`, so `web01` becomes `web01-2` while the old claim remains for up to
`stale_claim_ttl`. Mitigations: stale takeover reclaims the base name once expired,
`--claim-name` forces a specific name (failing loudly if held), and
`sqsh hosts --stale` plus `sqsh release` make the cleanup explicit. Operators who
want names to track hardware should shorten `stale_claim_ttl`; those who want names
never to move should raise it.

### 6.4 Inventory (why it is MVP, not future work)

With server-side filtering, publishing to a host that has no subscription **succeeds
silently and goes nowhere**. That is an unacceptable CLI experience for `reboot`. The
inventory table doubles as the claim registry from §6.3.

```
Table: sqsh-inventory
  PK: host (S)                        ← the claimed host_id
  attrs: instance_ref, claim_generation, claimed_at, alias,
         agent_version, os, kernel, ips (SS), queue_url,
         boot_id, last_seen (N, epoch s), ttl (N)
```

Heartbeat every `heartbeat_interval` (default 60s) refreshes `last_seen`. It is a
conditional reassertion of the claim, not a blind field update — see §6.3. `sqsh`
does a `GetItem` before publishing: absent → exit 5; `last_seen` older than
`3 × heartbeat_interval` → warn and require `--force`.

`ttl` must be **longer than `stale_claim_ttl`**, otherwise a claim record disappears
before it is eligible for takeover and the name silently becomes free through
deletion rather than through the audited takeover path.

`alias` is populated from an instance tag and resolved by `sqsh`, so operators can
type `sqsh reboot web01` against a host whose claimed `host_id` is `web01-2`. An
ambiguous alias is an error listing the candidates, never a guess.

`boot_id` is `/proc/sys/kernel/random/boot_id`; a change confirms a reboot actually
happened, which is how `sqsh reboot --wait` reports success (§11.2).

### 6.5 Queue and subscription provisioning

Because `host_id` is claimed at runtime, the filter policy cannot be fully rendered
at provisioning time. Registration is therefore split:

**Provisioning-time (OpenTofu).** Queue `sqsh-cmd-<instance_ref>`, its redrive policy,
the queue policy allowing `sns.amazonaws.com` to `sqs:SendMessage` (conditioned on the
topic ARN), and the subscription *resource* — created with an unclaimable placeholder
filter policy. Practical for anything with a stable instance ID.

**Runtime (agent, after the claim succeeds).** `Subscribe` if no subscription exists
for the queue, then `SetSubscriptionAttributes` to set
`FilterPolicy = {"host": ["<claimed host_id>"]}` and `RawMessageDelivery = true`. This
is also the step that re-points the filter policy after a takeover.

The filter policy therefore has **two writers unless the boundary is declared
explicitly**, and the resulting failure is silent: an `apply` reverts the policy, the
host stays up and keeps heartbeating, and every command sent to it disappears. §18.1–18.2
resolve this by classifying the filter policy's *value* as agent-owned runtime data and
excluding it from infrastructure state. Read that before writing the module.

`self_register: true` additionally lets the agent `CreateQueue` and
`SetQueueAttributes` for its own `instance_ref`-named queue, for autoscaling groups
where no provisioning step runs. Scope that permission with a resource condition on
the instance-ID-derived queue name so a host cannot create or bind queues belonging to
others.

---

## 7. Wire protocol and storage

### 7.1 Request

```json
{
  "version": 1,
  "request_id": "018f3a2c-6c1e-7b3a-9f2d-4a1b6c8e0d55",
  "target": { "host": "web01" },
  "command": "systemctl",
  "args": ["restart", "sshd"],
  "issued_at": "2026-07-31T10:20:15Z",
  "not_before": null,
  "expires_at": "2026-07-31T10:25:15Z",
  "timeout_seconds": 30,
  "issuer": "arn:aws:sts::123456789012:assumed-role/oncall/sgarcia",
  "signature": null,
  "signature_alg": null
}
```

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `version` | int | yes | Unknown value → `unsupported_version`. |
| `request_id` | string | yes | UUIDv7. Time-sortable; the dedupe key. |
| `target.host` | string | yes | Must equal the agent's `host_id`. |
| `command` | string | yes | Must be in `allowed_commands` and registered. |
| `args` | []string | yes | May be empty. Never shell-interpreted. |
| `issued_at` | RFC 3339 UTC | yes | Rejected if more than `max_clock_skew` in the future. |
| `not_before` | RFC 3339 UTC \| null | no | Reject as `not_yet_valid` if `now < not_before`. |
| `expires_at` | RFC 3339 UTC | yes | Reject as `expired` if `now > expires_at`. |
| `timeout_seconds` | int | yes | `1 ≤ t ≤ max_command_timeout`, else `invalid_timeout`. |
| `issuer` | string | yes | Caller identity, for audit. Informational only — never used for authorization. |
| `signature` | string \| null | no | Reserved. See §7.4. |
| `signature_alg` | string \| null | no | Reserved. `"ed25519"` when enabled. |

`expires_at` is **required**, not optional. `sqsh` defaults it to
`issued_at + 5m` (`--expires-in` to override). An operator who wants a command to
survive a long outage must say so explicitly.

### 7.2 Result

Written to DynamoDB, not to a queue.

```json
{
  "request_id": "018f3a2c-...",
  "host": "web01",
  "status": "succeeded",
  "exit_code": 0,
  "stdout": "...",
  "stderr": "",
  "output_truncated": false,
  "output_uri": null,
  "rejection_reason": null,
  "error": null,
  "duration_ms": 452,
  "started_at": "2026-07-31T10:20:16Z",
  "completed_at": "2026-07-31T10:20:16.452Z",
  "attempt": 1,
  "agent_version": "0.1.0",
  "ttl": 1785500000
}
```

`status` enum:

| Status | Meaning | `sqsh` exit |
| --- | --- | --- |
| `running` | Claimed, in progress. Not terminal. | — (keeps polling) |
| `succeeded` | Ran, exit code 0. | 0 |
| `failed` | Ran, non-zero exit code. `exit_code` set. | 1 |
| `timed_out` | Exceeded `timeout_seconds`, process killed. | 1 |
| `accepted` | Terminal command dispatched; no exit code is observable. **Provisional** — see §7.2.1. | 0 |
| `rejected` | Never executed. `rejection_reason` set. | 3 |
| `expired` | `expires_at` passed before execution. | 3 |

`rejection_reason` enum: `unsupported_version`, `malformed`, `invalid_target`,
`unknown_command`, `command_not_allowed`, `arg_not_allowed`, `invalid_timeout`,
`not_yet_valid`, `expired`, `duplicate`, `bad_signature`, `concurrency_limit`.

Every rejection is a **normal, recorded outcome**: the message is deleted and a result
is written. Rejections must never be silent drops, and must never be retried — a
malformed message will be malformed forever.

#### 7.2.1 Status is monotonic

`accepted` is terminal for the *client* and provisional in the *store*. The distinction
exists because a terminal command's outcome is only knowable after the host comes back:
§8.2 writes `accepted` before invoking `systemctl reboot`, and §8.4's reconciliation
resolves it on the next boot by comparing `boot_id`. So exactly one result record legally
receives two terminal writes.

Left implicit, that produces two defects. §8.4 scans for entries "with no terminal
result", which is ambiguous precisely on the case its own first bullet is about — if
`accepted` counts as terminal the bullet never fires, and if it does not then the table
above is wrong. And because §8.7 replays spooled writes, an `accepted` record flushed
*after* reconciliation has already written `succeeded` would overwrite it, silently
downgrading a confirmed reboot back to "dispatched".

Both are fixed by one rule: **a result's status only ever moves forward, and the store
enforces it.**

```
                        ┌─────────┐
   Claim ──────────────▶│ running │
                        └────┬────┘
         ┌───────────────────┼───────────────┬──────────────┐
         ▼                   ▼               ▼              ▼
    succeeded             failed         timed_out       accepted
         │                   │               │              │   (terminal
         └───────────────────┴───────────────┘              │    commands)
                             │                              │
                             │             §8.4 reconciliation, keyed on
                             │             boot_id — the only writer that
                             │             may overwrite a terminal status
                             │                              │
                             │                              ▼
                             │                      succeeded | failed
                             │                              │
   rejected / expired ───────┤                              │
                             ▼                              ▼
        ══════════════════ FINAL ═══════════════════════════════
        any later Complete is dropped, counted, and never applied
```

- `accepted → succeeded | failed` is the **only** transition out of a terminal status, it
  is writable **only** by startup reconciliation (§8.4), and it is keyed on `boot_id`.
- Every other terminal status is final. A `Complete` that would move a result backwards —
  to `running`, to `accepted`, or from one final status to another — is **dropped, not
  applied**, and increments `sqshd_result_downgrades_prevented_total`.
- This is what makes §8.7's "replay is always safe" claim true rather than merely
  plausible. Idempotency by `request_id` alone is not enough when two legitimate terminal
  writes exist; ordering has to be enforced, because the spool cannot guarantee it.

For `shutdown`, `accepted` is final by construction: the agent does not come back, so
nothing ever runs the transition. Same for a `reboot` on a host that never returns —
`sqsh` reports that honestly via §11.2 step 4 rather than inventing an outcome.

### 7.3 Versioning rules

1. Agents **reject** unknown `version` values rather than best-effort parsing.
2. Agents **ignore** unknown fields at any level, so additive changes need no version
   bump.
3. Field meanings are never repurposed. Removing or retyping a field requires a bump.
4. `protocol_version` on the SNS attributes allows filter policies to route only
   understood versions during a fleet upgrade.

### 7.4 Reserved signature field

Signing is **not verified in v1** (`require_signature: false`), but the canonical
signing string is fixed now so Ed25519 can be enabled later with no protocol break.

```
sqsh-request-v1\n
<request_id>\n
<target.host>\n
<command>\n
<len(args) as decimal>\n
<len(args[0])>:<args[0]>\n
<len(args[1])>:<args[1]>\n
...
<issued_at>\n
<not_before or "-">\n
<expires_at>\n
<timeout_seconds>\n
```

Arguments are length-prefixed so no combination of argument values can produce two
different argument lists with the same signing string. Timestamps use RFC 3339 with
a `Z` suffix and second precision. `signature` is base64 (standard, padded) over
Ed25519 of this exact byte string.

When `require_signature: true`, the agent loads trusted public keys from
`/etc/sqshd/trusted_keys.d/*.pub` and rejects unsigned or badly signed requests with
`bad_signature`. Key distribution and rotation are deliberately deferred.

### 7.5 Storage backend

The results and inventory tables sit in the command path, so the backend choice is a
correctness question, not a preference. The workload is:

- Two point writes per request (`running` claim, then terminal result).
- One conditional write per request (`attribute_not_exists(request_id)`) as the
  cross-agent duplicate guard.
- Point reads by exact key, polled at 250 ms–2 s by `sqsh` while it waits.
- Per-item expiry (14-day retention).
- Heartbeat writes at `fleet_size / heartbeat_interval` — 500 hosts at 60 s is
  ~8 writes/s sustained.

That is an OLTP key-value workload. Evaluated against it:

| Backend | Control plane | Notes |
| --- | --- | --- |
| **DynamoDB** | ✅ Recommended | Conditional writes, native per-item TTL, single-digit-ms point reads, ~$1.25 per million writes. |
| **Plain S3 objects** | ✅ Viable | See below. Fewer services, since S3 is already a dependency for output offload. |
| **S3 Tables (Iceberg)** | ❌ Wrong tool | See below. Correct for the *audit warehouse*. |
| **Glue Data Catalog** | ❌ Category error | A metadata catalog describing where tables live. It is not a row store and has no per-item read/write path. |

**Why S3 Tables cannot back the command path.** Iceberg uses optimistic concurrency:
every writer must win an atomic metadata swap, so throughput is a few commits per
minute, not thousands per second — [Adobe measured a ceiling around 15 transactions per
minute](https://quesma.com/blog/apache-iceberg-practical-limitations-2025/). Eight
heartbeat writes per second is two orders of magnitude past that, and every result
write would contend with every other agent's. Each commit also writes new data files,
so granular writes produce the small-files problem; [S3 Tables' managed compaction has
been observed lagging 2.5–3
hours](https://www.onehouse.ai/blog/s3-managed-tables-unmanaged-costs-the-20x-surprise-with-aws-s3-tables).
There is no conditional single-row upsert to build the idempotency claim on, no
per-row TTL, and a point lookup requires a query engine — Athena at seconds-to-minutes
and a per-query charge, against a CLI that polls every 250 ms. Iceberg is a columnar
analytics format; this is a keyed OLTP path.

**Where S3 Tables is exactly right.** [AWS Glue supports zero-ETL integration from
DynamoDB to S3
Tables](https://aws.amazon.com/about-aws/whats-new/2025/07/aws-glue-zero-etl-integrations-amazon-dynamodb-applications-s3-tables),
replicating changes every 15–30 minutes with CDC and schema evolution, without
consuming DynamoDB RCUs or touching table availability. So:

```
sqshd ──▶ DynamoDB (control plane, ms latency, conditional writes, TTL)
              │
              │ Glue zero-ETL, 15–30 min, no RCU cost
              ▼
         S3 Tables / Iceberg (audit warehouse, unbounded retention)
              │
              ▼
         Athena / Spark  ← "who rebooted what last quarter", per-host MTTR,
                            command-failure trends, compliance export
```

This gives the Glue-and-S3-Tables state history you asked about while keeping Iceberg
out of the path where a 3-hour compaction lag or a metadata-swap conflict would stall a
reboot. Replication lag is irrelevant for audit and reporting. DynamoDB TTL then only
governs the *hot* copy — the warehouse keeps history indefinitely at S3 prices, which is
a better retention story than raising `dedupe_retention`.

**The plain-S3 variant**, if dropping DynamoDB entirely matters more than the
conveniences above:

- Results at `s3://<bucket>/results/<request_id>.json`; the claim is
  `PutObject` with [`If-None-Match: *`, supported since August
  2024](https://aws.amazon.com/about-aws/whats-new/2024/08/amazon-s3-conditional-writes),
  and the terminal write uses `If-Match: <etag>`. Reads are strongly consistent.
- Expiry via a lifecycle rule on the prefix (day granularity instead of per-item
  seconds).
- Inventory at `s3://<bucket>/inventory/<host_id>.json`. `sqsh hosts` gets freshness
  from `ListObjectsV2`, whose `LastModified` per key makes a heartbeat GET unnecessary;
  only `--verbose` fetches the bodies.
- Costs: S3 `PUT` at ~$5/million is roughly 4× a 1 KB DynamoDB write, so heartbeats
  dominate — around $110/month for 500 hosts at 60 s versus ~$27 on DynamoDB. Lengthen
  `heartbeat_interval` to close the gap.
- Loses: per-item TTL precision, atomic partial updates, and query-by-attribute.

Both backends sit behind one interface, so this stays a deployment choice:

```go
type ResultStore interface {
    Claim(ctx context.Context, r Result) error   // must fail with ErrAlreadyClaimed
    // Complete enforces the §7.2.1 status ordering: a write that would move the
    // stored result backwards is dropped and reported, not applied. Only
    // accepted → succeeded|failed may overwrite an existing terminal status.
    Complete(ctx context.Context, r Result) error
    Get(ctx context.Context, requestID string) (Result, error)
}
type InventoryStore interface {
    ClaimName(ctx context.Context, rec HostRecord, staleCutoff time.Time) error
    // Heartbeat conditions on instance_ref = rec.InstanceRef and must fail with
    // ErrClaimLost rather than overwriting a record another instance now holds (§6.3).
    Heartbeat(ctx context.Context, rec HostRecord) error
    Get(ctx context.Context, hostID string) (HostRecord, error)
    List(ctx context.Context) ([]HostRecord, error)
}
```

Both implementations must map a failed condition — `ConditionalCheckFailedException` for
DynamoDB, `412 PreconditionFailed` for S3 — onto three sentinel errors:
`ResultStore.Claim` → `ErrAlreadyClaimed`, `InventoryStore.ClaimName` →
`ErrNameHeld`, and `InventoryStore.Heartbeat` → `ErrClaimLost`. The first two are
ordinary recorded outcomes (a `duplicate` rejection and the next suffix attempt
respectively); `ErrClaimLost` is fatal to the process (§9.4). Collapsing them into one
error would let a lost claim be retried as though it were a duplicate request.

The S3 variant pays for `ErrClaimLost`: an unconditional heartbeat is one `PutObject`,
while a conditional one needs the current ETag, so it is either a `HEAD` plus a
`PutObject` with `If-Match` or a cached ETag that a concurrent writer can invalidate.
That roughly doubles the request count on the write that already dominates the S3
variant's cost (see the ~$110/month figure above), which is a further argument for
treating `store: s3` as deferred (§21.5).

---

## 8. Execution semantics

This is the section the previous draft got wrong, and it matters most.

### 8.1 Delivery guarantee per command

SQS is at-least-once. Blindly retrying is unsafe: the previous draft's rule ("if
execution fails, don't delete the message; let the visibility timeout expire") would
**reboot-loop a host**. The message is still in flight when the box goes down, so the
agent receives it again on boot and reboots again, forever.

Each command declares its own semantics:

```go
type Semantics int

const (
    AtLeastOnce Semantics = iota // safe to retry: idempotent, read-only, or converging
    AtMostOnce                   // never retry automatically
)
```

| Command | Semantics | Why |
| --- | --- | --- |
| `reboot` | `AtMostOnce` | Terminal; retry means a boot loop. |
| `shutdown` | `AtMostOnce` | Terminal. |
| `systemctl` (`status`, `is-active`, `show`) | `AtLeastOnce` | Read-only. |
| `systemctl` (`restart`, `reload`) | `AtMostOnce` | Extra restarts are visible outages. |
| `puppet` | `AtMostOnce` | Long-running and converging; overlapping runs conflict. |
| `script` | Declared per script in its manifest; default `AtMostOnce`. | Unknown side effects. |

### 8.2 Message handling state machine

```
Receive
  │
  ├─ parse / validate ──── fail ──▶ write result(rejected) ──▶ DeleteMessage ──▶ done
  │
  ├─ dedupe store lookup ── hit ──▶ re-publish stored result ──▶ DeleteMessage ──▶ done
  │
  ├─ claim: conditional PutItem(status=running, attribute_not_exists(request_id))
  │     └─ condition failed ──▶ write result(rejected: duplicate) ─▶ Delete ─▶ done
  │
  ├─ acquire execution slot (worker pool + per-command lock)
  │
  ├─ AtMostOnce?
  │     yes ──▶ record dedupe(request_id, dispatched) [fsync]
  │         └──▶ terminal command? ──▶ write result(accepted) ──▶ DeleteMessage
  │                                                       ──▶ then exec
  │         └──▶ non-terminal ──────▶ DeleteMessage ──▶ exec ──▶ update result
  │     no  ──▶ exec (visibility heartbeat running) ──▶ update result ──▶ DeleteMessage
  │
  └─ done
```

Two invariants:

- **For `AtMostOnce`, the dedupe record is fsynced and the message deleted *before*
  the process is spawned.** Crash-during-execution is reported as `failed` by a
  reconciliation pass (§8.4), never re-executed.
- **For terminal commands, the `accepted` result is written and the message deleted
  before `systemctl reboot` is invoked.** Otherwise there is no result at all and the
  message survives the reboot.

### 8.3 Dedupe store

`/var/lib/sqshd/dedupe.db` (BoltDB or SQLite — single-writer, crash-safe, no server).

```
key:   request_id
value: { first_seen, status, result_digest, attempt }
```

- Written with an fsync before any `AtMostOnce` execution.
- Entries pruned after `dedupe_retention` (default 14d), which must exceed the
  maximum possible message lifetime (SQS `MessageRetentionPeriod`, default 4d).
- The store is the **authoritative** run-once mechanism; it works with DynamoDB
  unreachable. The conditional DynamoDB write is a secondary guard that catches the
  cross-agent case (two hosts sharing a `host_id`) and should fire an alert.
- Losing `/var/lib/sqshd` is equivalent to losing run-once. The directory must be on
  persistent storage, and the systemd unit uses `StateDirectory=sqshd`.

### 8.4 Startup reconciliation

On startup the agent scans the dedupe store for entries in `dispatched` state whose
result is not yet **final** in the §7.2.1 sense — that is, missing, `running`, or
`accepted`. `accepted` is explicitly in scope; it is the provisional status this pass
exists to resolve.

- Terminal command whose `boot_id` differs from the recorded one → `succeeded`
  (the reboot worked).
- Terminal command whose `boot_id` is unchanged → `failed`, `error: "reboot dispatched
  but host did not restart"`. The agent restarted without the machine doing so, and
  since `--delay` is not supported (§11.1) there is no pending action to wait for.
- Anything else → `failed`, `error: "agent restarted during execution"`.

This is what makes `AtMostOnce` safe: a crashed command becomes a reported failure
instead of a silent retry. It is also the only writer permitted to overwrite an
`accepted` record (§7.2.1), which is why it must run before the spool flusher — see
§9.1.

### 8.5 Visibility timeout

`visibility_timeout` is a **derived** value, not an independent knob:

```
visibility_timeout ≥ max_command_timeout + 30s
```

Config validation fails at startup if this does not hold. While an `AtLeastOnce`
command runs, the agent calls `ChangeMessageVisibility` every
`visibility_timeout / 3`, capped at `max_command_timeout + 60s` total, so a wedged
process cannot hold a message hostage forever.

### 8.6 Retries and the DLQ

Automatic redelivery now only ever reaches the target host, so `maxReceiveCount` is
meaningful:

- `maxReceiveCount: 3`, DLQ `sqsh-cmd-<host>-dlq`.
- Only `AtLeastOnce` commands can legitimately be redelivered.
- Alarm on `ApproximateNumberOfMessagesVisible > 0` on any DLQ.
- Retrying an `AtMostOnce` command is an **operator decision**:
  `sqsh retry <request_id>` publishes a *new* `request_id`. The agent never does
  this on its own.

### 8.7 Degraded mode: result store unavailable

Resolving open question 4: **the agent executes anyway and spools the result to disk**,
retrying the remote write with exponential backoff. The reasoning is that the command
most likely to be issued to a host with broken AWS connectivity is precisely the one
that fixes it — `script recover_network.sh`, or a reboot. An agent that refuses to act
until it can record the act is useless in the outage it exists for.

The cost is honest and bounded: availability of execution is preserved, and durability
of the *audit record* degrades from "remote, immediately" to "local, eventually."

**Write path.** The result is fsynced to `<state_dir>/spool/<request_id>.json` **before**
the remote write is attempted, and the spool entry is removed only after the remote
write is acknowledged. The spool is therefore the source of truth for "did this happen,"
and it survives a crash between execution and reporting.

**Retry.** Exponential backoff with full jitter, 1 s → 15 min cap, retried
indefinitely. A dedicated flusher goroutine drains the spool oldest-first, independent
of the receive loop, so a stuck write never blocks command execution.

Replaying a spooled write is safe because `Complete` is keyed by `request_id` **and**
enforces the §7.2.1 status ordering. Keying alone is not sufficient, and the reboot path
is the counterexample: an `accepted` record spooled before the reboot, then flushed after
startup reconciliation has already written `succeeded`, would overwrite a confirmed reboot
with "dispatched". The ordering rule drops that write; §9.1's step 8-before-step-14
ordering keeps the common case from relying on it.

**Claim degradation.** The conditional remote claim from §8.2 is skipped when the store
is unreachable — a conditional write against an unreachable store is not an option, so
this is forced rather than chosen. The local dedupe store (§8.3) is already the
authoritative run-once mechanism, so what is lost is the cross-agent duplicate detector.

The `host_id`-collision case that detector guards is **not** excluded by construction, and
this is the sharpest residual risk in degraded mode. An agent that cannot reach the store
is precisely an agent whose `last_seen` is going stale, and therefore an agent whose name
is a takeover candidate (§6.3). The two degradations compound: the window in which two
agents share a name is also the window in which neither can run the check that catches it.

The bound is `stale_claim_ttl`, not anything the agent does — a partition shorter than
`stale_claim_ttl` cannot open the window at all. That gives the knob a floor it did not
have before: **`stale_claim_ttl` should exceed the longest partition worth surviving.**
See §21.3, where this now constrains the tuning question. Every skipped claim increments
`sqshd_claims_skipped_total`.

**Backpressure.** An unbounded spool eventually fills the disk, and silently discarding
audit records is worse than refusing work. Graduated response on
`result_spool_max` (default 10 000 entries or 256 MiB):

| Spool fullness | Behaviour |
| --- | --- |
| < 50% | Normal. |
| ≥ 50% | `WARN` per accepted command. |
| ≥ 90% | Reject routine commands with `store_unavailable`. **Always** admit `reboot`, `shutdown`, and `script` — the break-glass set, which is the entire point of degraded mode. |
| = 100% | Drop the oldest spool entry to admit a new one; `ERROR` plus `sqshd_results_dropped_total`. |

Dropping is loud and metered, never silent. `sqshd_results_dropped_total > 0` should page:
it means an execution happened that will never appear in the audit trail.

**Startup.** The spool is *opened and enumerated* early (§9.1 step 5) so that a failure to
reach AWS at step 6 cannot discard it, but it is not **drained** until step 14 — after
reconciliation. The drain is best-effort, bounded by `spool_flush_timeout` (default 30 s),
and completes or times out before the first `ReceiveMessage`, so a reboot's record lands
as soon as the network is back without delaying the agent from becoming useful.

Enumerate-early / drain-late is the ordering that matters: draining at step 5 would replay
a reboot's spooled `accepted` *before* step 8 could upgrade it to `succeeded` — the common
case on every reboot, not an edge case.

**Known gap.** `sqsh --wait` polls the same store the agent cannot reach, so it cannot
distinguish "still running" from "ran, but the result is stuck in a spool." It times out
at `--wait-timeout` and exits 4 with wording that says exactly that, rather than
implying the command failed or was cancelled. Operators diagnosing an unreachable host
should read the journal directly; `sqshd --dump-spool` prints pending results as JSON for
that case.

---

## 9. Daemon lifecycle

### 9.1 Startup

```
1. Parse flags, load /etc/sqshd/config.yaml
2. Validate config          → exit 78 (EX_CONFIG) on error, no partial start
3. Resolve instance_ref     → IMDSv2 instance ID, else /etc/machine-id → exit 78
4. Open dedupe store        → exit on failure; never run without run-once
5. Open result spool        → enumerate pending writes; do NOT drain yet (§8.7)
6. Verify AWS identity      → sts:GetCallerIdentity, log the ARN
7. Resolve host_id          → reassert or claim a name (§6.3) → exit 78 on conflict
8. Reconcile in-flight      → §8.4; needs host_id and boot_id from step 7
9. Resolve queue URL        → sqsh-cmd-<instance_ref>; create if self_register (§6.5)
10. Bind subscription       → Subscribe if absent, set FilterPolicy to host_id (§6.5)
11. Validate scripts dir    → ownership and mode (§11.5); log and disable on failure
12. Write inventory heartbeat  → conditional on instance_ref = :me (§6.3)
13. sd_notify(READY=1)
14. Start metrics listener, heartbeat ticker, spool flusher, receive loop
       └─ the flusher's first pass is the bounded startup drain (spool_flush_timeout),
          and it completes or times out before the first ReceiveMessage (§8.7)
```

Ordering constraints worth preserving in the implementation:

- **7 before 8:** reconciliation keys on `host_id`, and its reboot-succeeded test
  compares the recorded `boot_id` against the current one.
- **7 before 10:** the filter policy *is* the claimed name, so a name change must
  re-point the subscription before any message is received.
- **5 before 6:** spool replay is queued whether or not AWS is currently reachable; a
  failure at step 6 must not discard it.
- **8 before 14:** the spool is *queued* at step 5 but only *drained* at step 14, after
  reconciliation has run. A reboot's `accepted` record is typically still spooled at
  boot, and reconciliation is the writer that upgrades it to `succeeded` (§8.4). Draining
  first would replay `accepted` after `succeeded` and downgrade it. §7.2.1's ordering rule
  makes that harmless if the two ever interleave anyway; this ordering makes the
  interleave rare rather than routine.

Steps 2–11 are fail-fast: an agent that cannot guarantee its own invariants must not
poll. `Restart=on-failure` with backoff handles transient AWS unavailability. Step 7 is
the deliberate exception to retrying — a name conflict is a permanent, operator-visible
condition, so it exits 78 and must stay stopped.

`Restart=on-failure` alone does **not** give that: systemd restarts on any non-zero exit,
so an exit-78 agent would restart every `RestartSec` and re-detect the same conflict
forever. The unit therefore sets `RestartPreventExitStatus=78` (§14.2). Without that
directive every exit-78 path in this document — config error, unresolvable
`instance_ref`, exhausted claim attempts, name conflict, claim loss (§9.4) — degrades
into a restart loop that looks like a crash loop.

### 9.2 Receive loop

```go
for ctx.Err() == nil {
    slots := pool.Available()          // 0 → block until a worker frees
    msgs := sqs.ReceiveMessage(ctx, MaxNumberOfMessages: min(slots, 10),
                                    WaitTimeSeconds: 20,
                                    VisibilityTimeout: cfg.VisibilityTimeout,
                                    AttributeNames: ["ApproximateReceiveCount"])
    for _, m := range msgs {
        pool.Go(func() { handle(ctx, m) })
    }
}
```

The agent never requests more messages than it has capacity to start, so it does not
sit on messages whose visibility timer is already running. `ReceiveMessage` errors
back off exponentially (1s → 60s, full jitter) and are counted, not fatal.

### 9.3 Graceful shutdown

On `SIGTERM`/`SIGINT`:

1. Stop receiving.
2. Cancel nothing yet; wait up to `shutdown_grace` (default 45s) for in-flight
   commands.
3. For anything still running: cancel the context, `SIGTERM` the child, `SIGKILL`
   after 5s, write `failed`.
4. For `AtLeastOnce` messages not yet started, `ChangeMessageVisibility(0)` so a
   restarted agent picks them up immediately instead of waiting out the timeout.
5. Flush the dedupe store, close, exit 0.

The systemd unit sets `TimeoutStopSec=60` (> `shutdown_grace` + kill margin) so
systemd does not `SIGKILL` mid-flush.

`reboot`/`shutdown` invoked by the agent will cause systemd to stop `sqshd` as part of
the shutdown transaction. Because the `accepted` result and the `DeleteMessage` both
happen *before* the command is invoked, this is a clean exit, not a lost message.

### 9.4 Claim loss at runtime

A heartbeat that returns `ErrClaimLost` (§6.3, §7.5) means another `instance_ref` now
holds this agent's `host_id`. This agent's own subscription still carries
`FilterPolicy = {"host": ["<host_id>"]}` — it was never granted `sns:Unsubscribe`, by
design — so it is still receiving and executing a copy of every command addressed to a
name it no longer owns. It must stop, and stopping is the only lever it has.

1. Log at `ERROR` with the winning `instance_ref`; set `sqshd_name_conflict{phase="runtime"}`.
2. Stop receiving.
3. Do **not** `ChangeMessageVisibility(0)` on unstarted messages, as §9.3 step 4 would.
   SNS delivered a copy of each to the new owner's queue as well, so these are duplicates
   rather than orphaned work; returning them early only accelerates this agent's own next
   spurious receive. Leave them to the visibility timeout and `expires_at`.
4. Drain in-flight commands under `shutdown_grace` and write their results, as §9.3
   steps 2–3. Work already dispatched is real and must be recorded even though the name
   has moved.
5. Flush the dedupe store and the spool, then exit 78 — the conflict is permanent until
   an operator intervenes, and restarting would only re-detect it at step 7.

`sqsh release <host_id>` then reaps the orphaned subscription. Until it runs, the
stopped agent's queue accumulates copies that expire unread, exactly as §6.3's
decommissioned-host case describes.

**Detection window.** Between the takeover and the losing agent's next heartbeat — at most
`heartbeat_interval` — two live agents match one name, and SNS delivers every command
published to it to both:

```
   takeover                          loser's next heartbeat
      │                                        │
      ├────────── ≤ heartbeat_interval ────────┤
      │        two executors, one name         │
      ▼                                        ▼
  publish reboot web01                    ErrClaimLost → exit 78
      │
      ├──▶ new owner  ──▶ Claim(request_id) ──▶ wins  ──▶ executes
      └──▶ old owner  ──▶ Claim(request_id) ──▶ fails ──▶ rejected: duplicate
                              (race — either may win)
```

The conditional claim of §8.2 bounds the damage to *one* execution, not to the *right*
execution: whichever agent writes `attribute_not_exists(request_id)` first runs the
command, so a command issued in this window may execute on either machine.
`sqshd_rejections_total{reason="duplicate"}` is the signal that it happened, which is why
§15.2 treats that counter as an audit trigger rather than an impossibility.

Shortening `heartbeat_interval` shortens the window. `stale_claim_ttl` governs whether it
ever opens at all, and §8.7 explains why that is the knob that matters.

---

## 10. Command dispatcher

```go
type Command interface {
    Name() string
    Semantics() Semantics
    // Validate rejects disallowed arguments before anything is executed.
    // It must be pure: no filesystem or network access beyond the allowlist config.
    Validate(cfg *config.Config, args []string) error
    // Execute runs the command. Implementations must use exec.CommandContext
    // with an absolute argv[0] and must never invoke a shell.
    Execute(ctx context.Context, args []string) Result
}
```

Rules for every implementation:

- Validate first, execute second. `Validate` runs on the full argument list before any
  process is spawned.
- **Allowlist by construction, not by denylist.** Never scan for `../` or `;` and
  accept what's left. Accept only values that exactly match a known-good set.
- Absolute paths for `argv[0]` (`/usr/bin/systemctl`, not `systemctl`) — the agent must
  not depend on `PATH`.
- Empty environment plus an explicit set (`PATH=/usr/sbin:/usr/bin:/sbin:/bin`,
  `LANG=C`, `HOME`), so output parsing is locale-stable.
- `Stdin` is `/dev/null`.
- Output captured to bounded buffers (§12).
- Exactly one command per file in `internal/commands`, with table-driven tests
  covering the rejection corpus.

---

## 11. Built-in commands

### 11.1 `reboot` / `shutdown`

| | |
| --- | --- |
| Args | none; any argument → `arg_not_allowed` |
| Runs | `/usr/bin/systemctl reboot` / `/usr/bin/systemctl poweroff` |
| Semantics | `AtMostOnce`, terminal |
| Result | `accepted`, written *before* invocation. For `reboot`, upgraded to `succeeded`/`failed` by reconciliation on the next boot (§7.2.1, §8.4); for `shutdown`, final. |

`--delay` is deliberately not supported: it would leave a pending action the agent
cannot cancel or report on.

### 11.2 Reboot confirmation

Because no exit code is observable, `sqsh reboot web01 --wait` confirms out of band:

1. Read the current `boot_id` from `sqsh-inventory`.
2. Publish, wait for `status: accepted`.
3. Poll inventory until `boot_id` changes and `last_seen` is fresh → success.
4. Timeout (`--wait-timeout`, default 10m) → exit 4 with "host has not reported back".

Inventory rather than the result record, because it is the faster and more direct signal:
`last_seen` proves the agent is serving again, whereas the result upgrade depends on
reconciliation and on the spool draining. The two converge on the same answer — §8.4 keys
its upgrade on the same `boot_id` comparison — and that convergence is the point. It is
what lets the results table answer "did this reboot complete?" on its own, so the audit
warehouse (§7.5) does not have to reconstruct the outcome by joining against a
current-state inventory table that only ever holds the latest `boot_id`.

### 11.3 `systemctl`

Verb and unit are validated **independently against separate allowlists**. The
previous draft's example list mixed the two (`reload puppet` shown as allowed while
`puppet` was shown as rejected), which is not expressible as a flat list of strings.

```yaml
commands:
  systemctl:
    verbs: [status, is-active, is-enabled, show, restart, reload, start, stop]
    units:
      - nginx.service
      - sshd.service
      - docker.service
      - puppet.service
```

Validation:

1. `len(args) == 2`, else `arg_not_allowed`.
2. `args[0] ∈ verbs`.
3. `args[1] ∈ units` — **exact match after normalization**. Normalization appends
   `.service` if there is no known unit suffix, then requires exact set membership.
4. Reject anything containing `/`, `..`, `@`, `%`, NUL, or leading `-`.
5. `--` and any other flag is rejected; flags are not part of the grammar.

Note that `.service` and `.timer` are distinct units and each must be listed
explicitly. Templated units (`foo@bar.service`) are not supported in v1.

| Verb class | Semantics |
| --- | --- |
| `status`, `is-active`, `is-enabled`, `show` | `AtLeastOnce` |
| `restart`, `reload`, `start`, `stop` | `AtMostOnce` |

`systemctl status` exits 3 for a stopped unit. That is **`succeeded` with
`exit_code: 3`**, not `failed`: the query worked. Each command declares which exit
codes count as success.

### 11.4 `puppet`

| | |
| --- | --- |
| Args | none (v1) |
| Runs | `/opt/puppetlabs/bin/puppet agent -t` |
| Semantics | `AtMostOnce` |
| Success codes | `0` (no changes) and `2` (changes applied) |
| Failure codes | `1`, `4`, `6` |

Fixed argv; `--environment`, `--noop`, and friends are not operator-controllable in v1
because they change what converges. Guarded by a lock so two runs cannot overlap; a
second request while one is running is rejected `concurrency_limit`.

### 11.5 `script`

Runs a pre-approved script from `scripts_dir` (default `/usr/lib/sqshd/scripts`).

Validation, in order:

1. `args[0]` matches `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` — so no `/`, no `..`, no
   leading dot, no NUL. This alone rejects traversal; the checks below defend against
   the filesystem changing under us.
2. `args[0]` is a key in the `scripts` config map (an allowlist of names, so dropping
   a file into the directory is not sufficient to make it runnable).
3. Open via `os.Root` rooted at `scripts_dir` (Go 1.24+; this project builds on
   Go 1.26), which resolves the path inside the root and refuses symlinks that escape
   it. Do not `filepath.Join` + `strings.HasPrefix`.
4. `fstat` the handle: regular file, owner `root` (uid 0), and
   `mode & 0o022 == 0` (not group- or world-writable). Otherwise
   `arg_not_allowed` — a script anyone can edit is an arbitrary-execution primitive.
5. Extra arguments only if the manifest declares an `args` pattern; default is none.

```yaml
commands:
  script:
    dir: /usr/lib/sqshd/scripts
    scripts:
      rotate_logs.sh:      { semantics: at_least_once, timeout: 120 }
      recover_network.sh:  { semantics: at_most_once,  timeout: 60  }
```

`scripts_dir` ownership and mode are validated once at startup (§9.1 step 8) and
re-checked per invocation via the open file handle, closing the TOCTOU window.

---

## 12. Output handling

DynamoDB items are capped at 400 KB and command output is unbounded, so truncation is
a protocol concern, not an implementation detail.

- `stdout` and `stderr` are each captured into a ring buffer of
  `output_inline_limit` (default 32 KiB). On overflow, **keep the tail** — the error is
  usually at the end.
- If either stream overflowed, set `output_truncated: true`.
- If `output_s3_bucket` is configured, the full streams are uploaded to
  `s3://<bucket>/<host>/<request_id>/{stdout,stderr}` and `output_uri` is set.
  `sqsh --full` fetches from there.
- Hard cap per stream on disk/S3: `output_max_bytes` (default 8 MiB). Beyond that the
  process is killed and the result is `failed`,
  `error: "output limit exceeded"` — a runaway command must not fill the disk.
- Output is captured as raw bytes; invalid UTF-8 is escaped when written to DynamoDB.

---

## 13. Configuration

`/etc/sqshd/config.yaml`, mode `0640`, owner `root:sqshd`. Unknown keys are a **startup
error** (typo'd `alowed_commands` must not silently allow nothing).

```yaml
# Identity (§6.2, §6.3)
host_id: web01                    # base name to CLAIM; suffixed on collision.
                                  # default: OS hostname. Not the queue name.
max_claim_attempts: 20            # web01 .. web01-20, then exit 78
stale_claim_ttl: 24h              # a name held by a host silent this long is takeable

# AWS
region: eu-central-1
topic_arn: arn:aws:sns:eu-central-1:123456789012:sqsh-commands
queue_url: ""                     # default: derived as sqsh-cmd-<instance_ref>
self_register: false              # allow CreateQueue for our own instance_ref
output_s3_bucket: ""              # empty disables S3 offload

# Result / inventory store (§7.5)
store: dynamodb                   # dynamodb | s3
results_table: sqsh-results       # store=dynamodb
inventory_table: sqsh-inventory   # store=dynamodb
store_s3_bucket: ""               # store=s3; required in that mode

# Polling and concurrency
wait_time_seconds: 20             # SQS long-poll wait; max 20
visibility_timeout: 90s           # must be >= max_command_timeout + 30s
max_concurrent_commands: 4
heartbeat_interval: 60s           # also bounds the §9.4 claim-loss detection window

# Timeouts
default_command_timeout: 30s
max_command_timeout: 60s
max_clock_skew: 300s
shutdown_grace: 45s

# Durability (§8.3, §8.7)
state_dir: /var/lib/sqshd
dedupe_retention: 336h            # 14d; must exceed SQS MessageRetentionPeriod
result_spool_max_entries: 10000
result_spool_max_bytes: 256MiB
spool_flush_timeout: 30s          # startup replay budget; does not delay polling

# Output
output_inline_limit: 32KiB
output_max_bytes: 8MiB

# Security
require_signature: false
trusted_keys_dir: /etc/sqshd/trusted_keys.d
allowed_commands: [reboot, shutdown, systemctl, puppet, script]

commands:
  systemctl:
    verbs: [status, is-active, show, restart, reload]
    units: [nginx.service, sshd.service, docker.service, puppet.service]
  script:
    dir: /usr/lib/sqshd/scripts
    scripts:
      rotate_logs.sh:     { semantics: at_least_once, timeout: 120 }
      recover_network.sh: { semantics: at_most_once,  timeout: 60 }

log_level: info
metrics_addr: 127.0.0.1:9787      # empty disables
```

Renames from the previous draft, and why:

| Was | Now | Reason |
| --- | --- | --- |
| `poll_interval: 20s` | `wait_time_seconds: 20` | It is SQS `WaitTimeSeconds`, not a sleep between polls. Naming it an interval invites someone to add a `time.Sleep`. |
| `max_workers: 2` / `workers = 4` | `max_concurrent_commands: 4` | The two sections contradicted each other. |
| `response_queue_url` | *(removed)* | Results go to the result store (§7.5). |
| `host_id` (authoritative) | `host_id` (a *requested base name*) | The effective name is claimed at runtime and may carry a suffix; the authoritative value lives in `identity.json` (§6.3). |

Config validation rules enforced at startup:

- `visibility_timeout ≥ max_command_timeout + 30s`
- `wait_time_seconds ≤ 20`
- `stale_claim_ttl < inventory ttl` — otherwise a claim record expires before it becomes
  takeable and names free themselves by deletion rather than by audited takeover (§6.4)
- `dedupe_retention > SQS MessageRetentionPeriod`
- `store: s3` requires `store_s3_bucket`; `store: dynamodb` requires both table names
- `self_register: true` requires `topic_arn`
- every key in `allowed_commands` is registered in the dispatcher, and every command
  needing per-command config has it
- `state_dir` is writable and on persistent storage

**Reload:** `SIGHUP` re-reads the allowlist, log level, and spool thresholds only.
Identity, queue, topic, and store settings require a restart — `host_id` especially,
since changing it mid-flight would abandon a claim and a dedupe namespace. A config that
fails validation on reload is logged and **discarded**; the running config stays in
effect.

---

## 14. Security

### 14.1 Privilege model — stated honestly

A daemon authorized to run `systemctl restart <unit>` and execute root-owned scripts
**is root-equivalent**. Running it as an unprivileged user with sudo rules does not
change that. Do not let the unix user create a false sense of containment: the real
boundaries are the IAM policy on `sns:Publish` and the agent-local allowlist.

Given that, two supported deployments:

**(A) Root + systemd hardening (recommended).** Simpler, no sudo parsing surface, and
the hardening directives are a genuine constraint on a *buggy* dispatcher.

**(B) Unprivileged `sqshd` user + narrowly scoped sudo.** Choose this if policy
requires it. Rules must name exact commands with exact arguments and `NOPASSWD`:

```
sqshd ALL=(root) NOPASSWD: /usr/bin/systemctl reboot, \
                          /usr/bin/systemctl poweroff, \
                          /usr/bin/systemctl restart nginx.service, \
                          /usr/bin/systemctl reload nginx.service, \
                          /usr/bin/systemctl restart sshd.service
```

Never `/usr/bin/systemctl *`, and never a wildcard argument — sudo wildcards are
routinely bypassable. The sudoers file must be generated from the same allowlist
config to prevent drift.

### 14.2 systemd unit

```ini
[Unit]
Description=sqsh command agent
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
ExecStart=/usr/local/bin/sqshd --config /etc/sqshd/config.yaml
Restart=on-failure
RestartPreventExitStatus=78
RestartSec=5s
TimeoutStopSec=60
WatchdogSec=120

StateDirectory=sqshd
StateDirectoryMode=0700
ConfigurationDirectory=sqshd

NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/var/lib/sqshd
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
CapabilityBoundingSet=CAP_SETUID CAP_SETGID
IPAddressDeny=any
IPAddressAllow=localhost

[Install]
WantedBy=multi-user.target
```

`Type=notify` + `WatchdogSec` means a wedged receive loop gets restarted rather than
sitting silently. `RestartPreventExitStatus=78` is load-bearing, not tidiness: every
fail-fast path in §9.1 and the claim-loss path in §9.4 exit 78 expecting to *stay*
stopped, and `Restart=on-failure` restarts on every non-zero status without it (§9.1).
`IPAddressAllow` must be widened to the AWS endpoint ranges (or the directives dropped)
if that filtering is used; VPC endpoints make this practical.

### 14.3 IAM

**Operator** (`sqsh-operator`):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow", "Action": "sns:Publish",
      "Resource": "arn:aws:sns:eu-central-1:123456789012:sqsh-commands" },
    { "Effect": "Allow", "Action": ["dynamodb:GetItem", "dynamodb:Query", "dynamodb:Scan"],
      "Resource": [
        "arn:aws:dynamodb:eu-central-1:123456789012:table/sqsh-results",
        "arn:aws:dynamodb:eu-central-1:123456789012:table/sqsh-inventory"
      ] }
  ]
}
```

`dynamodb:Scan` on inventory is what backs `sqsh hosts`. Keep it out of any role that
does not need fleet enumeration.

`sqsh release` (§6.3 orphan cleanup) needs `sns:Unsubscribe`, `sqs:DeleteQueue`, and
`dynamodb:DeleteItem` on inventory. Put these in a **separate, more privileged**
`sqsh-admin` role rather than the day-to-day operator role — they are the fleet-DoS
actions deliberately withheld from agents, and they should not be one typo away during
an incident.

**CI roles** (`sqsh-tofu-{env}`) are specified in §18.6. The property to preserve is that
the pipeline which *builds* the command path is denied `sns:Publish` and
`sqs:SendMessage`, and is constrained by an `iam:PermissionsBoundary` so it cannot mint a
role that escapes that deny.

Restrict destructive commands with a condition on the SNS message attribute:

```json
{ "Effect": "Deny", "Action": "sns:Publish",
  "Resource": "arn:aws:sns:eu-central-1:123456789012:sqsh-commands",
  "Condition": { "StringEquals": { "sns:MessageAttributes.command": ["reboot", "shutdown"] } } }
```

This is how a read-only on-call role is expressed without a second topic. Note it
relies on the publisher setting the attribute honestly — the attribute is a *routing*
and *policy* input, and the agent still authorizes from the message body, so an
operator who lies about the attribute gets their command dropped by the deny, not
escalated.

**Agent** (`sqsh-agent`, one role, scoped per host by ARN). Note every resource
condition binds to `instance_ref`, never to the claimed `host_id` — that is the whole
reason for the split in §6.2, since the friendly name is not known until runtime and can
be reassigned:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Sid": "OwnQueueOnly",
      "Effect": "Allow",
      "Action": ["sqs:ReceiveMessage", "sqs:DeleteMessage",
                 "sqs:ChangeMessageVisibility", "sqs:GetQueueAttributes",
                 "sqs:GetQueueUrl"],
      "Resource": "arn:aws:sqs:eu-central-1:123456789012:sqsh-cmd-${ec2:InstanceId}" },

    { "Sid": "BindOwnSubscription",
      "Effect": "Allow",
      "Action": ["sns:Subscribe", "sns:SetSubscriptionAttributes",
                 "sns:ListSubscriptionsByTopic", "sns:GetSubscriptionAttributes"],
      "Resource": "arn:aws:sns:eu-central-1:123456789012:sqsh-commands",
      "Condition": {
        "StringEquals": { "sns:Endpoint":
          "arn:aws:sqs:eu-central-1:123456789012:sqsh-cmd-${ec2:InstanceId}" } } },

    { "Sid": "Results",
      "Effect": "Allow",
      "Action": ["dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:GetItem"],
      "Resource": "arn:aws:dynamodb:eu-central-1:123456789012:table/sqsh-results" },

    { "Sid": "InventoryAndClaim",
      "Effect": "Allow",
      "Action": ["dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:GetItem"],
      "Resource": "arn:aws:dynamodb:eu-central-1:123456789012:table/sqsh-inventory" },

    { "Sid": "OutputOffload",
      "Effect": "Allow", "Action": "s3:PutObject",
      "Resource": "arn:aws:s3:::sqsh-output/${ec2:InstanceId}/*" }
  ]
}
```

The `sns:Endpoint` condition is what keeps runtime subscription binding (§6.5) from
becoming an escalation: the agent can create and re-point a subscription only when the
delivery endpoint is its own queue, so it cannot subscribe itself to another host's
traffic, and cannot re-point another host's subscription at itself.

Denied to agents, deliberately:

| Action | Why |
| --- | --- |
| `sns:Publish` | A host that can both read and publish commands turns one compromise into fleet-wide RCE. The single most important line in this policy. |
| `sqs:SendMessage` | Same reason, one hop lower. |
| `sns:Unsubscribe`, `sqs:DeleteQueue` | Fleet-wide DoS primitive. Orphan cleanup after a name takeover is an operator action (`sqsh release`) or a janitor's, never an agent's (§6.3). |
| `dynamodb:DeleteItem` | Results and claim records are append-and-expire; an agent must not be able to erase its own audit trail. |
| `dynamodb:Scan`, `Query` on inventory | An agent has no reason to enumerate the fleet. |

`self_register: true` additionally needs `sqs:CreateQueue` and `sqs:SetQueueAttributes`
on `sqsh-cmd-${ec2:InstanceId}` only.

For `store: s3` (§7.5), replace the two DynamoDB statements with `s3:GetObject` and
`s3:PutObject` on `results/*` and `inventory/${ec2:InstanceId}.json`. Conditional-write
enforcement can be pinned bucket-side so a missing `If-None-Match` is rejected by the
bucket policy rather than trusted to the client.

Credentials come from the instance profile via IMDSv2 (`http_tokens: required`) or
IRSA — never a static key in the config file.

### 14.4 Rate limiting and audit

- Per-agent token bucket (`max_commands_per_minute`, default 30). Excess is rejected
  `concurrency_limit` rather than queued, so a publish storm cannot be replayed
  slowly onto a host.
- Every accepted and rejected request is logged with `issuer`, `request_id`,
  `command`, `args`, and outcome. CloudTrail records the `sns:Publish` side; the two
  together are the audit trail.
- SSE-KMS on the topic, all queues, the tables, and the output bucket. The KMS key
  policy must allow `sns.amazonaws.com` to encrypt and the agent roles to decrypt,
  otherwise fan-out fails silently at the SNS→SQS hop.

---

## 15. Observability

### 15.1 Logging

Structured JSON to stdout (`log/slog`), captured by the journal.

```json
{
  "time": "2026-07-31T10:20:16.452Z",
  "level": "INFO",
  "msg": "command completed",
  "host": "web01",
  "request_id": "018f3a2c-...",
  "issuer": "arn:aws:sts::123456789012:assumed-role/oncall/sgarcia",
  "command": "systemctl",
  "args": ["restart", "sshd"],
  "status": "succeeded",
  "exit_code": 0,
  "duration_ms": 452,
  "attempt": 1
}
```

Every log line carries `request_id`, so a single grep spans validation through result
write. Command output is never logged at `info` (it can be megabytes and may contain
secrets) — only at `debug`, truncated.

### 15.2 Metrics

Prometheus on `metrics_addr` (localhost only by default):

| Metric | Type | Labels |
| --- | --- | --- |
| `sqshd_messages_received_total` | counter | — |
| `sqshd_commands_total` | counter | `command`, `status` |
| `sqshd_rejections_total` | counter | `reason` |
| `sqshd_command_duration_seconds` | histogram | `command` |
| `sqshd_commands_in_flight` | gauge | — |
| `sqshd_visibility_extensions_total` | counter | `command` |
| `sqshd_aws_errors_total` | counter | `api` |
| `sqshd_last_successful_poll_timestamp` | gauge | — |
| `sqshd_dedupe_hits_total` | counter | — |
| `sqshd_build_info` | gauge | `version`, `commit`, `host_id`, `instance_ref` |
| `sqshd_pending_results` | gauge | — |
| `sqshd_spool_oldest_age_seconds` | gauge | — |
| `sqshd_spool_bytes` | gauge | — |
| `sqshd_result_write_failures_total` | counter | `op` |
| `sqshd_results_dropped_total` | counter | — |
| `sqshd_result_downgrades_prevented_total` | counter | `from`, `to` |
| `sqshd_claims_skipped_total` | counter | — |
| `sqshd_name_takeovers_total` | counter | — |
| `sqshd_name_conflict` | gauge | `held_by`, `phase` (`startup` \| `runtime`) |

Alerts, in rough order of how badly they mean the system is lying to you:

| Condition | Meaning |
| --- | --- |
| `sqshd_results_dropped_total > 0` | Page. A command executed and will never appear in the audit trail (§8.7). |
| `time() - sqshd_last_successful_poll_timestamp > 300` | A host is silently not polling. This is the failure mode that makes the whole system untrustworthy — commands appear accepted and never run. |
| `sqshd_name_takeovers_total` increased | A name moved between machines. Benign at decommission; with the previous holder still alive it is the §9.4 window, and the commands issued in it should be audited. |
| `sqshd_name_conflict == 1` | Two machines claim one `host_id`; the loser has exited 78 and is not polling (§6.3, §9.4). Best-effort — see below. |
| DLQ depth `> 0` | Repeated failure on a retryable command, or a poison message. |
| `sqshd_spool_oldest_age_seconds > 3600` | Degraded mode has persisted for an hour; audit records are accumulating locally. |
| `sqshd_rejections_total{reason="duplicate"} > 0` | Cross-agent duplicate. Three causes, all worth investigating: the §9.4 takeover window, a restored-from-snapshot host reusing an `instance_ref`, or a bug. |

**Do not build the only name-conflict alert on `sqshd_name_conflict`.** It is exported by a
process that is on its way out, so a scrape may never see it — and a *startup* conflict
exits at §9.1 step 7, before the metrics listener starts at step 14, so at that phase the
gauge is never served at all. It is a hint for the runtime case and nothing more.

The detectors that survive are: the winner's `sqshd_name_takeovers_total`, incremented by a
process that keeps running; the `ERROR` line in the loser's journal, which is durable
because it is not a metric; and the loser's *absence* — `up == 0`, or a stalled
`sqshd_last_successful_poll_timestamp`. A name conflict is fundamentally observed as one
host going quiet while another announces it took a name, and the alerting should be shaped
that way rather than around a gauge the conflicted agent must survive long enough to serve.

The duplicate row is worth stating plainly: **restoring a host from a snapshot clones both
`instance_ref` and `identity.json`**, producing two agents that agree they are the same
host and share a dedupe namespace and a queue. Snapshot-based provisioning must clear
`/var/lib/sqshd/identity.json`; the metric above is the detector when it does not. Unlike
the §9.4 window, this one has no self-limiting mechanism — both clones reassert the same
`instance_ref` successfully forever, so neither ever sees `ErrClaimLost`.

---

## 16. CLI

```
sqsh reboot   <host>
sqsh shutdown <host>
sqsh puppet   <host>
sqsh systemctl <host> restart sshd
sqsh script   <host> recover_network.sh

sqsh hosts [--stale]             # list inventory with freshness and claim info
sqsh status <request_id>         # fetch a stored result
sqsh retry  <request_id>         # re-publish as a NEW request_id
sqsh release <host_id>           # sqsh-admin only: drop a dead host's claim,
                                   #   subscription, and queue (§6.3)
```

`sqsh hosts` shows `host_id`, `alias`, `instance_ref`, `agent_version`, and age of
`last_seen`; `--stale` filters to entries past `3 × heartbeat_interval`, which is the
cleanup worklist for `release`. Host arguments accept either a claimed `host_id` or an
`alias`; an ambiguous alias is an error listing the candidates.

Daemon-side flags relevant to the above: `sqshd --claim-name <name>` forces a specific
name and fails loudly if it is held, and `sqshd --dump-spool` prints pending results as
JSON for diagnosing a host that cannot reach the store (§8.7).

Global flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--wait` / `--no-wait` | `--wait` | Poll for the result vs. print `request_id` and exit 0. |
| `--wait-timeout` | 2m (10m for `reboot`) | Give up polling. Command may still run. |
| `--timeout` | server default | `timeout_seconds` in the request. |
| `--expires-in` | 5m | Sets `expires_at`. |
| `--force` | off | Proceed on a stale inventory entry. |
| `--yes` | off | Skip the confirmation prompt for `reboot`/`shutdown`. |
| `--json` | off | Emit the raw result record. |
| `--full` | off | Fetch untruncated output from S3. |

`sqsh` polls `GetItem` with backoff (250ms → 2s). On `--wait-timeout` it prints the
`request_id` and the exact `sqsh status` command to resume, and exits 4. Timing out
is **not** cancellation: nothing in this protocol can recall an in-flight command, and
the CLI must say so rather than implying the command was stopped.

Exit codes: `0` success · `1` remote command failed · `2` usage error · `3` rejected ·
`4` timed out waiting · `5` host unknown or stale · `6` local/AWS error.

One consequence of §7.2.1 is operator-visible: `sqsh status` on a `reboot` returns `0`
against the provisional `accepted` record and may return `1` once the host comes back and
reconciliation resolves it to `failed`. That is the record improving, not flapping —
statuses only move forward — but `--json` output should be read as "best known so far" for
a terminal command until the host has reported back.

---

## 17. Testing

| Layer | Approach |
| --- | --- |
| Validators | Table-driven, per command. Required rejection corpus: `../../evil`, `..%2f`, `nginx.service\x00`, `nginx.service;reboot`, `-nginx`, `--all`, `nginx@1.service`, absolute paths, symlinks out of `scripts_dir`, 64 KiB argument, empty args, 3 args. |
| Protocol | Round-trip; unknown fields preserved-ignored; unknown version rejected; canonical signing string golden-file test (locks the format before signing ships). |
| Execution semantics | Fakes for SQS/DynamoDB. Assert **ordering**: for `AtMostOnce`, dedupe fsync and `DeleteMessage` both happen before `exec`. This is the test that prevents the reboot loop from regressing. |
| Duplicate delivery | Deliver the same `request_id` twice, concurrently and sequentially; assert exactly one execution and two identical results. |
| Crash recovery | Kill `-9` mid-command, restart, assert reconciliation marks it `failed` and it is never re-run. |
| Status monotonicity | Drive every §7.2.1 transition and assert forward writes apply and backward ones are dropped and counted. The load-bearing case: spool an `accepted` for a reboot, let reconciliation write `succeeded`, *then* flush the spool, and assert the record still reads `succeeded`. Assert `accepted → succeeded` is rejected from any writer other than reconciliation. |
| Visibility | Command outlives one `visibility_timeout`; assert extension happens and no second delivery. |
| Clock skew | `issued_at` in the future, `expires_at` in the past, `not_before` in the future. |
| Name claiming | N agents claim the same base name concurrently; assert distinct names, no gaps, and that each fsyncs `identity.json` before binding a subscription. Restart each and assert it reasserts the same name. Delete `identity.json` and assert it recovers its name via `instance_ref = :me` rather than creeping to a suffix. |
| Claim conflict | Point two `instance_ref`s at one `identity.json`; assert the loser exits 78 and never polls. Snapshot-clone case (§15.2) included. |
| Stale takeover | Age `last_seen` past `stale_claim_ttl`; assert takeover bumps `claim_generation`, re-points the filter policy, and that a message addressed to the *previous* occupant is never delivered to the new one. |
| Degraded mode | Fault-inject store unavailability. Assert: execution proceeds; the result is fsynced to the spool before the remote attempt; the flusher drains it on recovery; replay is idempotent; the ≥90% threshold rejects routine commands but still admits `reboot`/`shutdown`/`script`. |
| Store conformance | One shared suite run against both `dynamodb` and `s3` implementations, plus an in-memory fake. The load-bearing assertions are that a second `Claim` for the same `request_id` returns `ErrAlreadyClaimed`, that a conditional `Heartbeat` under a foreign `instance_ref` returns `ErrClaimLost`, and that `Complete` refuses a backward status write — all mapped from `ConditionalCheckFailedException` and `412` respectively. |
| Integration | LocalStack for SNS/SQS/DynamoDB/S3; a container fleet of 3 agents asserting each receives only its own messages, with two sharing a base hostname to exercise suffixing end to end. |
| Safety | `execute: false` dry-run mode logs the exact `argv` without spawning, so `reboot` is testable on a dev box. |
| Naming contract | The test that makes §18.4 safe: stand up the platform module in LocalStack, boot a real agent with only `region` and `env` configured, and assert it derives and reaches every resource. This is what catches a `NAMING.md` drift between the two repositories. |
| Infrastructure drift | After `tofu apply`, have an agent claim a name and bind its filter policy, then assert `tofu plan -detailed-exitcode` returns 0. A non-zero exit means §18.2's `ignore_changes` is missing or misplaced, which is the silent routing break in §18.1. |
| Unbound subscription | Apply the `host` module without starting an agent; assert a command published for any host is **not** delivered to the placeholder subscription. |
| Policy | `tofu validate`, `tflint`, and `trivy config` in CI. Add an explicit assertion that no `sqsh-tofu-*` policy grants `sns:Publish` or `sqs:SendMessage` (§18.6) — a security property is worth a test, not just a sentence in a spec. |

---

## 18. Infrastructure as code

AWS resources are provisioned with **OpenTofu** (Terraform-compatible; see §18.7) from a
repository separate from the Go source and release tooling.

### 18.1 Resource ownership tiers

The split is by *lifecycle owner*, not just by directory. Getting this wrong produces a
specific and nasty failure: if OpenTofu manages a subscription's `filter_policy` while
the agent re-points it at runtime (§6.5), the next `apply` silently reverts routing. The
host stays up, keeps heartbeating, passes every `sqsh hosts` check — and every command
sent to it vanishes. Nothing in §15.2 catches that, because the agent never sees the
message it was supposed to receive.

| Tier | Owner | Resources | State |
| --- | --- | --- | --- |
| **0 Bootstrap** | OpenTofu, run once | State bucket, state KMS key, CI OIDC provider + roles | Local, then self-migrated |
| **1 Platform** | OpenTofu | SNS topic, `sqsh-results`, `sqsh-inventory`, output bucket, S3 Tables bucket + Glue zero-ETL, KMS keys, IAM roles/policies, CloudWatch alarms | Remote, per env+region |
| **2 Host** | OpenTofu *or* agent — never both | `sqsh-cmd-<instance_ref>` queue, its DLQ, redrive policy, queue policy, the subscription *resource* | Remote, or unmanaged |
| **3 Runtime** | Agent only | Inventory records, result items, **the `filter_policy` value**, `identity.json` | Never in state |

The load-bearing classification is the last one: **a filter policy's value is tier-3
runtime data, not tier-2 infrastructure.** It is the claimed `host_id`, decided at
runtime by §6.3, so OpenTofu cannot know it and must not assert it.

### 18.2 The subscription placeholder

Tier 2 therefore creates the subscription but declares its filter policy
agent-owned:

```hcl
resource "aws_sns_topic_subscription" "host" {
  topic_arn            = var.topic_arn
  protocol             = "sqs"
  endpoint             = aws_sqs_queue.host.arn
  raw_message_delivery = true

  # Placeholder only. The agent overwrites this with its claimed host_id
  # once the name claim in §6.3 succeeds.
  filter_policy = jsonencode({ host = ["__unclaimed__"] })

  lifecycle {
    ignore_changes = [filter_policy]
  }
}
```

`__unclaimed__` is deliberately unclaimable: §6.3 sanitises names to
`^[a-z0-9][a-z0-9-]{0,54}$`, which admits no underscore. So a subscription that OpenTofu
has created but no agent has yet bound matches **nothing**, rather than defaulting to
some live host's traffic. Fail-closed rather than fail-to-someone-else.

That also gives a free operational check: a subscription still reading `__unclaimed__`
some minutes after the instance came up means registration failed. Alarm on it — it is
the one registration failure the agent cannot self-report, because an agent that never
bound its subscription is also an agent that never receives a command to report on.

`self_register: true` (cattle/ASG hosts) moves all of tier 2 to the agent. In that mode
**do not `import` the agent-created queue into state.** Two owners for one resource
reintroduces exactly the drift this section exists to prevent.

### 18.3 Repository layout

```
sqsh/                                  ← Go source; no AWS credentials in CI
  cmd/ internal/                       (§5)
  deploy/
    sqshd.service                       packaged with the binary, versions with it
    sqshd.sudoers.tmpl                  generated from the allowlist (§14.1)
    config.example.yaml
  packaging/                           deb / rpm / container

sqsh-infrastructure/                   ← OpenTofu only; holds the AWS credentials
  bootstrap/                           tier 0, local state, run once per account
  modules/
    platform/                          tier 1
    host/                              tier 2, for long-lived instances
    observability/                     alarms, dashboards, DLQ monitors
    analytics/                         S3 Tables bucket + Glue zero-ETL (§7.5)
  envs/
    dev/eu-central-1/
    prod/eu-central-1/
    prod/us-east-1/
  policy/                              trivy / checkov rules, tflint config
  NAMING.md                            the §18.4 contract, versioned
```

One root module per `(env, region)` with its own state, rather than workspaces.
Workspaces share a backend and select the target through mutable shell state; the
failure mode is applying a prod change to dev or the reverse. For infrastructure whose
blast radius includes rebooting a fleet, the target belongs in the file path where it
shows up in a PR diff and cannot be fat-fingered.

If you would rather keep one repository, this tree drops in as a top-level
`infrastructure/` directory unchanged — only §18.6's pipeline split and module
`source` paths (relative vs. git ref) differ. The ownership tiers and the naming
contract are what actually matter, and they hold either way.

### 18.4 The inter-repo contract: convention, not outputs

Splitting the repos creates a coupling risk — the agent needs to know its topic ARN and
table names, and the obvious fix (pipe OpenTofu outputs into host configs) rebuilds the
dependency the split was meant to remove.

Instead, **every AWS name the agent needs is derivable on the host**:

| Value | Source | Cost |
| --- | --- | --- |
| Region | IMDSv2 | already fetched |
| Account ID | `sts:GetCallerIdentity` | already called, §9.1 step 6 |
| Environment | config, or an instance tag | — |
| Topic ARN | `arn:aws:sns:{region}:{account}:sqsh-commands-{env}` | derived |
| Tables | `sqsh-results-{env}`, `sqsh-inventory-{env}` | derived |
| Own queue | `sqsh-cmd-{instance_ref}` | derived, §6.2 |

So **no OpenTofu output ever needs to reach a host.** The infrastructure repo can be
destroyed and reapplied, or its state lost and rebuilt by import, and running agents
keep working as long as the names hold. That is the property worth protecting, and it is
why the naming convention — not a set of outputs — is the contract.

Consequences to accept:

- `NAMING.md` is a versioned interface document present in both repos, with a
  `naming_version` variable in the platform module. Changing a name is a breaking change
  requiring a coordinated rollout, exactly as a wire-format change would be (§7.3).
- Renaming for convenience is not available. This is the intended trade.

**Escape hatch.** For anything genuinely non-conventional (a shared topic in another
account, a migration in progress), the platform module writes a JSON discovery document
to SSM Parameter Store at `/sqsh/{env}/discovery`, and the agent reads it only when
`discovery_param` is set — adding `ssm:GetParameter` on that path to agent IAM. Keep
this the exception; the convention path has no extra dependency and no extra permission.

### 18.5 State backend

```hcl
terraform {
  required_version = ">= 1.9.0"

  backend "s3" {
    bucket       = "sqsh-tfstate-123456789012"
    key          = "prod/eu-central-1/terraform.tfstate"
    region       = "eu-central-1"
    encrypt      = true
    kms_key_id   = "alias/sqsh-tfstate"
    use_lockfile = true          # S3-native locking; no DynamoDB lock table
  }
}
```

[`use_lockfile` arrived in Terraform 1.10 and uses S3 conditional writes for the
lock](https://developer.hashicorp.com/terraform/language/backend/s3) — the same
`If-None-Match` primitive §7.5 uses for the result claim. It is the forward-compatible
choice for both tools: [`dynamodb_table` is deprecated in Terraform from 1.11 and slated
for removal](https://www.bschaatsbergen.com/s3-native-state-locking), while
[OpenTofu supports both mechanisms with no deprecation
planned](https://opentofu.org/docs/language/settings/backends/s3/). One fewer resource
and a smaller IAM surface for state access.

Bucket configuration: versioning on (state rollback), `BlockPublicAcls`, SSE-KMS with a
dedicated key, and a lifecycle rule expiring noncurrent versions after 90 days.

**Bootstrap chicken-and-egg:** `bootstrap/` runs with local state to create the bucket
and key, then migrates its own state into the bucket it just made. Its state is
deliberately low-value — a bucket, a key, and the OIDC roles — so losing it costs an
`import`, not a rebuild. Never commit state files; `.gitignore` must cover
`*.tfstate*`, `.terraform/`, and `*.tfplan`.

### 18.6 CI/CD and the credential split

The point of separating the repositories is that **only one of them holds AWS
credentials**, and neither holds enough to command the fleet.

| Repo | CI does | AWS access |
| --- | --- | --- |
| `sqsh` | `go test`, `go build`, lint, build deb/rpm/container, publish release artifacts | None, or write-only to an artifact bucket |
| `sqsh-infrastructure` | `tofu validate`, `tflint`, `trivy config`, `tofu plan` on PR, `tofu apply` on merge | GitHub OIDC → `sqsh-tofu-{env}` role, no static keys |

Three constraints that are security properties, not conveniences:

1. **`sqsh-tofu-*` is explicitly denied `sns:Publish` and `sqs:SendMessage`.** Otherwise
   a compromised infrastructure pipeline is fleet-wide RCE — the same reason agents are
   denied it in §14.3. The pipeline that *creates* the command path must not be able to
   *use* it.
2. **The plan role is read-only and distinct from the apply role.** PR plans run against
   untrusted branch contents; that job must not be able to mutate anything.
3. **The apply role carries an `iam:PermissionsBoundary` condition.** A role that can
   create IAM roles is otherwise transitively administrator, which would quietly make
   the deny in (1) meaningless — it could simply create a role that does have
   `sns:Publish` and assume it.

Prod applies additionally require environment protection with a human approver. Drift
detection runs `tofu plan -detailed-exitcode` on a schedule and alerts on exit 2;
because §18.1 keeps tier-3 data out of state and §18.2 ignores the filter policy, a
clean fleet should plan clean.

### 18.7 OpenTofu or Terraform

Either works — the HCL here is compatible with both. Recommendation is **OpenTofu**, for
one reason specific to this project: [state encryption, native since OpenTofu
1.7](https://opentofu.org/docs/language/state/encryption/), which [Terraform's open
binary still does not
provide](https://spacelift.io/blog/opentofu-vs-terraform). `sqsh` state is a complete
fleet topology — every queue ARN, every `instance_ref`, every IAM policy document. That
is a target-enumeration document, and `encrypt = true` on the S3 backend protects it at
rest in S3 while leaving it plaintext in any local copy, plan file, or CI artifact.

To stay usable from both tools, keep the OpenTofu-only `encryption` block in a
`encryption.tofu` file. OpenTofu reads `.tofu` files and Terraform ignores them, so the
same tree stays valid under either binary — worth verifying against the OpenTofu version
you pin, since the mechanism dates to 1.8. Otherwise: pin `required_version`, pin
provider versions, and avoid tool-specific functions.

### 18.8 What infrastructure must never manage

- **Inventory records and result items.** OpenTofu owns the tables, never their contents.
  Seeding an inventory record would fabricate a host that no agent has claimed, and
  §6.3's claim protocol would then conflict with it on first boot.
- **`identity.json`.** Host-local state, owned by the agent (§6.3).
- **The systemd unit, sudoers, and config template.** These version with the binary and
  ship in the package. The sudoers file in particular is *generated from the allowlist*
  (§14.1), which is code-repo data — splitting it into the infrastructure repo would let
  the two drift into a privilege gap.
- **Anything that runs a command.** No `null_resource`/`local-exec` wrappers that publish
  to the topic. Operations go through `sqsh`, where they are authorized, expiring,
  deduped, and audited. An `apply` that reboots hosts is neither.

---

## 19. Future extensions

Additive under the §7.3 rules — no version bump needed.

| Extension | Sketch |
| --- | --- |
| File distribution | Request carries `artifact: {uri, sha256, dest, mode}`. Agent downloads from S3, verifies the digest, stages atomically via temp file + rename, then executes. Never trusts the URI's content without the digest. |
| Streaming output | Agent appends chunks to `s3://.../<request_id>/stdout` or a per-request DynamoDB item; `sqsh --follow` tails. Preferred over a status queue: no ordering problems. |
| Fleet targeting | Publish with `group` / `role` / `env` attributes; subscription filter policies use `$or` across `host` and `group`. Server-side filtering means fleet targeting costs one publish, not N. Requires a fan-out result view in `sqsh`. |
| Scheduling | `not_before` already exists in the wire format. Long delays need care: SQS retains messages for at most 14 days, so anything longer needs EventBridge Scheduler to publish at the right time rather than a message sitting in a queue. |
| Richer health | Extend the inventory record already required by §6.4. |
| Fleet reporting | Query the S3 Tables mirror (§7.5) from Athena: reboot history by host, command failure rates, per-host MTTR, compliance exports. No new agent code — the zero-ETL pipeline already carries it. |
| Claim janitor | Scheduled Lambda holding the `sqsh-admin` rights, releasing inventory claims whose `instance_ref` no longer resolves to a running instance. Automates the `sqsh release` worklist for autoscaling fleets. |
| Plugin commands | External executables in an approved dir with a manifest declaring name, argument schema, semantics, timeout, and success exit codes. Prefer this over Go plugins, which are a versioning trap. |

---

## 20. Design decisions and rejected alternatives

| Decision | Chosen | Rejected | Why |
| --- | --- | --- | --- |
| Routing | SNS + per-host queue with filter policy | Shared queue + `ChangeMessageVisibility(0)` on non-match | Spurious receives count toward `maxReceiveCount`, so messages DLQ before the target sees them; unbounded latency; O(N) wakeups. |
| Results | DynamoDB keyed by `request_id` | Shared response queue | SQS has no selective receive: two concurrent `sqsh` users consume each other's responses. DynamoDB also gives the audit log, the idempotency claim, and survives a Ctrl-C'd CLI. |
| Results | DynamoDB | Ephemeral per-invocation response queue | Leaks queues on CLI crash; needs a reaper; no history. |
| Auth | IAM now, `signature` reserved and unverified | Ed25519 in v1 | Key distribution and rotation are a project of their own. Fixing the canonical signing string (§7.4) keeps the door open at zero protocol cost. |
| Auth | IAM now | KMS signing | Per-message KMS calls add latency and cost for a threat IAM already covers. |
| Retry | Per-command `AtMostOnce` / `AtLeastOnce` | "On failure, don't delete the message" | That rule reboot-loops a host: the message is in flight when the box goes down. |
| Run-once | Local fsynced store, authoritative | DynamoDB conditional write alone | Must hold with DynamoDB unreachable. DynamoDB is kept as the cross-agent duplicate detector. |
| Host identity | Claim-with-suffix against inventory, persisted in `identity.json` | Instance ID as the operator-facing name | `sqsh reboot i-0abc123def456` is hostile during an incident. |
| Host identity | Split `instance_ref` (queue, IAM) from `host_id` (routing, operator) | One identifier for both | A runtime-claimed name cannot anchor an IAM resource condition, and reusing a name would otherwise let a stale command reach replacement hardware. The split makes both problems disappear (§6.2). |
| Name conflict | Loser exits 78 | Silently re-claim a new suffix | `host_id` is the dedupe namespace and the routing label; renaming a running agent orphans its in-flight requests and splits its run-once history. |
| Result lifecycle | `accepted` is provisional; status is monotonic and `Complete` enforces it (§7.2.1) | `accepted` is simply terminal, or blind last-write-wins | Terminal commands legitimately produce two terminal writes — `accepted` before the reboot, `succeeded` after it. Declaring `accepted` final loses reboot confirmation from the audit trail; blind overwrite lets a spool replay downgrade a confirmed reboot back to "dispatched" (§8.7). |
| Claim reassertion | Every heartbeat is conditional on `instance_ref = :me`; `ErrClaimLost` is fatal (§9.4) | Unconditional `last_seen` refresh | Startup-only checking has no trigger for the case it exists for: a partitioned host never restarts, so it would clobber the new owner's claim on reconnect, leaving inventory naming one host and the filter policy pointing at another (§6.3). |
| Conflict alerting | Winner's counter + loser's absence + journal `ERROR` | Gauge on the conflicted agent | The conflicted process is exiting, and at startup it exits before the metrics listener binds. An alert that requires the failing process to serve a scrape is not an alert (§15.2). |
| Post-takeover eviction | Loser detects and stops itself | Takeover re-points or removes the loser's subscription | Agents hold no `sns:Unsubscribe` (that is a fleet-wide DoS primitive), so a takeover cannot evict anyone. Self-eviction is the only mechanism available, which is why the heartbeat must be the thing that notices (§9.4). |
| Orphan cleanup | Operator/janitor via `sqsh-admin` | Grant agents `sns:Unsubscribe` | Hands every host a fleet-wide DoS primitive. |
| Store unavailable | Execute, spool to disk, retry with backoff | Refuse to execute | The command most likely aimed at a host with broken connectivity is the one that repairs it. Refusing makes the tool useless in the outage it exists for. |
| Spool overflow | Reject routine commands at 90%, always admit break-glass, drop oldest loudly at 100% | Unbounded spool, or blanket refusal | Unbounded fills the disk; blanket refusal locks out recovery commands. |
| Control-plane store | DynamoDB (or plain S3), with S3 Tables as a downstream mirror | S3 Tables / Iceberg in the command path | Iceberg's optimistic concurrency tops out at commits per minute against a workload needing ~8 writes/s plus conditional claims; no per-row TTL; point reads need Athena at seconds-to-minutes. Details and sources in §7.5. |
| Control-plane store | DynamoDB | Glue Data Catalog | A metadata catalog, not a row store. No per-item read/write path exists to build on. |
| Filter policy ownership | Agent-owned tier-3 runtime data, `ignore_changes` in the module | OpenTofu manages the filter policy | Its value is a runtime-claimed name (§6.3) that OpenTofu cannot know. Asserting it makes every `apply` silently break routing on a host that still looks healthy (§18.1). |
| Unbound subscription | Placeholder `__unclaimed__`, unclaimable by §6.3's charset | Omit the filter policy until the agent binds it | A subscription with no filter policy matches *everything*, so an unbound host would receive the whole fleet's traffic. Fail closed. |
| Inter-repo coupling | Derived naming convention + `NAMING.md` | Pipe OpenTofu outputs into host configs | Rebuilds the dependency the repo split removes. Convention means infrastructure state can be lost and reimported with zero impact on running agents (§18.4). |
| Env separation | One root module per `(env, region)` | Workspaces | Workspaces select the target through mutable shell state. For infrastructure that can reboot a fleet, the target belongs in the path and in the PR diff. |
| State locking | S3 `use_lockfile` | DynamoDB lock table | Deprecated in Terraform from 1.11; one fewer resource and a smaller IAM surface (§18.5). |
| IaC tool | OpenTofu | Terraform | Native state encryption. `sqsh` state is a full fleet topology, and `encrypt = true` leaves it plaintext in plan files and CI artifacts. HCL stays compatible with both. |
| Script paths | `os.Root` + `fstat` on the handle | `filepath.Join` + `HasPrefix` | Prefix checks lose to symlinks and to TOCTOU between check and open. |
| Naming | `sqshd` / `sqsh`, `sqsh-` for AWS resources | Keeping `sqsd`/`sqsctl`; renaming the project | Binaries now match the project and the repo. The Sybase `sqsh` collision is accepted rather than solved — see §21.1. |
| Privilege | Root + systemd hardening | Unprivileged + wildcard sudo | Wildcard sudo rules are routinely bypassable and imply containment that does not exist. Narrow generated sudoers is offered as variant (B). |

---

## 21. Open questions

Resolved in draft 3: host identity (§6.2–6.3, claim-with-suffix), store-unavailable
behaviour (§8.7, execute and spool), storage backend (§7.5, DynamoDB control plane with
an S3 Tables mirror). Resolved in draft 4: provisioning (§18, separate OpenTofu
repository with explicit resource-ownership tiers). Resolved in draft 5: claim
reassertion (§6.3, §9.4, conditional heartbeat with `ErrClaimLost` fatal), which closes
the partition half of question 3 below; result-status monotonicity (§7.2.1,
`accepted` is provisional and `Complete` refuses backward writes); and naming
(question 1, `sqshd`/`sqsh`).

1. ~~**Naming.**~~ **Resolved in draft 5:** binaries are `sqshd` (daemon) and `sqsh`
   (CLI), consistent with the project and the repository. Config lives at
   `/etc/sqshd/`, state at `/var/lib/sqshd/`, and metrics are prefixed `sqshd_`.
   AWS resource names keep the `sqsh-` project prefix (`sqsh-cmd-<instance_ref>`,
   `sqsh-results`, `sqsh-inventory`) — those name the system, not the binary.

   The collision with the Sybase/SQL `sqsh` shell is **accepted, not solved.** It will
   confuse package search and web results, and the CLI name is now exactly the colliding
   token. Renaming the project was considered and rejected as more churn than the
   confusion is worth for an internal tool; if `sqsh` is ever published to a public
   package index, revisit it there rather than reopening it here.
2. **Per-host serialization.** `max_concurrent_commands: 4` permits a `puppet` run and
   a `systemctl restart puppet.service` to overlap. Is a global mutex for all
   `AtMostOnce` commands acceptable (simple, and matches how a human would operate),
   or is a per-unit lock needed?
3. **`stale_claim_ttl` default of 24h.** The partition half of this is **resolved**: the
   heartbeat is a conditional reassertion (§6.3), so a returning agent detects the
   takeover within one `heartbeat_interval` and exits 78 (§9.4) instead of silently
   clobbering the new owner. No liveness check beyond `last_seen` is needed —
   exit-78-plus-alert is the right answer, provided the alert is shaped as §15.2 requires.

   What remains is tuning, and §8.7 gives it a floor it did not have: because a
   store-unreachable agent skips the conditional claim, a partition longer than
   `stale_claim_ttl` can open a window in which two agents share a name *and* neither can
   detect the duplicate. So `stale_claim_ttl` should exceed the longest partition worth
   surviving. That conflicts directly with an autoscaling fleet churning hourly, which
   wants minutes. Is the answer a per-fleet value (long for pets, short for cattle), or
   should churning fleets stop relying on stale takeover altogether and release names
   explicitly at termination via the §19 claim janitor — making `stale_claim_ttl` a
   backstop rather than the mechanism?
4. **Suffix stability vs. name reuse.** Reimaging always produces a new `instance_ref`,
   so names drift upward until stale takeover reclaims them. Should reimaging be
   expected to pass `--claim-name` explicitly, or should provisioning clear the old
   inventory record as a normal teardown step? The second is cleaner but puts
   `sqsh-admin` rights in the provisioning pipeline.
5. **`store: s3` — ship it or defer it?** It is spec'd in §7.5 and costs one interface
   implementation, but two backends means two conformance matrices forever. Worth it
   only if avoiding DynamoDB is a real constraint rather than a preference.
6. **Multi-account / multi-region.** One topic per region, or a global topic with
   cross-region subscriptions? Affects whether `sqsh` needs a region flag, and whether
   name claims are region-scoped or global — a fleet-wide `host_id` namespace across
   regions needs one inventory table, which conflicts with regional isolation.
7. **`expires_at` default of 5 minutes** is tuned for interactive use and will surprise
   CI pipelines that publish and check later. Should `--no-wait` imply a longer
   default?
8. **Tier-2 ownership for autoscaling hosts.** §18.1 requires per-host queues to have
   exactly one owner. Long-lived instances are cleanly OpenTofu-owned; ASG instances need
   `self_register: true`, which costs the agent `sqs:CreateQueue` and a wider IAM
   surface. Is a mixed fleet acceptable (two provisioning paths, two IAM policies to
   audit), or should everything self-register for uniformity and accept the wider
   permission everywhere?
9. **Who reaps ASG-orphaned tier-2 resources?** A self-registered queue outlives its
   instance and is invisible to `tofu plan`, so nothing detects the leak. The §19 claim
   janitor could delete queues whose `instance_ref` no longer resolves, but that hands a
   Lambda the `sqs:DeleteQueue` right deliberately withheld from agents. Lambda with a
   narrow boundary, or a scheduled OpenTofu run that imports and prunes?
10. **`NAMING.md` duplication.** §18.4 makes the naming convention a versioned interface
   present in both repositories, which means it can drift. Generate the OpenTofu locals
   and the Go constants from one machine-readable source (a small JSON/YAML file vendored
   into both), or accept the duplication and cover it with an integration test that
   asserts a real agent finds real resources?
