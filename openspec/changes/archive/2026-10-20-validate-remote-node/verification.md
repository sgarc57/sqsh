# Verification record: validate-remote-node

Placeholders stand in for the account, the node, and the operator's access details. The real
values live in `deploy/remote/node.env` (gitignored) and in the AWS account.

## Target

- Region: `eu-central-1`
- Host identity (`HOST_ID`): `<host-id>`
- Account: `<account-id>`
- Node: `<node-ssh>`, SSH on `<node-ssh-port>`, login user `<node-user>`
- Node architecture: aarch64 (`uname -m`), Debian 13, systemd
- Binary: `sqshd` built for `linux/arm64` from commit `0b0d7c9` plus the uncommitted
  `fix-receive-loss` change (the version string is the commit short hash only)

## Node dependencies, as observed

| Dependency | Observed |
| --- | --- |
| SSH on the node's port with the operator's key | yes |
| systemd, with `sshd.service` active (an alias for `ssh.service`) | yes, `active` |
| Passwordless `sudo` for `<node-user>` | yes (`sudo -n true` succeeds) |
| AWS credentials for `<node-user>` under its `.aws` directory | yes, identity verified at daemon startup |

## Infrastructure

- `make provision` created the topic, results table (TTL enabled), queue and DLQ, the
  topic-scoped queue policy, and the raw-delivery subscription filtered on `host=<host-id>`.
- A second `make provision` changed nothing.
- The subscription was checked in AWS: `RawMessageDelivery=true`, `FilterPolicy={"host":["<host-id>"]}`.
- Teardown was **not** run. Its confirmation guard was tested (it refuses without `CONFIRM=yes`).

## End-to-end runs

The first two runs failed. The third failed on a wrong expectation in the script alone, and
the fourth passed.

| Run | Result | Cause and action |
| --- | --- | --- |
| 1 | aborted | The script waited for the wrong `dry_run` value. Fixed in the script. |
| 2 | failed, 10 checks | Two requests were received but not handled for their 90 s visibility timeout, then executed late (`attempt: 2`). The dry-run request ran for real. Cause: the receive path dropped messages when shutdown cancelled an in-flight poll. Fixed in `fix-receive-loss`. |
| 3 | failed, 1 check | The logged argv was `status --no-pager -- sshd.service`, which the systemctl command adds on purpose. The expectation in the script was wrong. Fixed in the script. All other checks passed, and every stored record had `attempt: 1`. |
| 4 | **passed, all checks** | Clean run on the fixed code. |

### Run 4 detail

| Check | Request | Stored status | Exit | Attempt |
| --- | --- | --- | --- | --- |
| Dry run: `systemctl status sshd.service` | `01a15988-e232-79ca-a7b8-1718bcae3d99` | succeeded | 0 | 1 |
| Logged argv (dry run) | — | — | — | — |
| Execute: `systemctl status sshd.service` | `01a15989-33c9-7367-8ba5-6dbc9ce8be89` | succeeded | 0 | 1 |
| Execute: `systemctl is-active sshd.service` | `01a15989-3824-749b-9746-8817f808d26e` | succeeded | 0 | 1 |
| Unsubscribed host `web99`: no result | `01a15989-3c61-79d1-a63d-30149dd725f8` | (no record) | CLI 4 | — |

Each request's CLI exit code matched the status read back with `sqsh status`. The dry run
logged `status --no-pager -- sshd.service` with nothing spawned. The daemon never logged the
unsubscribed request. The queue and DLQ were empty afterwards.

## Conclusion

The remote-node path works end to end on the fixed code: publish, routing to the subscribed
host only, receive, execute (dry run and real), record, and read back through `sqsh status`. The
failure found in run 2 is fixed, and it did not recur in runs 3 or 4.

Open follow-ups, all outside this change:
- A response lost in transit after SQS delivers a message still hides it until the visibility
  timeout (see `fix-receive-loss` proposal). The new `message received` log lines make it visible.
- The `is-active` loss in run 2 had no confirmed mechanism. It did not recur.
- Teardown has not been exercised against AWS.
