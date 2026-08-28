#!/bin/sh
# The exact inverse of install.sh, hosted the same way it is:
#
#   curl -fsSL https://raw.githubusercontent.com/AgentParley/AgentParley-tunnel/main/packaging/linux/uninstall.sh | sudo sh
#
# Does NOT delete the run-as user by default (it may be a pre-existing account with a home directory the
# operator wants kept) — set AGENTPARLEY_TUNNEL_DELETE_USER=true (or pass --delete-user if run from a downloaded
# copy) to remove a user this install.sh created.
#
# POSIX sh, same truncation-proofing as install.sh: everything lives in a function, last line is `main "$@"`.
set -eu

BINARY_NAME="agentparley-tunnel"
INSTALL_BIN_DIR="/usr/local/bin"
CONFIG_DIR="/etc/agentparley-tunnel"
STATE_DIR="/var/lib/agentparley-tunnel"
UNIT_DIR="/etc/systemd/system"
UNIT_NAME="agentparley-tunnel.service"

fail() {
	echo "$@" >&2
	exit 1
}

check_root() {
	if [ "$(id -u)" -ne 0 ]; then
		fail "uninstall.sh must run as root (sudo)."
	fi
}

parse_args() {
	DELETE_USER="${AGENTPARLEY_TUNNEL_DELETE_USER:-false}"
	for arg in "$@"; do
		case "$arg" in
		--delete-user)
			DELETE_USER=true
			;;
		*)
			fail "unknown argument: $arg"
			;;
		esac
	done
}

resolve_run_as_user() {
	RUN_AS_USER=""
	if [ -f "$CONFIG_DIR/config.yaml" ]; then
		RUN_AS_USER="$(sed -n 's/^run_as: *//p' "$CONFIG_DIR/config.yaml" | head -n1)"
	fi
}

stop_and_disable() {
	if systemctl is-active --quiet "$UNIT_NAME" 2>/dev/null; then
		systemctl stop "$UNIT_NAME"
	fi
	if systemctl is-enabled --quiet "$UNIT_NAME" 2>/dev/null; then
		systemctl disable "$UNIT_NAME"
	fi
	rm -f "$UNIT_DIR/$UNIT_NAME"
	systemctl daemon-reload
}

remove_files() {
	rm -f "$INSTALL_BIN_DIR/$BINARY_NAME"
	rm -rf "$CONFIG_DIR"
	rm -rf "$STATE_DIR"
}

delete_user_if_requested() {
	if [ "$DELETE_USER" = "true" ] && [ -n "$RUN_AS_USER" ]; then
		if id "$RUN_AS_USER" >/dev/null 2>&1; then
			userdel -r "$RUN_AS_USER" || echo "warning: failed to delete user $RUN_AS_USER" >&2
		fi
	fi
}

main() {
	check_root
	parse_args "$@"
	resolve_run_as_user
	stop_and_disable
	remove_files
	delete_user_if_requested
	echo "Uninstalled."
}

main "$@"
