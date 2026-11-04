#!/bin/sh
# Hosted installer for the agentparley command-line tool (Linux and macOS). Installs only the binary: no sudo, no
# config, no service. To connect a Linux server as a computer agents can use, see packaging/linux/install.sh.
#
#   curl -fsSL https://get.agentparley.ai/cli/install.sh | sh
#
# Overrides are environment variables: AGENTPARLEY_UPDATE_SERVER, AGENTPARLEY_VERSION.
#
# Everything lives inside functions and the last line is `main "$@"`, so a truncated `curl | sh` stream never
# executes a half-written script.
set -eu

DEFAULT_UPDATE_SERVER="https://get.agentparley.ai"

fail() {
	echo "$@" >&2
	exit 1
}

detect_os() {
	case "$(uname -s)" in
		Linux) echo linux ;;
		Darwin) echo darwin ;;
		*) fail "On Windows download agentparley-windows-amd64.exe from ${UPDATE_SERVER}/releases/<version>/ (see https://get.agentparley.ai)." ;;
	esac
}

detect_arch() {
	case "$(uname -m)" in
		x86_64 | amd64) echo amd64 ;;
		arm64 | aarch64) echo arm64 ;;
		*) fail "Unsupported CPU: $(uname -m)" ;;
	esac
}

verify_checksum() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum -c "$1"
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 -c "$1"
	else
		fail "Neither sha256sum nor shasum is available to verify the download."
	fi
}

main() {
	UPDATE_SERVER="${AGENTPARLEY_UPDATE_SERVER:-$DEFAULT_UPDATE_SERVER}"
	os="$(detect_os)"
	arch="$(detect_arch)"

	version="${AGENTPARLEY_VERSION:-$(curl -fsSL "$UPDATE_SERVER/latest-version")}"
	version="$(echo "$version" | tr -d '[:space:]')"
	echo "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || fail "Not a version number: $version"

	name="agentparley-$os-$arch"
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' EXIT

	echo "Downloading agentparley $version for $os/$arch..."
	curl -fsSL -o "$tmp/$name" "$UPDATE_SERVER/releases/$version/$name"
	curl -fsSL -o "$tmp/$name.sha256" "$UPDATE_SERVER/releases/$version/$name.sha256"
	(cd "$tmp" && verify_checksum "$name.sha256") || fail "Checksum mismatch: not installing."

	if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
		bin_dir="/usr/local/bin"
	else
		bin_dir="$HOME/.local/bin"
		mkdir -p "$bin_dir"
	fi
	install -m 0755 "$tmp/$name" "$bin_dir/agentparley"

	case ":$PATH:" in
		*":$bin_dir:"*) ;;
		*) echo "Add $bin_dir to your PATH: export PATH=\"$bin_dir:\$PATH\"" ;;
	esac
	echo "Installed agentparley $version. Sign in with: agentparley auth login"
}

main "$@"
