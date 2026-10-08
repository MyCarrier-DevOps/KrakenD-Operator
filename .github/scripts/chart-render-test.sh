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

# expect_render_fails NAME [helm args...]: helm template exits non-zero.
expect_render_fails() {
	local name=$1
	shift
	if render "$@" >/dev/null 2>&1; then fail "$name"; else pass "$name"; fi
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

# manifest_operations WEBHOOK FILE: the same, for the generated manifest
# (operator/config/webhook/manifests.yaml), in which each webhook item starts
# with "- admissionReviewVersions:".
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
expect_equal "the generated gateway webhook is not registered for DELETE" \
	"CREATE UPDATE " "$(manifest_operations vkrakendgateway.kb.io operator/config/webhook/manifests.yaml)"

# --- deleting a policy never waits on the webhook (a finalizer protects it) ---
expect_equal "the chart's policy webhook is not registered for DELETE" \
	"CREATE UPDATE " "$(webhook_operations vkrakendbackendpolicy.kb.io)"
expect_equal "the generated policy webhook is not registered for DELETE" \
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
expect_equal "every generated webhook has timeoutSeconds 15" \
	"$(grep -c '^  name: v.*kb.io' operator/config/webhook/manifests.yaml)" \
	"$(grep -c 'timeoutSeconds: 15' operator/config/webhook/manifests.yaml)"

# --- the operator knows its own username (AutoConfig write exemption) ------
# env_field NAME: the downward API fieldPath of env var NAME, read from stdin.
env_field() {
	awk -v name="$1" '$1 == "-" && $2 == "name:" && $3 == name { found = 1; next }
		found && $1 == "fieldPath:" { print $2; exit }
		found && $1 == "-" { exit }'
}
# env_value NAME: the literal value of env var NAME, read from stdin.
env_value() {
	awk -v name="$1" '$1 == "-" && $2 == "name:" && $3 == name { found = 1; next }
		found && $1 == "value:" { print $2; exit }
		found && $1 == "-" { exit }'
}
# env_line NAME: the line number of env var NAME in stdin, empty when absent.
env_line() { grep -nxE "[[:space:]]*- name: $1" | head -1 | cut -d: -f1; }
# expect_before NAME FIRST SECOND: env var FIRST is listed before SECOND in
# stdin, which Kubernetes needs to expand $(FIRST) inside SECOND's value.
expect_before() {
	local name=$1 first second input
	input=$(cat)
	first=$(env_line "$2" <<<"$input")
	second=$(env_line "$3" <<<"$input")
	if [ -n "$first" ] && [ -n "$second" ] && [ "$first" -lt "$second" ]; then pass "$name"; else fail "$name ($2 line '${first}', $3 line '${second}')"; fi
}
chart_deployment=$(render --show-only templates/deployment.yaml)
expect_equal "the chart sets POD_SERVICE_ACCOUNT from spec.serviceAccountName" "spec.serviceAccountName" \
	"$(env_field POD_SERVICE_ACCOUNT <<<"$chart_deployment")"
expect_equal "the chart sets POD_NAMESPACE from metadata.namespace" "metadata.namespace" \
	"$(env_field POD_NAMESPACE <<<"$chart_deployment")"

# --- telemetry -----------------------------------------------------------
expect_equal "the chart sets POD_NAME from metadata.name" "metadata.name" \
	"$(env_field POD_NAME <<<"$chart_deployment")"
expect_absent "nothing is exported over OTLP by default" "OTEL_" --show-only templates/deployment.yaml
expect_absent "the default stdout format passes no flag (older images keep working)" "--log-format" \
	--show-only templates/deployment.yaml
expect_contains "telemetry.logs.format=pretty passes --log-format=pretty" "- --log-format=pretty" \
	--show-only templates/deployment.yaml --set telemetry.logs.format=pretty
otlp=(--show-only templates/deployment.yaml --set telemetry.otlp.endpoint=http://collector:4318)
expect_contains "an OTLP endpoint is passed to the operator" 'value: "http://collector:4318"' "${otlp[@]}"
expect_contains "the OTLP protocol defaults to http/protobuf" 'value: "http/protobuf"' "${otlp[@]}"
expect_absent "every signal is exported by default" "S_EXPORTER" "${otlp[@]}"
expect_contains "OTLP headers are read from the named Secret" "name: otlp-auth" "${otlp[@]}" \
	--set telemetry.otlp.headersSecret.name=otlp-auth
expect_contains "a missing headers Secret or key does not stop the pod from starting" "optional: true" \
	"${otlp[@]}" --set telemetry.otlp.headersSecret.name=otlp-auth
expect_equal "a signal turned off is not exported" '"none"' \
	"$(render "${otlp[@]}" --set telemetry.otlp.signals.logs=false | env_value OTEL_LOGS_EXPORTER)"
expect_absent "headers without an endpoint pass nothing" "OTEL_EXPORTER_OTLP_HEADERS" \
	--show-only templates/deployment.yaml --set telemetry.otlp.headersSecret.name=otlp-auth
expect_contains "the sampler argument is passed as a string" 'value: "0.1"' --show-only templates/deployment.yaml \
	--set telemetry.traces.sampler=parentbased_traceidratio --set telemetry.traces.samplerArg=0.1
expect_contains "resource attributes are joined in key order" 'value: "env=prod,team=platform"' \
	--show-only templates/deployment.yaml --set telemetry.resourceAttributes.team=platform \
	--set telemetry.resourceAttributes.env=prod

# --- the opt-in node-local collector -------------------------------------
node_collector=(--show-only templates/deployment.yaml --set telemetry.otlp.nodeCollector.enabled=true)
node_deployment=$(render "${node_collector[@]}")
expect_equal "the node collector reads NODE_IP from status.hostIP" "status.hostIP" \
	"$(env_field NODE_IP <<<"$node_deployment")"
expect_contains "the node collector endpoint is the node IP on 4318" 'value: "http://$(NODE_IP):4318"' \
	"${node_collector[@]}"
expect_contains "the node collector port follows telemetry.otlp.nodeCollector.port" \
	'value: "http://$(NODE_IP):4317"' "${node_collector[@]}" \
	--set telemetry.otlp.nodeCollector.port=4317 --set telemetry.otlp.protocol=grpc
expect_contains "the node collector labels the node and the pod" \
	'value: "k8s.node.name=$(NODE_NAME),k8s.pod.uid=$(POD_UID),k8s.pod.ip=$(POD_IP)"' "${node_collector[@]}"
expect_contains "the node and pod labels follow the configured resource attributes" \
	'value: "env=prod,team=platform,k8s.node.name=$(NODE_NAME),k8s.pod.uid=$(POD_UID),k8s.pod.ip=$(POD_IP)"' \
	"${node_collector[@]}" --set telemetry.resourceAttributes.team=platform --set telemetry.resourceAttributes.env=prod
expect_equal "the node collector reads NODE_NAME from spec.nodeName" "spec.nodeName" \
	"$(env_field NODE_NAME <<<"$node_deployment")"
expect_equal "the node collector reads POD_UID from metadata.uid" "metadata.uid" \
	"$(env_field POD_UID <<<"$node_deployment")"
expect_equal "the node collector reads POD_IP from status.podIP" "status.podIP" \
	"$(env_field POD_IP <<<"$node_deployment")"
expect_equal "the node collector honours the signals setting" '"none"' \
	"$(render "${node_collector[@]}" --set telemetry.otlp.signals.logs=false | env_value OTEL_LOGS_EXPORTER)"
expect_contains "the node collector honours the headers Secret" "name: otlp-auth" \
	"${node_collector[@]}" --set telemetry.otlp.headersSecret.name=otlp-auth
if both_err=$(render "${node_collector[@]}" --set telemetry.otlp.endpoint=http://collector:4318 2>&1 >/dev/null); then
	fail "an endpoint together with the node collector is refused"
elif grep -qF "telemetry.otlp.endpoint" <<<"$both_err" && grep -qF "telemetry.otlp.nodeCollector.enabled" <<<"$both_err"; then
	pass "an endpoint together with the node collector is refused"
else
	fail "an endpoint together with the node collector is refused without naming both values: $both_err"
fi
for var in NODE_IP NODE_NAME POD_UID POD_IP; do
	expect_absent "the node collector is off by default: no $var" "name: $var" --show-only templates/deployment.yaml
done

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
leader_election_rules='- apiGroups:
  - coordination.k8s.io
  resources:
  - leases
  verbs:
  - get
  - create
  - update
- apiGroups:
  - ""
  resources:
  - events
  verbs:
  - create
  - patch'
expect_equal "the chart's leader-election Role grants leases and events only" "$leader_election_rules" \
	"$(manifest Role t-krakend-operator-leader-election-role | rules_block)"

# --- admin, editor and viewer ClusterRoles for each kind --------------------
# user_role_rules ROLE RESOURCE: the rules the chart grants ROLE on RESOURCE.
user_role_rules() {
	local verbs
	case $1 in
	admin) verbs="  - '*'" ;;
	editor) verbs=$'  - create\n  - delete\n  - get\n  - list\n  - patch\n  - update\n  - watch' ;;
	viewer) verbs=$'  - get\n  - list\n  - watch' ;;
	esac
	printf -- '- apiGroups:\n  - gateway.krakend.io\n  resources:\n  - %s\n  verbs:\n%s\n- apiGroups:\n  - gateway.krakend.io\n  resources:\n  - %s/status\n  verbs:\n  - get\n' "$2" "$verbs" "$2"
}
for pair in krakendgateway:krakendgateways krakendendpoint:krakendendpoints \
	krakendbackendpolicy:krakendbackendpolicies krakendautoconfig:krakendautoconfigs; do
	kind=${pair%%:*} resource=${pair#*:}
	for role in admin editor viewer; do
		expect_equal "the chart has a $role ClusterRole for $resource" "$(user_role_rules "$role" "$resource")" \
			"$(manifest ClusterRole "t-krakend-operator-$kind-$role-role" | rules_block)"
	done
done
expect_equal "the chart renders exactly the 12 user ClusterRoles" "12" \
	"$(render | grep -cE '^  name: t-krakend-operator-krakend(gateway|endpoint|backendpolicy|autoconfig)-(admin|editor|viewer)-role$')"

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
metrics_auth_rules='- apiGroups:
  - authentication.k8s.io
  resources:
  - tokenreviews
  verbs:
  - create
- apiGroups:
  - authorization.k8s.io
  resources:
  - subjectaccessreviews
  verbs:
  - create'
expect_equal "the chart's metrics auth ClusterRole creates TokenReviews and SubjectAccessReviews" \
	"$metrics_auth_rules" "$(manifest ClusterRole t-krakend-operator-metrics-auth-role | rules_block)"
metrics_reader_rules='- nonResourceURLs:
  - /metrics
  verbs:
  - get'
expect_equal "the chart's metrics-reader ClusterRole allows GET /metrics only" "$metrics_reader_rules" \
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
expect_contains "the operator pod's grace period is the 10 s cmd/run_shape_test.go budgets for" \
	"terminationGracePeriodSeconds: 10" --show-only templates/deployment.yaml
expect_contains "two replicas by default" "replicas: 2" --show-only templates/deployment.yaml
expect_contains "a PodDisruptionBudget allows one disruption" "maxUnavailable: 1" \
	--show-only templates/pdb.yaml
expect_absent "no PodDisruptionBudget for a single replica" "kind: PodDisruptionBudget" --set replicaCount=1
expect_absent "podDisruptionBudget.enabled=false renders none" "kind: PodDisruptionBudget" \
	--set podDisruptionBudget.enabled=false
expect_contains "replicas prefer different nodes by default" "preferredDuringSchedulingIgnoredDuringExecution" \
	--show-only templates/deployment.yaml
expect_absent "a user affinity replaces the default" "podAntiAffinity" --show-only templates/deployment.yaml \
	--set 'affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].key=kubernetes.io/os' \
	--set 'affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator=Exists'
expect_render_fails "several replicas without leader election are refused" --set leaderElection.enabled=false
expect_contains "one replica without leader election still renders" "kind: Deployment" \
	--set leaderElection.enabled=false --set replicaCount=1

# --- metrics.certManager --------------------------------------------------
expect_absent "metrics are served with the operator's own certificate by default" "--metrics-cert-path"
expect_absent "no metrics Certificate by default" "t-krakend-operator-metrics-cert"
mc=(--set metrics.certManager.enabled=true)
expect_contains "metrics.certManager passes the metrics certificate path" \
	"- --metrics-cert-path=/tmp/k8s-metrics-server/metrics-certs" --show-only templates/deployment.yaml "${mc[@]}"
expect_contains "metrics.certManager mounts the metrics certificate" \
	"mountPath: /tmp/k8s-metrics-server/metrics-certs" --show-only templates/deployment.yaml "${mc[@]}"
expect_equal "the metrics Certificate covers the metrics Service and fills the mounted Secret" \
	"$(printf '%s\n' '  dnsNames:' \
		'    - t-krakend-operator-metrics-service.krakend-operator-system.svc' \
		'    - t-krakend-operator-metrics-service.krakend-operator-system.svc.cluster.local' \
		'  issuerRef:' '    kind: Issuer' '    name: t-krakend-operator-selfsigned-issuer' \
		'  secretName: t-krakend-operator-metrics-server-cert')" \
	"$(manifest Certificate t-krakend-operator-metrics-cert "${mc[@]}" | awk '/^spec:/ { f = 1; next } f')"
# mounted_secret PATH [helm args...]: the Secret behind the volume mounted at PATH.
mounted_secret() {
	local path=$1
	shift
	render --show-only templates/deployment.yaml "$@" | awk -v p="$path" '
		$1 == "-" && $2 == "name:" { last = $3 }
		$1 == "mountPath:" && $2 == p { vol = last }
		$1 == "volumes:" { vols = 1 }
		vols && $1 == "-" && $2 == "name:" { cur = $3 }
		vols && $1 == "secretName:" && cur == vol { print $2; exit }'
}
expect_equal "the metrics certificate path holds the metrics Certificate's Secret" \
	"t-krakend-operator-metrics-server-cert" "$(mounted_secret /tmp/k8s-metrics-server/metrics-certs "${mc[@]}")"
expect_equal "the webhook certificate path still holds the webhook Secret" \
	"t-krakend-operator-webhook-server-cert" "$(mounted_secret /tmp/k8s-webhook-server/serving-certs "${mc[@]}")"
# issuers [helm args...]: how many Issuers the chart renders.
issuers() { render "$@" | grep -c '^kind: Issuer$' || true; }
expect_equal "the webhook certificate alone renders one Issuer" "1" "$(issuers)"
expect_equal "one Issuer serves both certificates" "1" "$(issuers "${mc[@]}")"
expect_equal "the metrics certificate alone renders one Issuer" "1" "$(issuers "${mc[@]}" --set webhooks.enabled=false)"
expect_equal "no Issuer without a cert-manager certificate" "0" "$(issuers --set webhooks.enabled=false)"
expect_absent "no Issuer when metrics are off too" "kind: Issuer" \
	"${mc[@]}" --set metrics.enabled=false --set webhooks.enabled=false
# rendered_issuer [helm args...]: the rendered Issuer's metadata.name.
rendered_issuer() { render "$@" | awk '/^kind: Issuer$/ { f = 1 } f && $1 == "name:" { print $2; exit }'; }
# issuer_ref CERTIFICATE [helm args...]: the issuerRef name of that Certificate.
issuer_ref() { manifest Certificate "$@" | awk '$1 == "issuerRef:" { f = 1 } f && $1 == "name:" { print $2; exit }'; }
expect_equal "the webhook Certificate names the rendered Issuer" "$(rendered_issuer)" \
	"$(issuer_ref t-krakend-operator-serving-cert)"
expect_equal "the metrics Certificate names the rendered Issuer" "$(rendered_issuer "${mc[@]}")" \
	"$(issuer_ref t-krakend-operator-metrics-cert "${mc[@]}")"
expect_absent "metrics.enabled=false renders no metrics certificate" "metrics-cert" "${mc[@]}" --set metrics.enabled=false
expect_contains "both certificates stay mounted together" \
	"mountPath: /tmp/k8s-webhook-server/serving-certs" --show-only templates/deployment.yaml "${mc[@]}"
sm=(--show-only templates/servicemonitor.yaml --set metrics.serviceMonitor.enabled=true)
expect_contains "without it the ServiceMonitor skips verification" "insecureSkipVerify: true" "${sm[@]}"
expect_absent "with it the ServiceMonitor verifies the certificate" "insecureSkipVerify" "${sm[@]}" "${mc[@]}"
expect_equal "the ServiceMonitor checks the metrics Service against the certificate's CA" \
	"$(printf '%s\n' '        serverName: t-krakend-operator-metrics-service.krakend-operator-system.svc' \
		'        ca:' '          secret:' '            name: t-krakend-operator-metrics-server-cert' \
		'            key: ca.crt')" \
	"$(render "${sm[@]}" "${mc[@]}" | awk '$1 == "tlsConfig:" { f = 1; next } $1 == "selector:" { f = 0 } f')"

# --- networkPolicy ----------------------------------------------------------
expect_absent "no NetworkPolicy by default" "kind: NetworkPolicy"
np=(--show-only templates/networkpolicy.yaml --set networkPolicy.enabled=true)
# np_ports [helm args...]: the ports the NetworkPolicy admits, space-separated.
np_ports() { render "${np[@]}" "$@" | awk '$1=="port:"||($1=="-"&&$2=="port:"){print $NF}' | tr '\n' ' '; }
expect_equal "the NetworkPolicy selects only the operator pods, for ingress" \
	"$(printf '%s\n' '  podSelector:' '    matchLabels:' '      app.kubernetes.io/name: krakend-operator' \
		'      app.kubernetes.io/instance: t' '      control-plane: controller-manager' \
		'  policyTypes:' '    - Ingress')" \
	"$(render "${np[@]}" | awk '$1 == "podSelector:" { f = 1 } $1 == "ingress:" { f = 0 } f')"
expect_equal "the NetworkPolicy admits the metrics and webhook ports" "8443 9443 " "$(np_ports)"
expect_equal "with webhooks off only the metrics port is admitted" "8443 " "$(np_ports --set webhooks.enabled=false)"
expect_equal "with metrics off only the webhook port is admitted" "9443 " "$(np_ports --set metrics.enabled=false)"
expect_equal "the admitted metrics port follows metrics.service.port" "9000 9443 " "$(np_ports --set metrics.service.port=9000)"
expect_equal "metrics are admitted from the selected namespaces" \
	"$(printf '%s\n' '    - from:' '        - namespaceSelector:' '            matchLabels:' '              metrics: enabled')" \
	"$(render "${np[@]}" | awk '$1 == "-" && $2 == "from:" { f = 1 } $1 == "ports:" { f = 0 } f')"
expect_equal "only the metrics rule restricts its sources" "1" "$(render "${np[@]}" | grep -c 'from:')"
expect_contains "the metrics namespace selector is configurable" "team: observability" "${np[@]}" \
	--set networkPolicy.metricsNamespaceSelector.matchLabels.team=observability
expect_absent "a namespace selector can drop the default label" "metrics: enabled" "${np[@]}" \
	--set networkPolicy.metricsNamespaceSelector.matchLabels.metrics=null \
	--set networkPolicy.metricsNamespaceSelector.matchLabels.team=observability
expect_absent "with metrics and webhooks off no port is admitted" "port:" "${np[@]}" \
	--set metrics.enabled=false --set webhooks.enabled=false

# --- fix-round rows ------------------------------------------------------
expect_contains "a sampler argument of 0 is passed, not dropped" 'value: "0"' --show-only templates/deployment.yaml \
	--set telemetry.traces.sampler=parentbased_traceidratio --set telemetry.traces.samplerArg=0
printf 'telemetry:\n  traces:\n    sampler: parentbased_traceidratio\n    samplerArg: 0\n' >"$workdir/sampler.yaml"
expect_contains "a samplerArg of 0 in a values file is passed" 'value: "0"' --show-only templates/deployment.yaml \
	-f "$workdir/sampler.yaml"
expect_contains "a comma in a resource attribute value is percent-encoded" 'value: "env=prod%2Cteam%3Dplatform"' \
	--show-only templates/deployment.yaml --set 'telemetry.resourceAttributes.env=prod\,team=platform'
expect_contains "a space in a resource attribute value is percent-encoded" 'value: "team=platform%20engineering"' \
	--show-only templates/deployment.yaml --set 'telemetry.resourceAttributes.team=platform engineering'
expect_render_fails "a comma in a resource attribute key is refused" \
	--show-only templates/deployment.yaml --set 'telemetry.resourceAttributes.a\,b=c'
expect_render_fails "an equals sign in a resource attribute key is refused" \
	--show-only templates/deployment.yaml --set 'telemetry.resourceAttributes.a\=b=c'
render "${node_collector[@]}" | expect_before "NODE_IP is defined before the endpoint that expands it" \
	NODE_IP OTEL_EXPORTER_OTLP_ENDPOINT
for var in NODE_NAME POD_UID POD_IP; do
	render "${node_collector[@]}" | expect_before "$var is defined before the resource attributes that expand it" \
		"$var" OTEL_RESOURCE_ATTRIBUTES
done
expect_absent "an empty log format passes no flag" "--log-format" --show-only templates/deployment.yaml \
	--set telemetry.logs.format=

if [ "$failures" -gt 0 ]; then
	printf '%d chart render test(s) failed\n' "$failures"
	exit 1
fi
printf 'all chart render tests passed\n'
