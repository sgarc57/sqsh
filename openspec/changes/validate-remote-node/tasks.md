## 0. Confidentiality

- [ ] 0.1 Add `deploy/remote/node.env` to `.gitignore` (the existing `.env` patterns do not match it)
- [ ] 0.2 Write `deploy/remote/node.env.example` with the variable names `HOST_ID`, `NODE_SSH`, `NODE_PORT`, `NODE_KEY`, `NODE_USER`, and `NODE_HOME`, set to placeholder values
- [ ] 0.3 Make the Makefile include `deploy/remote/node.env` when present, and fail with a pointer to the example file when a remote target runs without it
- [ ] 0.4 Make the deploy and e2e scripts read the node values from the environment only; no script contains a literal node address, port, key path, or user
- [ ] 0.5 Before the first commit of this change, run `git grep` for the real values (kept by the operator, not in the repo) across the working tree and staged files; expect no matches

## 1. Provisioning

- [ ] 1.1 Write `deploy/provision/provision.sh` taking `HOST_ID`: create the topic `sqsh-commands`, the table `sqsh-results` (PAY_PER_REQUEST, TTL on `ttl`), the DLQ `sqsh-cmd-<host-id>-dlq`, and the queue `sqsh-cmd-<host-id>` (VisibilityTimeout 90, RedrivePolicy maxReceiveCount 3), following README "Manual AWS setup" without its hand-escaped JSON
- [ ] 1.2 Add the topic-scoped queue policy (`sns.amazonaws.com`, `sqs:SendMessage`, `aws:SourceArn` = topic ARN) built with `jq` or a here-document, not `sed` escaping
- [ ] 1.3 Create the raw-delivery subscription with `FilterPolicy={"host":["<host-id>"]}`; if a subscription already exists for the queue with different attributes, stop with an error naming it (design D2)
- [ ] 1.4 Make every create step check-then-create and print the ARN or URL it used; re-running must be a no-op
- [ ] 1.5 Write `deploy/provision/teardown.sh` removing only the subscription, queue, DLQ, topic, and table it names; require `CONFIRM=yes`
- [ ] 1.6 Run `make provision` twice; the second run creates nothing and exits 0
- [ ] 1.7 Confirm with `aws sns list-subscriptions-by-topic` that the subscription's `FilterPolicy` and `RawMessageDelivery` are set as specified

## 2. Build

- [ ] 2.1 Add `build-linux-arm64` to the Makefile: `CGO_ENABLED=0 GOOS=linux GOARCH=arm64` with the same `LDFLAGS`, output to `bin/linux-arm64/sqshd` (design D7)
- [ ] 2.2 Verify the output is an aarch64 ELF with `file`, and that `sqshd -version` prints the commit

## 3. Deploy

- [ ] 3.1 Write `deploy/config.remote.yaml.tmpl`, rendered from `deploy/config.example.yaml`, with `host_id: <host-id>`, the provisioned queue URL, the topic ARN, `results_table: sqsh-results`, `allowed_commands: [systemctl]`, `units: [sshd.service]` (no nginx; see proposal "Dependencies on the node"), and `metrics_addr: 127.0.0.1:9787`
- [ ] 3.2 Write `deploy/sqshd.service`: `ExecStart=/usr/local/bin/sqshd -config /etc/sqshd/config.yaml`, `User=<node-user>`, `Environment=HOME=<node-home>`, `Restart=on-failure`, no hardening (design D5). The rendered unit is produced from a template at install time, so the real user never lives in the repo
- [ ] 3.3 Write `deploy/remote/deploy.sh <action>` over `ssh -p $NODE_PORT -i $NODE_KEY $NODE_USER@$NODE_SSH`. Actions: `install` (copy binary to `/usr/local/bin/sqshd`, rendered config to `/etc/sqshd/config.yaml`, rendered unit to `/etc/systemd/system/`, then `sudo systemctl daemon-reload`), `start` and `stop`, `enable` and `disable`, and `status` (`systemctl status` and `journalctl -u sqshd -n 50`)
- [ ] 3.4 Make `install` and `start` fail fast with a clear message if `sudo -n true` fails on the node, rather than hanging on a password prompt
- [ ] 3.5 Add `deploy` with `ACTION=<action>` and the `build-linux-arm64` prerequisite to the Makefile
- [ ] 3.6 Run `install`, `enable`, `start`; confirm the journal shows "AWS identity verified" with the expected account and "queue resolved", and no config error (exit 78)

## 4. End-to-end check

- [ ] 4.1 Write `deploy/remote/e2e.sh` that renders the config with `execute: false`, installs it, restarts the unit, and waits for the journal to log "dry-run mode" before publishing
- [ ] 4.2 Dry run: `sqsh systemctl <host-id> status sshd.service`. Assert the CLI exits 0, the stored record has status `succeeded`, and the journal contains the exact argv `systemctl status sshd.service` with no process spawned
- [ ] 4.3 Render the config with `execute: true`, reinstall, restart the unit, and wait for the "dry-run" warning to be absent from the journal
- [ ] 4.4 Execute `systemctl status sshd.service`. Assert CLI exit 0, stored `succeeded` with `exit_code: 0`, and non-empty stdout in the record
- [ ] 4.5 Execute `systemctl is-active sshd.service`. Assert CLI exit 0, stored `succeeded` with `exit_code: 0`
- [ ] 4.6 For each request, assert `sqsh --json status <request_id>` returns the same status and exit code as the CLI's own exit mapping
- [ ] 4.7 Publish one request addressed to an unsubscribed host and assert no result is written within the wait timeout. This shows routing excludes it (SPEC §3)
- [ ] 4.8 Restore the `execute: true` config on exit with a `trap`, whether the checks passed or failed. Stop the unit only if the operator passes `--stop`
- [ ] 4.9 Exit non-zero on any assertion failure and print the failing request ID and the relevant journal lines

## 5. Verification record

- [ ] 5.1 Write `openspec/changes/validate-remote-node/verification.md` with: date, region, target as `<host-id>`, the account as `<account-id>`, the node as `<node-ssh>` (with the port and user as placeholders), `uname -m` output, the binary's commit, each request ID with command, stored status, and exit code, and the pass/fail of each check in section 4. Do not write real access values into it
- [ ] 5.2 Record the dependency check results from proposal "Dependencies on the node" (sudo, sshd active, AWS identity) as observed at the time of the run
- [ ] 5.3 Run `make teardown CONFIRM=yes` only if the operator asks; otherwise leave the resources and unit state as the record states

## 6. Documentation

- [ ] 6.1 Add a "Remote node test" section to `README.md` listing the Makefile targets in order, a one-line description of each, the node dependencies from the proposal, and a link to `verification.md`. Use placeholders only and point to `node.env.example`
- [ ] 6.2 Leave the "What is NOT implemented yet" table as is; only note that this slice has been exercised on one real node

## 7. Hand-off

- [ ] 7.1 Report to the operator that `add-walking-skeleton/verification.md` in git history contains the account ID and IAM user name, and get a decision on rewriting history before the repo is published (design, "Hand-off note")
