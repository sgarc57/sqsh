# Design

## Context

The walking skeleton builds with `make build` for the host architecture only. Its AWS
setup is documented as shell commands in `README.md` ("Manual AWS setup"); the account
currently has no sqsh resources, so provisioning must create them. The target node is
reached over SSH on a non-default port with an operator-held key. It runs Debian 13 on
aarch64, with systemd, passwordless `sudo` for the login user, and AWS CLI credentials for
the IAM identity used in the earlier real-AWS isolation run.

Access details are placeholders in this document and in every committed file; see
proposal.md "Confidentiality of access details" for the table of placeholders and where the
real values live.

`sqshd` reads `/etc/sqshd/config.yaml` by default and has no service unit. See proposal.md
for motivation, the services this slice uses (SQS, SNS, DynamoDB; not S3), and the
non-goals.

## Goals / Non-Goals

**Goals:**

- A fresh operator can provision, deploy, run as a systemd service, and tear down the slice
  from a checkout, one command per step, with no hand-typed JSON escaping. The operator
  supplies the node values in a local, gitignored file.
- The end-to-end check runs against real SQS, SNS, and DynamoDB and the real node, and
  fails on any mismatch between the CLI exit code and the stored status.
- The result is a verification record another person can re-run, with no access details in
  it.

**Non-Goals:**

- Any change to `cmd/`, `internal/`, or the wire protocol.
- Hardening, a dedicated service account, or log rotation.
- Repairing drift. The scripts create what is missing and refuse to overwrite a resource
  whose settings differ.
- Multi-host provisioning. The scripts take `HOST_ID`, but one host is exercised.

## Decisions

**D1. Provisioning uses bash and the AWS CLI, not OpenTofu.**
SPEC §18 places infrastructure in a separate OpenTofu repository, which does not exist yet.
Adding it here would front-load that project. The scripts are a stopgap and are named as
such so §18 can replace them. *Alternative rejected:* extending the README snippet in place,
because it cannot check for existing resources or tear down cleanly.

**D2. Each create step checks for the resource first, then creates it.**
`create-queue` and `create-topic` are naturally idempotent when attributes match, but
`create-table` fails on an existing table and `subscribe` with different attributes fails.
Checking first gives one behaviour: exists with expected settings → keep; exists with
different settings → stop with an error naming the resource; missing → create.
*Alternative rejected:* delete-and-recreate, which is destructive on a results table.

**D3. The filter policy is set at subscription creation, not by the agent.**
The SPEC §6.5 runtime-subscribe path is deferred. The script creates the subscription with
`FilterPolicy={"host":["<host-id>"]}` and `RawMessageDelivery=true` directly, so the
two-writer problem in §6.5 does not arise in this slice.

**D4. Queue visibility is 90 s; `max_command_timeout` stays at its 60 s default.**
This satisfies `visibility_timeout >= max_command_timeout + 30s` (SPEC §8.5) with no config
change, and matches the README's queue attributes.

**D5. The node runs `sqshd` as a system unit installed with `sudo`, running as the login
user.** The unit lives at `/etc/systemd/system/sqshd.service` with `User=<node-user>` and
`Environment=HOME=<node-home>`, so the daemon's default AWS credential chain finds
`<node-home>/.aws`. Installing and enabling a system unit needs root, and passwordless `sudo`
is present on the node, so this is the simplest form that survives reboot. *Alternatives
rejected:* a user unit (needs `loginctl enable-linger`, which also needs root, so it saves
nothing); `nohup` (does not survive reboot and needs a pid-file wrapper). The unit has no
hardening (SPEC §14.2 is deferred).

**D6. The dry-run and execute phases use two configs, not a runtime flag.**
`execute: false` is a config key. The e2e script writes the config, restarts the unit, and
runs the first phase; it repeats with `execute: true` for the second. This tests the same
switch an operator flips, rather than adding a test-only flag.

