# sqsh

Run allowlisted commands on hosts through SNS → SQS, with results recorded in DynamoDB.
`sqsh` (operator CLI) publishes a request; `sqshd` (agent) on the target host receives it
from its own queue, validates it, runs it without a shell, and records the outcome.

`SPEC.md` is the full design. **This build is the walking skeleton**
(`openspec/changes/archive/2026-10-20-add-walking-skeleton`): the thinnest end-to-end slice that proves a
command reaches exactly the right host and its result comes back.

## What works in this release

- `sqsh systemctl <host> <verb> <unit>` with the read-only verbs `status`, `is-active`,
  `is-enabled`, `show`. Verb and unit are checked against separate allowlists.
- `sqsh status <request_id>` to read a recorded result.
- Server-side routing by SNS filter policy, with the agent re-checking `target.host`.
- Every request ends in a recorded result: `succeeded`, `failed`, `timed_out`,
  `rejected` (with a reason), or `expired`.
- Duplicate deliveries execute once (conditional claim in DynamoDB).
- Structured JSON logs and Prometheus metrics.

## What is NOT implemented yet

Each item below is specified in `SPEC.md` and deferred to a later change. Until then it
**does not exist**, whatever `SPEC.md` says:

| Not implemented | SPEC.md | Consequence today |
| --- | --- | --- |
| `reboot`, `shutdown`, state-changing `systemctl` verbs, `AtMostOnce` handling | §8.1, §11.1 | Only read-only `systemctl` queries can run. |
| `script` command | §11.5 | — |
| `puppet` | §11.4 | Registered but disabled: always rejected `command_not_allowed`. |
| Name claiming, inventory, heartbeat, `sqsh hosts`, `sqsh release` | §6.3–6.4 | `host_id` comes from config. **Two hosts with the same `host_id` will both receive commands**; the conditional claim lets only one run each, but which one is arbitrary. Treat any `duplicate` rejection as a misconfiguration alarm. CLI exit code 5 is unused. |
| Dedupe store, startup reconciliation | §8.3–8.4 | Not needed while every command is `AtLeastOnce`. |
| Result spool / degraded mode | §8.7 | If DynamoDB is unreachable after a command runs, its result is lost (logged only). If the claim cannot be written, the command does not run and the message is left for redelivery. |
| S3 output offload, `--full` | §12 | Output beyond `max_output_bytes` per stream is dropped (the tail is kept) and the result is flagged `output_truncated`. |
| Signature verification | §7.4 | The signing string is computed and tested but never verified. |
| systemd unit, sudoers, packaging, privilege model | §14 | Run the daemon in the foreground. Not deployable as-is. |
| `SIGHUP` reload, `sqsh retry` | §13, §8.6 | — |
| OpenTofu infrastructure | §18 | Create resources by hand (below). |

## Build and test

```sh
make build          # bin/sqshd, bin/sqsh
make test           # unit tests, no AWS needed
make lint
make integration    # needs Docker: starts LocalStack, runs test/integration, stops it
```

## Manual AWS setup

One topic, one results table, and per host: a queue, its queue policy, and a filtered
raw-delivery subscription. Example for `web01` in `eu-central-1`:

```sh
REGION=eu-central-1
ACCOUNT=$(aws sts get-caller-identity --query Account --output text)
HOST=web01

TOPIC_ARN=$(aws sns create-topic --region $REGION --name sqsh-commands --query TopicArn --output text)

aws dynamodb create-table --region $REGION --table-name sqsh-results \
  --attribute-definitions AttributeName=request_id,AttributeType=S \
  --key-schema AttributeName=request_id,KeyType=HASH --billing-mode PAY_PER_REQUEST
aws dynamodb wait table-exists --region $REGION --table-name sqsh-results
aws dynamodb update-time-to-live --region $REGION --table-name sqsh-results \
  --time-to-live-specification Enabled=true,AttributeName=ttl

# Queue with a DLQ: rejected messages are deleted, so only repeated failures land here.
aws sqs create-queue --region $REGION --queue-name sqsh-cmd-$HOST-dlq
DLQ_ARN=arn:aws:sqs:$REGION:$ACCOUNT:sqsh-cmd-$HOST-dlq
QUEUE_URL=$(aws sqs create-queue --region $REGION --queue-name sqsh-cmd-$HOST \
  --attributes "{\"VisibilityTimeout\":\"90\",\"RedrivePolicy\":\"{\\\"deadLetterTargetArn\\\":\\\"$DLQ_ARN\\\",\\\"maxReceiveCount\\\":\\\"3\\\"}\"}" \
  --query QueueUrl --output text)
QUEUE_ARN=arn:aws:sqs:$REGION:$ACCOUNT:sqsh-cmd-$HOST

# Allow only this topic to deliver to the queue.
aws sqs set-queue-attributes --region $REGION --queue-url $QUEUE_URL --attributes "{\"Policy\":\"$(
  printf '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"sns.amazonaws.com"},"Action":"sqs:SendMessage","Resource":"%s","Condition":{"ArnEquals":{"aws:SourceArn":"%s"}}}]}' \
    $QUEUE_ARN $TOPIC_ARN | sed 's/"/\\"/g')\"}"

# Raw delivery (the SQS body is the request JSON verbatim) filtered to this host.
aws sns subscribe --region $REGION --topic-arn $TOPIC_ARN --protocol sqs \
  --notification-endpoint $QUEUE_ARN \
  --attributes "{\"RawMessageDelivery\":\"true\",\"FilterPolicy\":\"{\\\"host\\\":[\\\"$HOST\\\"]}\"}"
```

