#!/bin/sh
# Hosted, argument-free installer for the agentparley-tunnel daemon.
#
#   curl -fsSL https://raw.githubusercontent.com/AgentParley/AgentParley-tunnel/main/packaging/linux/install.sh | sudo sh
#
# A piped script has no argv, so every override is an environment variable — see the README for the full list
# (AGENTPARLEY_TUNNEL_USER, AGENTPARLEY_TUNNEL_API_SERVER, AGENTPARLEY_TUNNEL_EGRESS_SERVER,
# AGENTPARLEY_TUNNEL_UPDATE_SERVER, AGENTPARLEY_TUNNEL_BINARY). Deliberately does NOT run `agentparley-tunnel login` —
# the device-grant approval is an interactive, separate step (install, THEN login, THEN start).
#
# POSIX sh on purpose (no bash-isms): plain [ ] tests, no arrays, no [[ ]]. Everything below lives inside a
# function and the very last line is `main "$@"` — a `curl | sh` stream cut off mid-transfer then parses to a
# syntactically valid PREFIX that never reaches main and does nothing, rather than executing a half-written
# script. `curl -f` alone only catches a bad HTTP status, not a truncated body, so this is the actual mitigation.
set -eu

BINARY_NAME="agentparley-tunnel"
INSTALL_BIN_DIR="/usr/local/bin"
CONFIG_DIR="/etc/agentparley-tunnel"
CONFIG_FILE="$CONFIG_DIR/config.yaml"
STATE_DIR="/var/lib/agentparley-tunnel"
UNIT_DIR="/etc/systemd/system"
UNIT_NAME="agentparley-tunnel.service"

DEFAULT_API_SERVER="https://app.agentparley.ai"
DEFAULT_EGRESS_SERVER="ssh-tunnel.agentparley.ai:443"
DEFAULT_UPDATE_SERVER="https://tunnel-app.agentparley.ai"   # where the SIGNED BINARY is fetched from
INSTALL_SCRIPT_URL="https://raw.githubusercontent.com/AgentParley/AgentParley-tunnel/main/packaging/linux/install.sh"   # where THIS script is served (the public repo, auditable)

log() {
	echo "$@"
}

fail() {
	echo "$@" >&2
	exit 1
}

check_root() {
	if [ "$(id -u)" -ne 0 ]; then
		fail "install.sh must run as root: curl -fsSL $INSTALL_SCRIPT_URL | sudo sh"
	fi
}

check_systemd() {
	if [ ! -d /run/systemd/system ]; then
		fail "this installer supports Linux with systemd only — no systemd was found at /run/systemd/system."
	fi
}

check_os() {
	OS_NAME="$(uname -s)"
	if [ "$OS_NAME" != "Linux" ]; then
		fail "this installer supports Linux only (found: $OS_NAME) — there is no macOS or Windows build of agentparley-tunnel."
	fi
}

detect_arch() {
	MACHINE_ARCH="$(uname -m)"
	case "$MACHINE_ARCH" in
	x86_64)
		GOARCH="amd64"
		;;
	aarch64 | arm64)
		GOARCH="arm64"
		;;
	*)
		fail "unsupported CPU architecture: $MACHINE_ARCH — agentparley-tunnel is built for x86_64 and aarch64/arm64 only."
		;;
	esac
}

resolve_run_as_user() {
	RUN_AS_USER="${AGENTPARLEY_TUNNEL_USER:-${SUDO_USER:-}}"
	if [ -z "$RUN_AS_USER" ] || [ "$RUN_AS_USER" = "root" ]; then
		echo "no run-as user could be determined (ran as root directly, with no SUDO_USER) — the daemon has no" >&2
		echo "privilege to switch users, so it must run as, and only as, the account whose behalf commands run on." >&2
		fail "re-run as: curl -fsSL $INSTALL_SCRIPT_URL | sudo AGENTPARLEY_TUNNEL_USER=<name> sh"
	fi
}

