#!/usr/bin/env bash
# End-to-end check against the real node, run from the operator's workstation. The sqsh
# CLI publishes, the daemon on the node receives and executes, and the CLI and DynamoDB
# agree on the outcome.
#
# Phases:
#   1. dry run (execute: false): accepted, recorded succeeded, argv logged, nothing spawned
#   2. execute the read-only systemctl queries on sshd.service
#   3. a request for an unsubscribed host: no result, and the daemon never sees it
#
# Usage: deploy/remote/e2e.sh [--stop]     --stop leaves the unit stopped afterwards
# Requires: make provision, make build (bin/sqsh), make build-linux-arm64 (the Makefile
# e2e target does both). The node is restored to execute: true on exit, pass or fail.

source "$(dirname "$0")/lib.sh"

need jq
need aws

STOP_AFTER=no
for arg in "$@"; do
	case "$arg" in
	--stop) STOP_AFTER=yes ;;
	*) die "unknown argument: $arg" ;;
	esac
done

SQSH="$ROOT/bin/sqsh"
[ -x "$SQSH" ] || die "bin/sqsh missing; run make build"
[ -f "$ROOT/bin/linux-arm64/sqshd" ] || die "bin/linux-arm64/sqshd missing; run make build-linux-arm64"

require_node_sudo
resolve_aws
export AWS_REGION="$REGION" SQSH_TOPIC_ARN="$TOPIC_ARN" SQSH_RESULTS_TABLE="$RESULTS_TABLE"

ERR_LOG=$(mktemp)
failures=0

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }

# expect <description> <actual> <expected>
expect() {
	if [ "$2" = "$3" ]; then pass "$1 ($2)"; else fail "$1: got '$2', want '$3'"; fi
}

# The CLI exit code the contract (SPEC §16) assigns to a terminal status.
expected_cli_rc() {
	case "$1" in
	succeeded) echo 0 ;;
	failed | timed_out) echo 1 ;;
	rejected | expired) echo 3 ;;
	*) echo unknown ;;
	esac
}

journal_since() {
	remote "journalctl -u sqshd --since '$1' -o cat --no-pager"
}

# Wait until the daemon has logged its startup with the expected dry_run value.
wait_for_daemon() { # wait_for_daemon <dry_run: true|false> <since>
	local i seen=""
	for i in $(seq 1 30); do
		seen=$(journal_since "$2" 2>/dev/null |
			jq -R -r 'fromjson? | select(.msg == "receive loop starting") | .dry_run' | tail -n1)
		[ "$seen" = "$1" ] && return 0
		sleep 1
	done
	die "daemon did not start with dry_run=$1 within 30s; last seen: '${seen:-none}'"
}

# Run the CLI with --json and keep what it reported. Sets CLI_RC, OUT, RID, STATUS, EXIT, STDOUT.
publish() { # publish <label> <sqsh args...>
	local label=$1 out rc
	shift
	echo "-- $label: sqsh --json $*"
	if out=$("$SQSH" --json "$@" 2>>"$ERR_LOG"); then rc=0; else rc=$?; fi
	CLI_RC=$rc
	OUT=$out
	RID=$(jq -r '.request_id // empty' <<<"$out" 2>/dev/null || true)
	STATUS=$(jq -r '.status // empty' <<<"$out" 2>/dev/null || true)
	EXIT=$(jq -r '.exit_code // empty' <<<"$out" 2>/dev/null || true)
	STDOUT=$(jq -r '.stdout // empty' <<<"$out" 2>/dev/null || true)
	echo "   request_id: ${RID:-none}  cli_exit: $rc  status: ${STATUS:-none}"
}

# Read the record back from the store and compare it with what the CLI reported.
check_stored() { # check_stored <label>
	local stored
	if ! stored=$("$SQSH" --json status "$RID" 2>>"$ERR_LOG"); then
		fail "$1: sqsh status could not read the record"
		return
	fi
	expect "$1: stored status matches CLI" "$(jq -r '.status // empty' <<<"$stored")" "$STATUS"
	expect "$1: stored exit_code matches CLI" "$(jq -r '.exit_code // empty' <<<"$stored")" "$EXIT"
	expect "$1: CLI exit matches stored status" "$CLI_RC" "$(expected_cli_rc "$STATUS")"
}

