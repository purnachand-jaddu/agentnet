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

# fake_gh dir auth_status verify_status: writes a gh stub into dir/gh that logs its call to
# dir/gh.calls, then exits auth_status for `gh auth ...` or verify_status for `gh attestation ...`.
fake_gh() {
	dir="$1"
	auth_status="$2"
	verify_status="$3"
	cat > "$dir/gh" <<EOF
#!/bin/sh
echo "\$@" >> "$dir/gh.calls"
case "\$1" in
	auth) exit $auth_status ;;
	attestation) exit $verify_status ;;
	*) exit 1 ;;
esac
EOF
	chmod +x "$dir/gh"
}

# run_installer release_dir bin_dir fake_bin_dir tmp_root args...: runs install.sh against a fake
# release, with fake_bin_dir first on PATH (so our gh stub wins over any real gh), TMPDIR pointed
# at a test-owned dir (to check nothing survives a successful install), and no controlling tty
# where possible, so the `/dev/tty` branch is exercised without risking a hang.
run_installer() {
	rel="$1"
	bin="$2"
	fakebin="$3"
	tmproot="$4"
	shift 4
	if command -v setsid >/dev/null 2>&1; then
		env AGENTNET_RELEASE_BASE="file://$rel" AGENTNET_BIN="$bin" PATH="$fakebin:$PATH" TMPDIR="$tmproot" \
			setsid sh "$install_sh" "$@" < /dev/null
	else
		env AGENTNET_RELEASE_BASE="file://$rel" AGENTNET_BIN="$bin" PATH="$fakebin:$PATH" TMPDIR="$tmproot" \
			sh "$install_sh" "$@" < /dev/null
	fi
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# --- checksum matches, gh logged in and attests successfully: installs and runs, args pass
# through, and nothing is left behind in TMPDIR.
rel1="$work/rel1"
bin1="$work/bin1"
fake1="$work/fake1"
tmp1="$work/tmp1"
mkdir -p "$rel1" "$bin1" "$fake1" "$tmp1"
make_release "$rel1"
fake_gh "$fake1" 0 0
set +e
out=$(run_installer "$rel1" "$bin1" "$fake1" "$tmp1" up)
status=$?
set -e
if [ "$status" -eq 0 ]; then pass "match: install.sh exited 0"; else fail_test "match: install.sh exited $status"; fi
case "$out" in
	*"agentnet-dummy: up"*) pass "match: ran the installed binary with its args" ;;
	*) fail_test "match: expected dummy output with 'up', got: $out" ;;
esac
if [ -x "$bin1/agentnet" ]; then pass "match: binary installed"; else fail_test "match: binary not installed"; fi
if [ -f "$fake1/gh.calls" ]; then pass "match: gh attestation invoked"; else fail_test "match: gh was not called"; fi
if [ -e "$bin1/.agentnet.new" ]; then fail_test "match: temp install file left behind"; else pass "match: no leftover .agentnet.new"; fi
leftover=$(ls -A "$tmp1" 2>/dev/null || true)
if [ -z "$leftover" ]; then pass "match: no leftover temp dir in TMPDIR"; else fail_test "match: TMPDIR not empty: $leftover"; fi

# --- checksum mismatch: aborts, nothing installed.
rel2="$work/rel2"
bin2="$work/bin2"
fake2="$work/fake2"
tmp2="$work/tmp2"
mkdir -p "$rel2" "$bin2" "$fake2" "$tmp2"
make_release "$rel2"
echo "0000000000000000000000000000000000000000000000000000000000000000  $archive_name" > "$rel2/checksums.txt"
fake_gh "$fake2" 0 0
set +e
run_installer "$rel2" "$bin2" "$fake2" "$tmp2" up > "$work/mismatch.out" 2> "$work/mismatch.err"
status=$?
set -e
if [ "$status" -ne 0 ]; then pass "mismatch: install.sh aborted"; else fail_test "mismatch: install.sh exited 0"; fi
if [ ! -e "$bin2/agentnet" ]; then pass "mismatch: nothing installed"; else fail_test "mismatch: binary was installed anyway"; fi

# --- no checksum entry for this archive: aborts, nothing installed.
rel3="$work/rel3"
bin3="$work/bin3"
fake3="$work/fake3"
tmp3="$work/tmp3"
mkdir -p "$rel3" "$bin3" "$fake3" "$tmp3"
make_release "$rel3"
echo "" > "$rel3/checksums.txt"
fake_gh "$fake3" 0 0
set +e
run_installer "$rel3" "$bin3" "$fake3" "$tmp3" up > "$work/nosum.out" 2> "$work/nosum.err"
status=$?
set -e
if [ "$status" -ne 0 ]; then pass "no-checksum: install.sh aborted"; else fail_test "no-checksum: install.sh exited 0"; fi
if [ ! -e "$bin3/agentnet" ]; then pass "no-checksum: nothing installed"; else fail_test "no-checksum: binary was installed anyway"; fi

# --- gh installed but not logged in: skips attestation with a warning, install still succeeds.
rel4="$work/rel4"
bin4="$work/bin4"
fake4="$work/fake4"
tmp4="$work/tmp4"
mkdir -p "$rel4" "$bin4" "$fake4" "$tmp4"
make_release "$rel4"
fake_gh "$fake4" 1 0
set +e
run_installer "$rel4" "$bin4" "$fake4" "$tmp4" up > "$work/notlogged.out" 2> "$work/notlogged.err"
status=$?
set -e
if [ "$status" -eq 0 ]; then pass "gh-not-logged-in: install.sh exited 0"; else fail_test "gh-not-logged-in: install.sh exited $status"; fi
if [ -x "$bin4/agentnet" ]; then pass "gh-not-logged-in: binary installed anyway"; else fail_test "gh-not-logged-in: binary not installed"; fi
if grep -q "skipping attestation: gh not logged in" "$work/notlogged.err"; then
	pass "gh-not-logged-in: warning printed"
else
	fail_test "gh-not-logged-in: no warning in stderr: $(cat "$work/notlogged.err")"
fi

# --- gh logged in but attestation verify fails: aborts, nothing installed.
rel5="$work/rel5"
bin5="$work/bin5"
fake5="$work/fake5"
tmp5="$work/tmp5"
mkdir -p "$rel5" "$bin5" "$fake5" "$tmp5"
make_release "$rel5"
fake_gh "$fake5" 0 1
set +e
run_installer "$rel5" "$bin5" "$fake5" "$tmp5" up > "$work/ghfail.out" 2> "$work/ghfail.err"
status=$?
set -e
if [ "$status" -ne 0 ]; then pass "gh-verify-fail: install.sh aborted"; else fail_test "gh-verify-fail: install.sh exited 0"; fi
if [ ! -e "$bin5/agentnet" ]; then pass "gh-verify-fail: nothing installed"; else fail_test "gh-verify-fail: binary was installed anyway"; fi

if [ "$fail" -ne 0 ]; then
	echo "install_test.sh: FAILED" >&2
	exit 1
fi
echo "install_test.sh: all tests passed"
