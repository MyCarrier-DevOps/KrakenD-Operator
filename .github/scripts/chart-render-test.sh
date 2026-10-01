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

# ca_bundles [helm args...]: the caBundle value of each webhook, space-separated.
ca_bundles() {
	render --show-only templates/validating-webhook-configuration.yaml "$@" |
		awk '$1 == "caBundle:" { print $2 }' | tr '\n' ' '
}

# --- webhooks.enabled drives --enable-webhooks ---------------------------
expect_absent "enabled webhooks pass no flag (older images keep working)" "--enable-webhooks"
expect_contains "webhooks.enabled=false disables them in the operator" \
	"--enable-webhooks=false" --set webhooks.enabled=false
expect_absent "webhooks.enabled=false passes no serving certificate path" \
	"--webhook-cert-path" --set webhooks.enabled=false
expect_absent "webhooks.enabled=false declares no serving certificate volume" \
	"webhook-server-cert" --set webhooks.enabled=false
expect_absent "webhooks.enabled=false mounts no serving certificate" \
	"serving-certs" --set webhooks.enabled=false
expect_contains "enabled webhooks pass the serving certificate path" \
	"--webhook-cert-path=/tmp/k8s-webhook-server/serving-certs"
expect_contains "enabled webhooks declare the serving certificate volume" \
	"secretName: t-krakend-operator-webhook-server-cert" --show-only templates/deployment.yaml
expect_contains "enabled webhooks mount the serving certificate" \
	"mountPath: /tmp/k8s-webhook-server/serving-certs" --show-only templates/deployment.yaml

# --- caBundle is base64 of the PEM, never double-encoded ------------------
pem=$'-----BEGIN CERTIFICATE-----\nZmFrZS1jZXJ0aWZpY2F0ZQ==\n-----END CERTIFICATE-----'
printf '%s' "$pem" >"$workdir/ca.pem"
b64=$(printf '%s' "$pem" | base64 | tr -d '\n')
want="$b64 $b64 $b64 $b64 " # one per webhook
printf '# Issuer: Example Root CA\n%s' "$pem" >"$workdir/ca-commented.pem"
no_cert_manager=(--set webhooks.certManager.enabled=false)
expect_equal "a PEM caBundle is base64-encoded once" "$want" \
	"$(ca_bundles "${no_cert_manager[@]}" --set-file webhooks.caBundle="$workdir/ca.pem")"
expect_equal "a base64 caBundle is passed through" "$want" \
	"$(ca_bundles "${no_cert_manager[@]}" --set webhooks.caBundle="$b64")"
b64_commented=$(base64 <"$workdir/ca-commented.pem" | tr -d '\n')
want_commented="$b64_commented $b64_commented $b64_commented $b64_commented "
expect_equal "a PEM with leading text is base64-encoded once" "$want_commented" \
	"$(ca_bundles "${no_cert_manager[@]}" --set-file webhooks.caBundle="$workdir/ca-commented.pem")"

if [ "$failures" -gt 0 ]; then
	printf '%d chart render test(s) failed\n' "$failures"
	exit 1
fi
printf 'all chart render tests passed\n'
