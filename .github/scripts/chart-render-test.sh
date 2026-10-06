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

# manifest KIND NAME [helm args...]: the rendered document of that kind and
# metadata name.
manifest() {
	local kind=$1 name=$2
	shift 2
	render "$@" | awk -v k="kind: $kind" -v n="  name: $name" '
		/^---/ { if (doc ~ ("\n" k "\n") && index(doc, "\n" n "\n")) print doc; doc = ""; next }
		{ doc = doc "\n" $0 }
		END { if (doc ~ ("\n" k "\n") && index(doc, "\n" n "\n")) print doc }'
}

# rules_block: the "rules:" list of a ClusterRole or Role manifest on stdin.
rules_block() { awk '/^rules:/ { f = 1; next } /^---/ { f = 0 } f'; }

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

# --- autoconfig.maxConcurrentReconciles drives the AutoConfig worker count -
expect_contains "the AutoConfig worker count defaults to 4" \
	"--autoconfig-max-concurrent-reconciles=4"
expect_contains "autoconfig.maxConcurrentReconciles sets the AutoConfig worker count" \
	"--autoconfig-max-concurrent-reconciles=8" --set autoconfig.maxConcurrentReconciles=8

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

# --- deleting a policy never waits on the webhook (a finalizer protects it) ---
expect_equal "the chart's policy webhook is not registered for DELETE" \
	"CREATE UPDATE " "$(webhook_operations vkrakendbackendpolicy.kb.io)"
expect_equal "the kustomize policy webhook is not registered for DELETE" \
	"CREATE UPDATE " "$(manifest_operations vkrakendbackendpolicy.kb.io operator/config/webhook/manifests.yaml)"

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

# --- the operator knows its own username (AutoConfig write exemption) ------
# env_field NAME: the downward API fieldPath of env var NAME, read from stdin.
env_field() {
	awk -v name="$1" '$1 == "-" && $2 == "name:" && $3 == name { found = 1; next }
		found && $1 == "fieldPath:" { print $2; exit }
		found && $1 == "-" { exit }'
}
chart_deployment=$(render --show-only templates/deployment.yaml)
expect_equal "the chart sets POD_SERVICE_ACCOUNT from spec.serviceAccountName" "spec.serviceAccountName" \
	"$(env_field POD_SERVICE_ACCOUNT <<<"$chart_deployment")"
expect_equal "the chart sets POD_NAMESPACE from metadata.namespace" "metadata.namespace" \
	"$(env_field POD_NAMESPACE <<<"$chart_deployment")"
expect_equal "kustomize sets POD_SERVICE_ACCOUNT from spec.serviceAccountName" "spec.serviceAccountName" \
	"$(env_field POD_SERVICE_ACCOUNT <operator/config/manager/manager.yaml)"
expect_equal "kustomize sets POD_NAMESPACE from metadata.namespace" "metadata.namespace" \
	"$(env_field POD_NAMESPACE <operator/config/manager/manager.yaml)"

# --- the chart refuses clusters below the Kubernetes 1.33 floor ---------
if floor_err=$(helm template t "$CHART" --kube-version 1.32.0 2>&1 >/dev/null); then
	fail "a Kubernetes 1.32 cluster is refused"
elif grep -q kubeVersion <<<"$floor_err"; then
	pass "a Kubernetes 1.32 cluster is refused"
else
	fail "a Kubernetes 1.32 cluster is refused for another reason: $floor_err"
fi

# --- RBAC ---------------------------------------------------------------
expect_equal "the chart's manager ClusterRole matches config/rbac/role.yaml" \
	"$(rules_block <operator/config/rbac/role.yaml)" \
	"$(manifest ClusterRole t-krakend-operator-manager-role | rules_block)"
expect_equal "the leader-election Role grants nothing on configmaps" "0" \
	"$(manifest Role t-krakend-operator-leader-election-role | grep -c -- '- configmaps' || true)"
expect_equal "the chart's leader-election Role matches config/rbac" \
	"$(rules_block <operator/config/rbac/leader_election_role.yaml)" \
	"$(manifest Role t-krakend-operator-leader-election-role | rules_block)"

