#!/bin/sh
# Installs the latest agentnet release: downloads it, verifies its checksum (and attestation if
# `gh` is available), installs it, then runs it.
set -eu

repo="purnachand-jaddu/agentnet"
base="${AGENTNET_RELEASE_BASE:-https://github.com/$repo/releases/latest/download}"
bin_dir="${AGENTNET_BIN:-$HOME/.local/bin}"

os=$(uname -s)
case "$os" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*)
		echo "install.sh: unsupported OS: $os" >&2
		exit 1
		;;
esac

arch=$(uname -m)
case "$arch" in
	x86_64) arch=amd64 ;;
	arm64|aarch64) arch=arm64 ;;
	*)
		echo "install.sh: unsupported architecture: $arch" >&2
		exit 1
		;;
esac

archive="agentnet_${os}_${arch}.tar.gz"
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT

# AGENTNET_RELEASE_BASE is for tests only: it may be a file:// URL, so --proto only guards the real base.
fetch() {
	if [ -n "${AGENTNET_RELEASE_BASE:-}" ]; then
		curl -fsSL -o "$2" "$1"
	else
		curl -fsSL --proto '=https' -o "$2" "$1"
	fi
}

fetch "$base/$archive" "$tmp_dir/$archive"
fetch "$base/checksums.txt" "$tmp_dir/checksums.txt"

expected=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp_dir/checksums.txt")
if [ -z "$expected" ]; then
	echo "install.sh: no checksum entry for $archive" >&2
	exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$tmp_dir/$archive" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
	actual=$(shasum -a 256 "$tmp_dir/$archive" | awk '{ print $1 }')
else
	echo "install.sh: need sha256sum or shasum to verify the download" >&2
	exit 1
fi

if [ "$expected" != "$actual" ]; then
	echo "install.sh: checksum mismatch for $archive" >&2
	exit 1
fi

# Checksums prove the download is intact; attestation (when gh is around) proves it came from this repo's CI.
if command -v gh >/dev/null 2>&1; then
	if ! gh attestation verify "$tmp_dir/$archive" --repo "$repo" >/dev/null 2>&1; then
		echo "install.sh: attestation verification failed for $archive" >&2
		exit 1
	fi
fi

tar -xzf "$tmp_dir/$archive" -C "$tmp_dir"
mkdir -p "$bin_dir"
cp "$tmp_dir/agentnet" "$bin_dir/agentnet"
chmod 0755 "$bin_dir/agentnet"

case ":$PATH:" in
	*":$bin_dir:"*) ;;
	*) echo "install.sh: $bin_dir is not on your PATH; add it, e.g. export PATH=\"$bin_dir:\$PATH\"" >&2 ;;
esac

# An openable /dev/tty lets `curl ... | sh -s -- up` still prompt for consent even though stdin is
# the pipe. `-r` can be true with no controlling terminal to back it, so open it for real in a
# subshell first rather than trusting the permission bit alone.
if (exec 3< /dev/tty) 2>/dev/null; then
	exec "$bin_dir/agentnet" "$@" < /dev/tty
else
	exec "$bin_dir/agentnet" "$@"
fi