resolve_settings() {
	API_SERVER="${AGENTPARLEY_TUNNEL_API_SERVER:-$DEFAULT_API_SERVER}"
	EGRESS_SERVER="${AGENTPARLEY_TUNNEL_EGRESS_SERVER:-$DEFAULT_EGRESS_SERVER}"
	UPDATE_SERVER="${AGENTPARLEY_TUNNEL_UPDATE_SERVER:-$DEFAULT_UPDATE_SERVER}"
	LOCAL_BINARY="${AGENTPARLEY_TUNNEL_BINARY:-}"
}

fetch() {
	URL="$1"
	OUT="$2"
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "$OUT" "$URL"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$OUT" "$URL"
	else
		fail "neither curl nor wget is installed — install one of them and re-run."
	fi
}

download_and_verify() {
	WORKDIR="$(mktemp -d)"
	trap 'rm -rf "$WORKDIR"' EXIT

	VERSION_FILE="$WORKDIR/latest-version"
	fetch "$UPDATE_SERVER/latest-version" "$VERSION_FILE"
	VERSION="$(cat "$VERSION_FILE")"
	if ! echo "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
		fail "unexpected content at $UPDATE_SERVER/latest-version — expected a version like 1.2.3, got: $VERSION"
	fi

	RELEASE_BINARY_NAME="${BINARY_NAME}-linux-${GOARCH}"
	BINARY_PATH="$WORKDIR/$RELEASE_BINARY_NAME"
	fetch "$UPDATE_SERVER/releases/$VERSION/$RELEASE_BINARY_NAME" "$BINARY_PATH"
	fetch "$UPDATE_SERVER/releases/$VERSION/$RELEASE_BINARY_NAME.sha256" "$BINARY_PATH.sha256"

	if ! (cd "$WORKDIR" && sha256sum -c "$RELEASE_BINARY_NAME.sha256") >/dev/null 2>&1; then
		fail "checksum verification failed for $UPDATE_SERVER/releases/$VERSION/$RELEASE_BINARY_NAME — aborting before anything was installed."
	fi

	BINARY_SOURCE="$BINARY_PATH"
	INSTALLED_VERSION="$VERSION"
}

use_local_binary() {
	if [ ! -f "$LOCAL_BINARY" ]; then
		fail "AGENTPARLEY_TUNNEL_BINARY=$LOCAL_BINARY does not exist."
	fi
	BINARY_SOURCE="$LOCAL_BINARY"
	INSTALLED_VERSION="local ($LOCAL_BINARY)"
}

ensure_run_as_user() {
	if ! id "$RUN_AS_USER" >/dev/null 2>&1; then
		log "creating system user $RUN_AS_USER"
		useradd --system --create-home --shell /bin/bash "$RUN_AS_USER"
	fi
}

install_binary() {
	STAGED_BINARY="$INSTALL_BIN_DIR/.$BINARY_NAME.new"
	install -m 0755 -o root -g root "$BINARY_SOURCE" "$STAGED_BINARY"
	mv "$STAGED_BINARY" "$INSTALL_BIN_DIR/$BINARY_NAME"
}

write_config() {
	install -d -m 0755 -o root -g root "$CONFIG_DIR"
	if [ -f "$CONFIG_FILE" ]; then
		log "config already exists at $CONFIG_FILE — leaving it as-is"
		return
	fi

	cat >"$CONFIG_FILE" <<EOF
server:
  api: $API_SERVER
  egress: $EGRESS_SERVER
run_as: $RUN_AS_USER
read_only: false
allow_commands: []
deny_commands: []
enabled: true
auto_update: true
EOF
	if [ "$UPDATE_SERVER" != "$DEFAULT_UPDATE_SERVER" ]; then
		echo "update_server: $UPDATE_SERVER" >>"$CONFIG_FILE"
	fi
	chmod 0644 "$CONFIG_FILE"
}

write_state_dir() {
	install -d -m 0700 -o "$RUN_AS_USER" -g "$RUN_AS_USER" "$STATE_DIR"
}