**D7. Cross-compilation is a Makefile target with `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`.**
The binaries are static and need only `systemctl` from the node, which it already has.
Output goes to `bin/linux-arm64/`, leaving the host build in `bin/` untouched.

**D8. The end-to-end check runs from the operator workstation, not the node.**
The CLI is what must reach the host, so testing from the node would hide a broken publish
path. The node is reached to deploy, to read the journal when a step fails, and to confirm
the dry-run argv in the log.

**D9. Assertions use the CLI's JSON output, not log scraping, where possible.**
`sqsh --json status <id>` provides the stored status and exit code. The one log check is the
dry-run argv, since the result record carries no argv field (SPEC §7.2).

**D10. Node access values live in a gitignored `deploy/remote/node.env`.**
The Makefile includes that file if present and fails with a pointer to
`node.env.example` if it is missing. The example file has the variable names and
placeholder values; the real file is never committed. Scripts print the node's address only
where the operator asked for it, never into a committed file, and the verification record
uses placeholders. *Alternative rejected:* a committed config with a hostname and an
"ignore local changes" workaround, which is easy to leak by accident.

## Risks / Trade-offs

- **Access details leak into a commit** → `node.env` is gitignored. Before the repo is
  published, a grep for the real values (kept by the operator, not in the repo) runs against
  the tree. This is a manual step; a pre-commit check is a follow-up.
- **Git history already contains the account ID and an IAM user name** → The closed
  `add-walking-skeleton/verification.md` is in history. Removing it from history is a
  rewrite and is not part of this change; see the hand-off note below.
- **Filter-policy propagation delay** → The README notes up to a minute. The e2e script waits
  for the dry-run request to be received before asserting, and fails with the queue name if
  no result appears within 120 s.
- **The daemon uses an existing IAM user's keys, with broad scope** → Accepted for this test;
  the same identity ran the real-AWS isolation test. The deploy script does not copy
  credentials. Least privilege is a follow-up (README "IAM, minimally").
- **`sqshd` runs as the login user, who has passwordless sudo** → The daemon does not use
  sudo itself. Its allowlist is read-only `systemctl`, so a compromised command path cannot
  restart or reboot the node in this slice. Hardening is deferred.
- **`systemctl` results depend on unit state on the node** → `sshd.service` is active, and
  the assertions expect exit 0. If it stops, the check fails and reports the observed state.
  The exit-3 path is covered by `TestSystemctlShowExit3IsFailure` and the stubbed `systemctl`
  tests, not by this run.
- **The unit stays enabled after the run** → The e2e script stops the unit on exit (trap) but
  leaves it enabled, so the daemon comes back after reboot. `make disable` removes that, for
  manual cleanup.
- **Teardown removes results history** → Teardown deletes the results table. It needs
  `CONFIRM=yes`.

## Migration Plan

1. Copy `deploy/remote/node.env.example` to `deploy/remote/node.env` and fill it in.
2. `make provision HOST_ID=<host-id>` creates the resources; re-running it is a no-op.
3. `make build-linux-arm64 && make deploy ACTION=install` copies the binary, config, and unit,
   then runs `daemon-reload`, `enable`, and `start`.
4. `make e2e` runs the dry-run phase, then the execute phase, and writes the outcome to
   `verification.md`.
5. Rollback: `make deploy ACTION=disable` stops and disables the unit, and
   `make teardown CONFIRM=yes` removes the AWS resources. Neither touches
   `add-walking-skeleton` or the node's other services.

## Hand-off note: existing history

`openspec/changes/add-walking-skeleton/verification.md`, committed in the prior history,
contains the AWS account ID and an IAM user name. This change does not edit a closed change,
and rewriting history is destructive, so the choice is left to the operator. Options are: rewrite
the history with `git filter-repo` before publishing, or publish a fresh repository from the
current tree. Either needs the operator's go-ahead.

## Open Questions

- **Should the unit stay enabled after the e2e run?** Defaulted to enabled, so it survives a
  reboot. Changing the default to disabled is a one-line change.