Filter-policy changes take up to a minute to propagate in SNS.

IAM, minimally: the agent needs `sqs:ReceiveMessage`, `sqs:DeleteMessage`,
`sqs:ChangeMessageVisibility`, `sqs:GetQueueAttributes` on its own queue and
`dynamodb:PutItem` on the table. The operator needs `sns:Publish` on the topic and
`dynamodb:GetItem` on the table. Both need `sts:GetCallerIdentity` (always allowed).

## Running the daemon in the foreground

```sh
cp deploy/config.example.yaml ./sqshd.yaml   # edit host_id, region, queue_url, units
./bin/sqshd -config ./sqshd.yaml
```

Startup validates the config (exit **78** on any error, including unknown keys), verifies
the AWS identity and logs its ARN, checks the queue exists, starts metrics on
`metrics_addr`, and begins long-polling. `SIGTERM`/`SIGINT` stops receiving, waits up to
`shutdown_grace` for running commands, then kills what remains and records it `failed`.

Set `execute: false` to dry-run: the exact argv is logged and a `succeeded` result
recorded, but nothing is spawned.

## Using the CLI

```sh
export AWS_REGION=eu-central-1
export SQSH_TOPIC_ARN=arn:aws:sns:eu-central-1:123456789012:sqsh-commands
export SQSH_RESULTS_TABLE=sqsh-results      # default

sqsh systemctl web01 status nginx            # waits for the result (default 2m)
sqsh --no-wait systemctl web01 is-active sshd
sqsh --json status 018f3a2c-6c1e-7b3a-9f2d-4a1b6c8e0d55
```

Exit codes: `0` succeeded · `1` failed or timed out · `2` usage error · `3` rejected or
expired · `4` gave up waiting (the command was **not** cancelled and may still run) ·
`6` local or AWS error.

## Testing against real AWS

The routing premise is a property of real SNS filter policies; LocalStack reimplements
them. `make integration-real-aws` runs the isolation test with your default AWS
credentials, creating and then deleting `sqsh-it-*` resources in `SQSH_IT_REGION`
(default `eu-central-1`).

## Remote node test

This slice has been exercised end to end on one real node (see
`openspec/changes/validate-remote-node/verification.md`). The node is reached over SSH, and
its access details are never committed.

Copy `deploy/remote/node.env.example` to `deploy/remote/node.env` (gitignored) and fill in
the host ID, SSH target, port, key, login user, and home directory. Then run the targets in
order:

```sh
make provision   # create the topic, results table, queue, DLQ, policy, and subscription
make deploy ACTION=install   # cross-build linux/arm64 and install sqshd, config, and unit
make deploy ACTION=status    # systemctl status and the last 50 journal lines
make e2e                     # dry run, then execute, then an unsubscribed host
make teardown CONFIRM=yes    # remove this host's queue, DLQ, and subscription
```

`make deploy` also takes `ACTION=start|stop|restart|enable|disable`. `make teardown` keeps the
shared topic while other hosts are subscribed, and keeps the results table unless
`DROP_RESULTS=yes` is set. `make e2e E2E_FLAGS=--stop` leaves the unit stopped afterwards.

Node dependencies, beyond SSH itself: systemd with `sshd.service` active, passwordless `sudo`
for the login user (needed to install the unit), and AWS credentials for that user under its
home directory.

## Examples against the remote node

These were run against the test node. Replace `<host-id>` with your `HOST_ID`. The CLI
needs the same environment as the e2e check, and the topic ARN is the one `make provision`
created:

```sh
export AWS_REGION=eu-central-1
export SQSH_TOPIC_ARN=<topic-arn>          # aws sns list-topics, the sqsh-commands topic
export SQSH_RESULTS_TABLE=sqsh-results
```

**SSH service status.** Waits for the result and prints it:

```sh
sqsh systemctl <host-id> status sshd.service
```

Prints `status: succeeded (exit 0)`, then the `systemctl status` output on stdout. Exit `0`.

**Is sshd active, and is it enabled?**

```sh
sqsh systemctl <host-id> is-active sshd.service    # prints: active, exit 0
sqsh systemctl <host-id> is-enabled sshd.service   # prints: alias, exit 0 (sshd.service is an alias for ssh.service on Debian)
```

**Full unit properties** (long output, so use `--json` to keep it structured):

```sh
sqsh --json systemctl <host-id> show sshd.service
```

**Publish without waiting, then read the result later:**

```sh
id=$(sqsh --no-wait systemctl <host-id> is-active sshd.service)
sqsh status "$id"            # human-readable
sqsh --json status "$id"     # the stored record as JSON
```

**Requests the agent rejects.** These are recorded, not dropped, and exit `3`:

```sh
sqsh systemctl <host-id> restart sshd.service   # rejected: arg_not_allowed, verb "restart" is not allowed
sqsh systemctl <host-id> status nginx.service   # rejected: arg_not_allowed, unit "nginx.service" is not allowed
```

**A host with no subscription.** The CLI gives up waiting after `--wait-timeout` and exits
`4`. It does not cancel anything, and no record appears:

```sh
sqsh --wait-timeout 30s systemctl web99 status sshd.service
```

**Not supported: a public address.** The only commands this slice runs are read-only
`systemctl` queries on `sshd.service`. No command returns the node's public address. Adding one
means a new command in the allowlist, which is a design change, not a configuration change.