restore() {
	local rc=$?
	echo "== restoring execute: true on the node"
	if install_node true && remote "sudo systemctl restart sqshd"; then
		:
	else
		echo "warning: could not restore the node config; check it by hand" >&2
	fi
	if [ "$STOP_AFTER" = yes ]; then
		remote "sudo systemctl stop sqshd" || echo "warning: could not stop sqshd" >&2
	fi
	rm -f "$ERR_LOG"
	exit "$rc"
}
trap restore EXIT

# Phase 1: dry run.
echo "== phase 1: dry run (execute: false)"
install_node false
since=$(remote 'date "+%Y-%m-%d %H:%M:%S"')
remote "sudo systemctl restart sqshd"
wait_for_daemon true "$since"

publish "dry run" --wait-timeout 60s systemctl "$HOST_ID" status sshd.service
[ -n "$RID" ] || die "no request_id from the CLI: $(cat "$ERR_LOG")"
expect "dry run: CLI exit" "$CLI_RC" 0
expect "dry run: status" "$STATUS" succeeded
check_stored "dry run"
argv_logged=$(journal_since "$since" |
	jq -R -r --arg r "$RID" 'fromjson? | select(.request_id == $r and .msg == "dry run: not executing") | .argv[1:] | join(" ")' |
	tail -n1)
# The systemctl command adds --no-pager and -- (commands/systemctl.go), so the logged argv
# is the verb, those flags, and the unit. The -- stops a unit name being read as a flag.
expect "dry run: logged argv (after argv[0])" "$argv_logged" "status --no-pager -- sshd.service"
case "$STDOUT" in
*"dry run: would execute"*) pass "dry run: nothing spawned, stdout says so" ;;
*) fail "dry run: stdout does not say 'dry run: would execute': '$STDOUT'" ;;
esac

# Phase 2: execute the read-only queries.
echo "== phase 2: execute (read-only systemctl)"
install_node true
since=$(remote 'date "+%Y-%m-%d %H:%M:%S"')
remote "sudo systemctl restart sqshd"
wait_for_daemon false "$since"

publish "status sshd" --wait-timeout 60s systemctl "$HOST_ID" status sshd.service
expect "status sshd: CLI exit" "$CLI_RC" 0
expect "status sshd: status" "$STATUS" succeeded
expect "status sshd: exit_code" "$EXIT" 0
[ -n "$STDOUT" ] && pass "status sshd: stdout captured" || fail "status sshd: stdout is empty"
check_stored "status sshd"

publish "is-active sshd" --wait-timeout 60s systemctl "$HOST_ID" is-active sshd.service
expect "is-active sshd: CLI exit" "$CLI_RC" 0
expect "is-active sshd: status" "$STATUS" succeeded
expect "is-active sshd: exit_code" "$EXIT" 0
expect "is-active sshd: stdout" "$STDOUT" active
check_stored "is-active sshd"

# Phase 3: a host with no subscription. The CLI gives up waiting; no record may appear and
# the daemon must never log the request.
echo "== phase 3: unsubscribed host"
since=$(remote 'date "+%Y-%m-%d %H:%M:%S"')
publish "unsubscribed host" --wait-timeout 30s systemctl web99 status sshd.service
expect "unsubscribed: CLI exit (gave up)" "$CLI_RC" 4
expect "unsubscribed: status reported" "$STATUS" unknown
if "$SQSH" --json status "$RID" >/dev/null 2>>"$ERR_LOG"; then
	s_rc=0
else
	s_rc=$?
fi
expect "unsubscribed: no record in the store" "$s_rc" 4
seen=$(journal_since "$since" | grep -c "$RID" || true)
expect "unsubscribed: daemon never logged the request" "$seen" 0

echo "== summary"
if [ "$failures" -eq 0 ]; then
	echo "all checks passed"
else
	echo "$failures check(s) failed. Recent daemon log:"
	remote 'journalctl -u sqshd -n 40 --no-pager -o cat' | tail -n 40
fi
exit $((failures > 0))
