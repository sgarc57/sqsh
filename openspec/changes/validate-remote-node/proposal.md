# Proposal

## Why

The walking skeleton (`add-walking-skeleton`) is implemented and its unit tests pass, but
it has only been proven on a laptop (LocalStack) and against a single real-AWS isolation
test that has since been torn down. Nothing has yet run the full path on a real node:
publish → SNS routing → SQS receive → execute on the host → result in DynamoDB → `sqsh status`.
That path is the thing the operator needs to trust, so it is the next thing to prove, on
the first real target node.

Doing this by hand is error-prone: the README's AWS setup is a sequence of shell commands
with nested JSON escaping, there is no arm64 build target (the machine is aarch64), and
there is no repeatable way to deploy, run as a service, or tear down.

## Confidentiality of access details

This repository is intended to be published. **No node access detail is committed.** That
covers the node's hostname or domain, SSH port, key path, login user, home directory, and
the AWS account ID. In this change's files they appear as placeholders:

| Placeholder | Meaning | Where the value comes from |
| --- | --- | --- |
| `<host-id>` | sqsh host identity (the `host_id` config value, the filter-policy value, and the suffix of the queue names) | `HOST_ID` in `deploy/remote/node.env` |
| `<node-ssh>` | SSH target (hostname or alias) | `NODE_SSH` |
| `<node-ssh-port>` | SSH port | `NODE_PORT` |
| `<node-ssh-key>` | path to the SSH private key | `NODE_KEY` |
| `<node-user>` | login user on the node | `NODE_USER` |
| `<node-home>` | that user's home directory | derived: `/home/<node-user>` |
| `<account-id>` | AWS account ID | `aws sts get-caller-identity` at run time |

`deploy/remote/node.env` is read by the Makefile and deploy scripts. It is gitignored, and
`deploy/remote/node.env.example` lists the variable names with placeholder values. The
verification record (`verification.md`) is written with the placeholders, not the values.

## What Changes

- Add a repeatable provisioning script that creates, for `<host-id>` in the configured
  region: the `sqsh-commands` topic, the `sqsh-results` table with TTL enabled, the
  `sqsh-cmd-<host-id>` queue with its DLQ and redrive policy, the topic-scoped queue policy,
  and a raw-delivery subscription filtered on `{"host": ["<host-id>"]}`. It is idempotent and
  prints the resource identifiers it used.
- Add a matching teardown script that removes only the resources the provisioning script
  names, and nothing else.
- Add a Makefile target that cross-builds `sqshd` for `linux/arm64`.
- Add a deploy step that copies the `sqshd` binary, its config, and a systemd unit to the
  node over SSH, then enables and starts the unit. The unit is minimal: it runs as the login
  user so the existing AWS credentials are found, and it has no hardening.
- Add a scripted end-to-end check run against the real node, in this order:
  1. Dry run with `execute: false`: the request is accepted and recorded `succeeded`, and
     the daemon log shows the exact argv that was logged, with nothing spawned.
  2. Execute `systemctl status sshd.service`: recorded `succeeded`, exit code 0, non-empty
     stdout.
  3. Execute `systemctl is-active sshd.service`: recorded `succeeded`, exit code 0.
  For each request the script asserts that the `sqsh` CLI exit code matches the stored
  status and that `sqsh --json status <request_id>` returns the same record.
- Record the outcome, with commands, timestamps, and request IDs (placeholders for account and
  node), in `verification.md`.

**Services used by this slice.** The return path is DynamoDB, read with `sqsh status`.
- **SQS** (with its DLQ) is the transport the agent receives commands from. It is required.
- **SNS** is the fan-out point that routes each command to one host by filter policy. It is
  required, because proving that routing is the premise of the walking skeleton
  (`add-walking-skeleton/proposal.md`).
- **DynamoDB** holds results and the claim record. It is required.
- **S3 is not used.** In SPEC it appears only for output offload (§12, deferred) and for
  analytics via S3 Tables (§3). Neither is in this slice.

**Explicitly out of scope** (non-goals):

- Mutating `systemctl` verbs (`restart`, `reload`, `start`, `stop`) and `reboot`/`shutdown`.
  These need `AtMostOnce` and the dedupe/reconciliation machinery (SPEC §8.3–8.4).
- The `nginx` check. The node does not run nginx, so the allowlist for this slice is
  `sshd.service` only.
- `script` command (SPEC §11.5).
- Name claiming, inventory, and heartbeat (SPEC §6.3–6.4). `host_id` stays a config value.
- Ed25519 signature verification (SPEC §7.4). Until it lands, anyone with `sns:Publish` on
  the topic can request the allowlisted read-only commands. Acceptable while the allowlist
  is read-only and the topic is not shared.
- Result spool / degraded mode (SPEC §8.7), S3 output offload (SPEC §12).
- systemd hardening, generated sudoers, and packaging (SPEC §14). The unit file here is a
  minimal stand-in.
- OpenTofu (SPEC §18). The provisioning script is a stopgap that §18 later replaces.

## Dependencies on the node

The test assumes only the following on the node. Nothing else is installed or configured.

- SSH reachable on `<node-ssh-port>` with the operator's key (`<node-ssh-key>`).
- systemd as the init system, with `sshd.service` active (on this Debian box it is an alias
  for `ssh.service`).
- Passwordless `sudo` for `<node-user>`, needed only to install and enable the system unit.
  Verified present on the node.
- AWS CLI credentials for `<node-user>` under `<node-home>/.aws`, which the daemon's default
  credential chain uses. Verified present for the account in use.

## Capabilities

This change adds scripts, a build target, a systemd unit, and a verification record. It does
not change any requirement of the system's behavior: the wire protocol, routing, dispatch,
result store, and CLI are used exactly as the walking skeleton defines them. No delta specs
are created; `.openspec.yaml` declares `skip_specs: true`.

### New Capabilities

None.

### Modified Capabilities

None.

## Impact

- **New files**: `deploy/provision/` (create and teardown scripts), `deploy/remote/` (deploy
  and end-to-end check scripts), `deploy/remote/node.env.example`, `deploy/config.remote.yaml.tmpl`,
  `deploy/sqshd.service`, and `openspec/changes/validate-remote-node/verification.md`.
- **`.gitignore`**: ignores `deploy/remote/node.env` (the real node values).
- **Makefile**: new `build-linux-arm64`, `provision`, `teardown`, `deploy`, and `e2e`
  targets, which take `HOST_ID` and the node variables from `node.env`. Existing targets are
  unchanged.
- **AWS**: creates `sqsh-commands`, `sqsh-results`, `sqsh-cmd-<host-id>`, and
  `sqsh-cmd-<host-id>-dlq` in the configured region. The account currently has none of these,
  so nothing is overwritten. Costs are negligible.
- **Remote node**: installs `/usr/local/bin/sqshd`, `/etc/sqshd/config.yaml`, and
  `/etc/systemd/system/sqshd.service`, then enables and starts that unit. The `sqsh` CLI runs
  from the operator's workstation, not the node.
- **Code**: no changes to `cmd/`, `internal/`, or existing specs. `add-walking-skeleton` is
  not modified.