# --- metrics -------------------------------------------------------------
expect_contains "metrics RBAC lets the operator create TokenReviews" "- tokenreviews" \
	--show-only templates/metrics-rbac.yaml
expect_contains "metrics RBAC lets the operator create SubjectAccessReviews" "- subjectaccessreviews" \
	--show-only templates/metrics-rbac.yaml
# binding_summary: "<role> <subject>/<namespace>" of a ClusterRoleBinding on stdin.
binding_summary() {
	awk '$1 == "roleRef:" { in_ref = 1 } $1 == "subjects:" { in_ref = 0 }
		in_ref && $1 == "name:" { role = $2 }
		$1 == "-" && $2 == "kind:" { subject = 1; next }
		subject && $1 == "name:" { name = $2 }
		subject && $1 == "namespace:" { ns = $2 }
		END { print role, name "/" ns }'
}
expect_equal "the metrics auth ClusterRole is bound to the operator ServiceAccount" \
	"t-krakend-operator-metrics-auth-role t-krakend-operator-controller-manager/krakend-operator-system" \
	"$(manifest ClusterRoleBinding t-krakend-operator-metrics-auth-rolebinding | binding_summary)"
expect_contains "a metrics-reader ClusterRole allows GET /metrics" "- /metrics" \
	--show-only templates/metrics-rbac.yaml
expect_absent "metrics.enabled=false renders no metrics RBAC" "tokenreviews" --set metrics.enabled=false
expect_equal "the chart's metrics auth ClusterRole matches config/rbac" \
	"$(rules_block <operator/config/rbac/metrics_auth_role.yaml)" \
	"$(manifest ClusterRole t-krakend-operator-metrics-auth-role | rules_block)"
# The kustomize copy quotes the path; the chart does not.
expect_equal "the chart's metrics-reader ClusterRole matches config/rbac" \
	"$(rules_block <operator/config/rbac/metrics_reader_role.yaml | tr -d '"')" \
	"$(manifest ClusterRole t-krakend-operator-metrics-reader | rules_block)"
expect_contains "the ServiceMonitor selects only the metrics Service" "app.kubernetes.io/component: metrics" \
	--set metrics.serviceMonitor.enabled=true --show-only templates/servicemonitor.yaml
expect_absent "no ServiceMonitor by default" "kind: ServiceMonitor"
# selects SERVICE_TEMPLATE: "yes" when every spec.selector.matchLabels pair of
# the ServiceMonitor is among the metadata labels of that Service template.
selects() {
	local labels selector
	selector=$(render --set metrics.serviceMonitor.enabled=true --show-only templates/servicemonitor.yaml |
		awk '/^  selector:/ { f = 1; next } f && /^    matchLabels:/ { next } f && NF { sub(/^ +/, ""); print }')
	labels=$(render --show-only "$1" | awk '/^  labels:/ { f = 1; next } f && /^    / { sub(/^ +/, ""); print; next } f { exit }')
	while IFS= read -r pair; do
		grep -qxF -- "$pair" <<<"$labels" || { echo no; return; }
	done <<<"$selector"
	echo yes
}
expect_equal "the ServiceMonitor selector matches the metrics Service" "yes" "$(selects templates/metrics-service.yaml)"
expect_equal "the ServiceMonitor selector does not match the webhook Service" "no" "$(selects templates/webhook-service.yaml)"
expect_absent "the webhook Service carries no metrics component label" "app.kubernetes.io/component: metrics" \
	--show-only templates/webhook-service.yaml

# --- availability ---------------------------------------------------------
expect_contains "two replicas by default" "replicas: 2" --show-only templates/deployment.yaml
expect_contains "a PodDisruptionBudget allows one disruption" "maxUnavailable: 1" \
	--show-only templates/pdb.yaml
expect_absent "no PodDisruptionBudget for a single replica" "kind: PodDisruptionBudget" --set replicaCount=1
expect_absent "podDisruptionBudget.enabled=false renders none" "kind: PodDisruptionBudget" \
	--set podDisruptionBudget.enabled=false

if [ "$failures" -gt 0 ]; then
	printf '%d chart render test(s) failed\n' "$failures"
	exit 1
fi
printf 'all chart render tests passed\n'
