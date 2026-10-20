## 0. Confidentiality

- [x] 0.1 Add `deploy/remote/node.env` to `.gitignore` (the existing `.env` patterns do not match it)
- [x] 0.2 Write `deploy/remote/node.env.example` with the variable names `HOST_ID`, `NODE_SSH`, `NODE_PORT`, `NODE_KEY`, `NODE_USER`, and `NODE_HOME`, set to placeholder values
- [x] 0.3 Make the Makefile include `deploy/remote/node.env` when present, and fail with a pointer to the example file when a remote target runs without it
- [x] 0.4 Make the deploy and e2e scripts read the node values from the environment only; no script contains a literal node address, port, key path, or user (checked: `git grep` and the untracked-file scan are clean)
- [x] 0.5 Before the first commit of this change, run `git grep` for the real values (kept by the operator, not in the repo) across the working tree, staged files, and history; expect no matches. Done: clean on the tree at `0a17163` and across all history.

## 1. Provisioning

- [x] 1.1 Write `deploy/provision/provision.sh` taking `HOST_ID`: create the topic `sqsh-commands`, the table `sqsh-results` (PAY_PER_REQUEST, TTL on `ttl`), the DLQ `sqsh-cmd-<host-id>-dlq`, and the queue `sqsh-cmd-<host-id>` (VisibilityTimeout 90, RedrivePolicy maxReceiveCount 3), following README "Manual AWS setup" without its hand-escaped JSON
- [x] 1.2 Add the topic-scoped queue policy (`sns.amazonaws.com`, `sqs:SendMessage`, `aws:SourceArn` = topic ARN) built with `jq` or a here-document, not `sed` escaping
- [x] 1.3 Create the raw-delivery subscription with `FilterPolicy={"host":["<host-id>"]}`; if a subscription already exists for the queue with different attributes, stop with an error naming it (design D2)
- [x] 1.4 Make every create step check-then-create and print the ARN or URL it used; re-running must be a no-op
- [x] 1.5 Write `deploy/provision/teardown.sh` removing this host's subscription, queue, and DLQ, and the topic only when no subscriptions remain on it; the results table only with `DROP_RESULTS=yes`. Require `CONFIRM=yes`. Teardown was not run against AWS; only the confirmation guard was tested
- [x] 1.6 Run `make provision` twice; the second run creates nothing and exits 0
- [x] 1.7 Confirm with `aws sns list-subscriptions-by-topic` that the subscription's `FilterPolicy` and `RawMessageDelivery` are set as specified

## 2. Build

- [x] 2.1 Add `build-linux-arm64` to the Makefile: `CGO_ENABLED=0 GOOS=linux GOARCH=arm64` with the same `LDFLAGS`, output to `bin/linux-arm64/sqshd` (design D7)
- [x] 2.2 Verify the output is an aarch64 ELF with `file` (done), and that the commit is reported on the node (the journal shows `commit: 0b0d7c9` at startup)

## 3. Deploy

- [x] 3.1 Write `deploy/config.remote.yaml.tmpl`, rendered from `deploy/config.example.yaml`, with `host_id: <host-id>`, the provisioned queue URL, the topic ARN, `results_table: sqsh-results`, `allowed_commands: [systemctl]`, `units: [sshd.service]` (no nginx; see proposal "Dependencies on the node"), and `metrics_addr: 127.0.0.1:9787`
- [x] 3.2 Write `deploy/sqshd.service`: `ExecStart=/usr/local/bin/sqshd -config /etc/sqshd/config.yaml`, `User=<node-user>`, `Environment=HOME=<node-home>`, `Restart=on-failure`, no hardening (design D5). The rendered unit is produced from a template at install time, so the real user never lives in the repo
- [x] 3.3 Write `deploy/remote/deploy.sh <action>` over `ssh -p $NODE_PORT -i $NODE_KEY $NODE_USER@$NODE_SSH`. Actions: `install` (copy binary to `/usr/local/bin/sqshd`, rendered config to `/etc/sqshd/config.yaml`, rendered unit to `/etc/systemd/system/`, then `sudo systemctl daemon-reload`), `start`, `stop`, `restart`, `enable`, `disable`, and `status` (`systemctl status` and `journalctl -u sqshd -n 50`). `restart` was added for the e2e check
- [x] 3.4 Make `install` and `start` fail fast with a clear message if `sudo -n true` fails on the node, rather than hanging on a password prompt
- [x] 3.5 Add `deploy` with `ACTION=<action>` and the `build-linux-arm64` prerequisite (for `install`) to the Makefile
- [x] 3.6 Run `install`, `enable`, `start`; confirm the journal shows "AWS identity verified" with the expected account and "queue resolved", and no config error (exit 78)