write_unit() {
	cat >"$UNIT_DIR/$UNIT_NAME" <<EOF
[Unit]
Description=AgentParley tunnel daemon
Documentation=https://github.com/agentparley/tunnel
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# install.sh writes this to the run-as user it resolved (matching config.yaml's run_as — the daemon refuses to
# start on a mismatch; see internal/shellrun.VerifyMatchesProcess).
User=$RUN_AS_USER
ExecStart=/usr/local/bin/agentparley-tunnel start
Restart=always
RestartSec=5

# A command can run up to SshLimits.MaxCommandTimeoutSeconds (600s); the daemon's own graceful-shutdown path
# (client.serve's SIGTERM handling) needs to outlive the longest in-flight operation, not fight it — 660s gives a
# full 600s command 60s of margin before systemd escalates to SIGKILL.
TimeoutStopSec=660

# The default (control-group) would SIGKILL every descendant on a daemon restart or stop — including a
# \`nohup … &\` job the agent launched via RunSshBackgroundCommandUseCase specifically so it would SURVIVE the
# command that spawned it. \`process\` kills only this unit's own main process, leaving detached children alone.
# Real sshd sets the same value for the identical reason: reproducing sshd's contract extends to this, not just
# the environment variables (see internal/shellrun).
KillMode=process

NoNewPrivileges=yes

# Bounds the whole activation, ExecStartPre included. systemd's default is 90s; a ~20 MB binary download on a
# slow link (DSL/cellular) can exceed that, and a timed-out ExecStartPre aborts the WHOLE unit — the \`-\` prefix
# does NOT excuse a timeout, only a non-zero exit — so Restart=always would then loop the download forever and
# the tunnel would never start. 420s sits safely above self-update's own internal deadlines below.
TimeoutStartSec=420
ExecStartPre=-+/usr/local/bin/agentparley-tunnel self-update

# NO filesystem sandboxing directives here on purpose (no ProtectSystem, PrivateTmp, ProtectHome, ReadOnlyPaths,
# …). This daemon's entire job is running commands and reading/writing files as an arbitrary path the AGENT
# chooses on behalf of the box's owner — /home/*, /srv/*, /var/*, wherever the owner's workload lives. Any of the
# standard sandboxing directives would silently break exactly the feature this service exists to provide (a file
# write failing inside a systemd-namespaced private /tmp or a read-only /usr looks, from the agent's side, like a
# permissions bug on the box, not a policy this unit imposed). The OS's own user/permission model — the SAME
# model real SSH access is bound by — is the actual boundary; config.yaml's read_only/allow_commands/deny_commands
# are the owner's levers on top of that, not systemd's.

[Install]
WantedBy=multi-user.target
EOF
	chmod 0644 "$UNIT_DIR/$UNIT_NAME"
}

enable_service_and_report() {
	WAS_ACTIVE=false
	if systemctl is-active --quiet "$UNIT_NAME" 2>/dev/null; then
		WAS_ACTIVE=true
	fi

	systemctl daemon-reload
	systemctl enable "$UNIT_NAME"

	echo ""
	echo "Installed agentparley-tunnel ($INSTALLED_VERSION). Next steps:"
	echo "  1. sudo -u $RUN_AS_USER $INSTALL_BIN_DIR/$BINARY_NAME login"
	echo "  2. sudo systemctl start $UNIT_NAME"

	if [ "$WAS_ACTIVE" = "true" ]; then
		echo ""
		echo "The service is currently running the previous binary — run 'sudo systemctl restart $UNIT_NAME' to pick up this install."
	fi
}

main() {
	check_root
	check_os
	check_systemd
	detect_arch
	resolve_run_as_user
	resolve_settings

	if [ -n "$LOCAL_BINARY" ]; then
		use_local_binary
	else
		download_and_verify
	fi

	ensure_run_as_user
	install_binary
	write_config
	write_state_dir
	write_unit
	enable_service_and_report
}

main "$@"
