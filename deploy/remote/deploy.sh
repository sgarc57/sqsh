#!/usr/bin/env bash
# Install and manage sqshd on the remote node over SSH.
#
# Usage: deploy/remote/deploy.sh <install|start|stop|restart|enable|disable|status>
#
# install renders the config with execute: false (dry run), so a fresh install never runs
# commands until the e2e check, or an operator, turns execution on. Run `make build-linux-arm64`
# first; the Makefile's `deploy` target does that for install.

source "$(dirname "$0")/lib.sh"

action="${1:-}"

case "$action" in
install)
	[ -f "$ROOT/bin/linux-arm64/sqshd" ] || die "bin/linux-arm64/sqshd missing; run make build-linux-arm64"
	require_node_sudo
	resolve_aws
	install_node false
	;;
start | stop | restart | enable | disable)
	require_node_sudo
	remote "sudo systemctl $action sqshd"
	echo "sqshd: $action done"
	;;
status)
	remote 'systemctl --no-pager status sqshd || true; echo; journalctl -u sqshd -n 50 --no-pager || true'
	;;
*)
	die "usage: deploy.sh <install|start|stop|restart|enable|disable|status>"
	;;
esac