## 4. End-to-end check

Implemented in `deploy/remote/e2e.sh`. Run 4 passed every check. Runs 1–3 are recorded in `verification.md`.

- [x] 4.1 Write `deploy/remote/e2e.sh` that renders the config with `execute: false`, installs it, restarts the unit, and waits for the journal to log `receive loop starting` with `dry_run: true` before publishing
- [x] 4.2 Dry run: `sqsh systemctl <host-id> status sshd.service`. Assert the CLI exits 0, the stored record has status `succeeded`, and the journal contains the logged argv with no process spawned. The argv is `status --no-pager -- sshd.service`, because the systemctl command adds those flags (run 3 fixed the expectation)
- [x] 4.3 Render the config with `execute: true`, reinstall, restart the unit, and wait for `receive loop starting` with `dry_run: false` before the execute requests
- [x] 4.4 Execute `systemctl status sshd.service`. Assert CLI exit 0, stored `succeeded` with `exit_code: 0`, and non-empty stdout in the record
- [x] 4.5 Execute `systemctl is-active sshd.service`. Assert CLI exit 0, stored `succeeded` with `exit_code: 0`
- [x] 4.6 For each request, assert `sqsh --json status <request_id>` returns the same status and exit code as the CLI's exit mapping
- [x] 4.7 Publish one request addressed to an unsubscribed host and assert no result is written within the wait timeout, and the daemon never logs it
- [x] 4.8 Restore the `execute: true` config on exit with a `trap`, whether the checks passed or failed. Stop the unit only with `--stop`
- [x] 4.9 Exit non-zero on any assertion failure and print the failing request ID and the relevant journal lines
- [x] 4.10 Fix the stranded-request cause and rerun until all checks pass. The cause was the receive path, fixed in `fix-receive-loss`. Run 4 passed all checks, with every stored record at `attempt: 1` and an empty queue afterwards

## 5. Verification record

- [x] 5.1 Write `openspec/changes/validate-remote-node/verification.md` with: date, region, target as `<host-id>`, the account as `<account-id>`, the node as `<node-ssh>` (with the port and user as placeholders), `uname -m` output, the binary's commit, each request ID with command, stored status, and exit code, and the pass/fail of each check in section 4. Do not write real access values into it
- [x] 5.2 Record the dependency check results from proposal "Dependencies on the node" (sudo, sshd active, AWS identity) as observed at the time of the run
- [x] 5.3 Teardown: not requested by the operator, so the resources and unit are kept as the record states. The confirmation guard was tested. Run `make teardown CONFIRM=yes` when wanted

## 6. Documentation

- [x] 6.1 Add a "Remote node test" section to `README.md` with the Makefile targets in order, a one-line description of each, the node dependencies, and a link to `verification.md`. Use placeholders only and point to `node.env.example`
- [x] 6.2 Leave the "What is NOT implemented yet" table as is. The remote-node section notes that this slice has been exercised on one real node

## 7. Hand-off

- [x] 7.1 Report to the operator that `add-walking-skeleton/verification.md` in git history contains the account ID and IAM user name, and get a decision on rewriting history before the repo is published (design, "Hand-off note"). Done: history rewritten and force-pushed to `main` with the operator's approval
