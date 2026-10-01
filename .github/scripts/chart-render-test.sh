#!/usr/bin/env bash
# Render-level tests for the krakend-operator Helm chart: each case renders
# the chart with `helm template` and asserts on the output.
# Run from the repository root: bash .github/scripts/chart-render-test.sh
set -euo pipefail

CHART=charts/krakend-operator
failures=0
workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

render() { helm template t "$CHART" --namespace krakend-operator-system "$@"; }

pass() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s\n' "$1"
	failures=$((failures + 1))
}

# expect_contains NAME NEEDLE [helm args...]: the render succeeds and
# contains NEEDLE.
expect_contains() {
	local name=$1 needle=$2 out
	shift 2
	if ! out=$(render "$@" 2>&1); then
		fail "$name (helm template failed: $out)"
		return
	fi
	if grep -qF -- "$needle" <<<"$out"; then pass "$name"; else fail "$name"; fi
}

# expect_absent NAME NEEDLE [helm args...]: the render succeeds and does not
# contain NEEDLE.
expect_absent() {
	local name=$1 needle=$2 out
	shift 2
	if ! out=$(render "$@" 2>&1); then
		fail "$name (helm template failed: $out)"
		return
	fi
	if grep -qF -- "$needle" <<<"$out"; then fail "$name"; else pass "$name"; fi
}

# expect_equal NAME WANT GOT
expect_equal() {
	if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 (want '$2', got '$3')"; fi
}

# --- webhooks.enabled drives --enable-webhooks ---------------------------
expect_absent "enabled webhooks pass no flag (older images keep working)" "--enable-webhooks"
expect_contains "webhooks.enabled=false disables them in the operator" \
	"--enable-webhooks=false" --set webhooks.enabled=false

if [ "$failures" -gt 0 ]; then
	printf '%d chart render test(s) failed\n' "$failures"
	exit 1
fi
printf 'all chart render tests passed\n'
