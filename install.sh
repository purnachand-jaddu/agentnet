#!/bin/sh
# Installs the latest agentnet release: downloads it, verifies its checksum (and attestation if
# `gh` is installed and logged in), installs it, then runs it.
set -eu

main() {
	repo="purnachand-jaddu/agentnet"
	base="${AGENTNET_RELEASE_BASE:-https://github.com/$repo/releases/latest/download}"
	bin_dir="${AGENTNET_BIN:-$HOME/.local/bin}"

	if [ -n "${AGENTNET_RELEASE_BASE:-}" ]; then
		echo "install.sh: using AGENTNET_RELEASE_BASE=$AGENTNET_RELEASE_BASE (testing only)" >&2
	fi

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

	# --proto always allows https (the real base) and file (AGENTNET_RELEASE_BASE in tests); it
	# never allows a plain-http or downgraded base, even when AGENTNET_RELEASE_BASE is set.
	fetch() {
		curl -fsSL --proto '=https,file' -o "$2" "$1"
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

	# Checksums prove the download is intact; attestation (gh installed and logged in) proves it
	# came from this repo's own release workflow. Not logged in isn't a failure: skip it and say so.
	if command -v gh >/dev/null 2>&1; then
		if gh auth status >/dev/null 2>&1; then
			if ! gh attestation verify "$tmp_dir/$archive" --repo "$repo" >/dev/null; then
				echo "install.sh: attestation verification failed for $archive" >&2
				exit 1
			fi
		else
			echo "install.sh: skipping attestation: gh not logged in" >&2
		fi
	fi

	tar -xzf "$tmp_dir/$archive" -C "$tmp_dir"
	mkdir -p "$bin_dir"
	# Install via a temp file + rename: an in-place overwrite can hit a running agentnet (the
	# service `agentnet setup` installs runs this same path), which fails on Linux and can kill a
	# running process on macOS. A rename onto the same filesystem is atomic and leaves it alone.
	cp "$tmp_dir/agentnet" "$bin_dir/.agentnet.new"
	chmod 0755 "$bin_dir/.agentnet.new"
	mv -f "$bin_dir/.agentnet.new" "$bin_dir/agentnet"

	case ":$PATH:" in
		*":$bin_dir:"*) ;;
		*) echo "install.sh: $bin_dir is not on your PATH; add it, e.g. export PATH=\"$bin_dir:\$PATH\"" >&2 ;;
	esac

	rm -rf "$tmp_dir"
	trap - EXIT

	# An openable /dev/tty lets `curl ... | sh -s -- up` still prompt for consent even though stdin
	# is the pipe. `-r` can be true with no controlling terminal to back it, so open it for real in
	# a subshell first rather than trusting the permission bit alone.
	if (exec 3< /dev/tty) 2>/dev/null; then
		exec "$bin_dir/agentnet" "$@" < /dev/tty
	else
		exec "$bin_dir/agentnet" "$@"
	fi
}

main "$@"
