#!/usr/bin/env bash
# Render-level tests for the krakend-operator Helm chart: each case renders
# the chart with `helm template` and asserts on the output.
# Run from the repository root: bash .github/scripts/chart-render-test.sh
set -euo pipefail

CHART=charts/krakend-operator
failures=0
workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

render() { helm template t "$CHART" --namespace krakend-operator-system --kube-version 1.33.0 "$@"; }

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

# webhook_operations WEBHOOK [helm args...]: the admission operations the
# chart registers for WEBHOOK, space-separated.
webhook_operations() {
	local webhook=$1
	shift
	render --show-only templates/validating-webhook-configuration.yaml "$@" |
		awk -v w="- name: $webhook" '
			index($0, w) { f = 1; next }
			/- name: v/ { f = 0 }
			f && /- (CREATE|UPDATE|DELETE|CONNECT)$/ { printf "%s ", $2 }'
}

# manifest_operations WEBHOOK FILE: the same, for a kustomize manifest in
# which each webhook item starts with "- admissionReviewVersions:".
manifest_operations() {
	awk -v w="name: $1" '
		/^- admissionReviewVersions:/ { f = 0 }
		index($0, w) { f = 1 }
		f && /- (CREATE|UPDATE|DELETE|CONNECT)$/ { printf "%s ", $2 }' "$2"
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
printf '%s' "$pem" | base64 -w 20 >"$workdir/ca-wrapped.b64"
expect_equal "a wrapped base64 caBundle is rendered unwrapped" "$want" \
	"$(ca_bundles "${no_cert_manager[@]}" --set-file webhooks.caBundle="$workdir/ca-wrapped.b64")"

# --- no webhook is registered for an operation it does not validate -------
expect_equal "the chart's gateway webhook is not registered for DELETE" \
	"CREATE UPDATE " "$(webhook_operations vkrakendgateway.kb.io)"
expect_equal "the kustomize gateway webhook is not registered for DELETE" \
	"CREATE UPDATE " "$(manifest_operations vkrakendgateway.kb.io operator/config/webhook/manifests.yaml)"

# --- three concurrent krakend validations fit in the operator's limit ------
memory_limit() {
	render --show-only templates/deployment.yaml "$@" |
		awk '$1 == "limits:" { in_limits = 1 } in_limits && $1 == "memory:" { print $2; exit }'
}
expect_equal "the operator memory limit is 512Mi" "512Mi" "$(memory_limit)"
expect_equal "a memory request does not stand in for the limit" "1Gi" \
	"$(memory_limit --set resources.limits.memory=1Gi --set resources.requests.memory=512Mi)"

# --- every webhook call is bounded by an explicit timeout ------------------
chart_webhooks=$(render --show-only templates/validating-webhook-configuration.yaml)
expect_equal "every chart webhook has timeoutSeconds 15" \
	"$(grep -c -- '- name: v' <<<"$chart_webhooks")" \
	"$(grep -c 'timeoutSeconds: 15' <<<"$chart_webhooks")"
expect_equal "every kustomize webhook has timeoutSeconds 15" \
	"$(grep -c '^  name: v.*kb.io' operator/config/webhook/manifests.yaml)" \
	"$(grep -c 'timeoutSeconds: 15' operator/config/webhook/manifests.yaml)"

# --- the chart refuses clusters below the Kubernetes 1.33 floor ---------
if helm template t "$CHART" --kube-version 1.32.0 >/dev/null 2>&1; then
	fail "a Kubernetes 1.32 cluster is refused"
else
	pass "a Kubernetes 1.32 cluster is refused"
fi

if [ "$failures" -gt 0 ]; then
	printf '%d chart render test(s) failed\n' "$failures"
	exit 1
fi
printf 'all chart render tests passed\n'
