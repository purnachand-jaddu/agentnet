#!/bin/sh
# Hermetic tests for install.sh: temp dirs only, a fake release and a fake gh, never touching
# ~/.local/bin, ~/.agentnet, ~/.claude or ~/.codex.
set -eu

script_dir=$(cd "$(dirname "$0")" && pwd)
install_sh="$script_dir/install.sh"

fail=0
pass() { echo "ok - $1"; }
fail_test() {
	echo "not ok - $1" >&2
	fail=1
}

os=$(uname -s)
case "$os" in
	Darwin) rel_os=darwin ;;
	Linux) rel_os=linux ;;
	*)
		echo "install_test.sh: unsupported OS $os" >&2
		exit 1
		;;
esac
arch=$(uname -m)
case "$arch" in
	x86_64) rel_arch=amd64 ;;
	arm64|aarch64) rel_arch=arm64 ;;
	*)
		echo "install_test.sh: unsupported arch $arch" >&2
		exit 1
		;;
esac
archive_name="agentnet_${rel_os}_${rel_arch}.tar.gz"

checksum_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{ print $1 }'
	else
		shasum -a 256 "$1" | awk '{ print $1 }'
	fi
}

# make_release dir: builds a dummy "agentnet" binary (a shell script echoing its args) plus a
# matching archive and checksums.txt under dir, as install.sh expects at AGENTNET_RELEASE_BASE.
make_release() {
	dir="$1"
	build="$dir/build"
	mkdir -p "$build"
	cat > "$build/agentnet" <<'DUMMY'
#!/bin/sh
echo "agentnet-dummy: $*"
DUMMY
	chmod +x "$build/agentnet"
	(cd "$build" && tar -czf "$dir/$archive_name" agentnet)
	sum=$(checksum_of "$dir/$archive_name")
	echo "$sum  $archive_name" > "$dir/checksums.txt"
	rm -rf "$build"
}

# fake_gh dir status: writes a gh stub into dir/gh that exits status and logs its call to dir/gh.calls.
fake_gh() {
	dir="$1"
	status="$2"
	cat > "$dir/gh" <<EOF
#!/bin/sh
echo "\$@" >> "$dir/gh.calls"
exit $status
EOF
	chmod +x "$dir/gh"
}

# run_installer release_dir bin_dir fake_bin_dir args...: runs install.sh against a fake release,
# with fake_bin_dir first on PATH (so our gh stub wins over any real gh) and no controlling tty
# where possible, so the `[ -r /dev/tty ]` branch is exercised without risking a hang.
run_installer() {
	rel="$1"
	bin="$2"
	fakebin="$3"
	shift 3
	if command -v setsid >/dev/null 2>&1; then
		env AGENTNET_RELEASE_BASE="file://$rel" AGENTNET_BIN="$bin" PATH="$fakebin:$PATH" \
			setsid sh "$install_sh" "$@" < /dev/null
	else
		env AGENTNET_RELEASE_BASE="file://$rel" AGENTNET_BIN="$bin" PATH="$fakebin:$PATH" \
			sh "$install_sh" "$@" < /dev/null
	fi
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# --- checksum matches, gh attests successfully: installs and runs, passing args through.
rel1="$work/rel1"
bin1="$work/bin1"
fake1="$work/fake1"
mkdir -p "$rel1" "$bin1" "$fake1"
make_release "$rel1"
fake_gh "$fake1" 0
set +e
out=$(run_installer "$rel1" "$bin1" "$fake1" up)
status=$?
set -e
if [ "$status" -eq 0 ]; then pass "match: install.sh exited 0"; else fail_test "match: install.sh exited $status"; fi
case "$out" in
	*"agentnet-dummy: up"*) pass "match: ran the installed binary with its args" ;;
	*) fail_test "match: expected dummy output with 'up', got: $out" ;;
esac
if [ -x "$bin1/agentnet" ]; then pass "match: binary installed"; else fail_test "match: binary not installed"; fi
if [ -f "$fake1/gh.calls" ]; then pass "match: gh attestation invoked"; else fail_test "match: gh was not called"; fi

# --- checksum mismatch: aborts, nothing installed.
rel2="$work/rel2"
bin2="$work/bin2"
fake2="$work/fake2"
mkdir -p "$rel2" "$bin2" "$fake2"
make_release "$rel2"
echo "0000000000000000000000000000000000000000000000000000000000000000  $archive_name" > "$rel2/checksums.txt"
fake_gh "$fake2" 0
set +e
run_installer "$rel2" "$bin2" "$fake2" up > "$work/mismatch.out" 2> "$work/mismatch.err"
status=$?
set -e
if [ "$status" -ne 0 ]; then pass "mismatch: install.sh aborted"; else fail_test "mismatch: install.sh exited 0"; fi
if [ ! -e "$bin2/agentnet" ]; then pass "mismatch: nothing installed"; else fail_test "mismatch: binary was installed anyway"; fi

# --- gh attestation fails: aborts, nothing installed.
rel3="$work/rel3"
bin3="$work/bin3"
fake3="$work/fake3"
mkdir -p "$rel3" "$bin3" "$fake3"
make_release "$rel3"
fake_gh "$fake3" 1
set +e
run_installer "$rel3" "$bin3" "$fake3" up > "$work/ghfail.out" 2> "$work/ghfail.err"
status=$?
set -e
if [ "$status" -ne 0 ]; then pass "gh-fail: install.sh aborted"; else fail_test "gh-fail: install.sh exited 0"; fi
if [ ! -e "$bin3/agentnet" ]; then pass "gh-fail: nothing installed"; else fail_test "gh-fail: binary was installed anyway"; fi

if [ "$fail" -ne 0 ]; then
	echo "install_test.sh: FAILED" >&2
	exit 1
fi
echo "install_test.sh: all tests passed"
