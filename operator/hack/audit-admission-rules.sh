#!/usr/bin/env bash
# Lists stored KrakenD objects that violate the admission rules introduced with
# complete admission: the CRD schema and CEL rules, and the rules the webhooks
# add (route conflicts between KrakenDEndpoints of one gateway, Enterprise-only
# extra_config namespaces on CE gateways). Read-only: it only runs `kubectl get`.
#
# Usage: hack/audit-admission-rules.sh        read the current kube context
#        hack/audit-admission-rules.sh DIR    read DIR/{endpoints,gateways,autoconfigs,backendpolicies}.json
#
# Prints one line per object or conflict; no output means nothing to fix.
set -euo pipefail

# The CRD's endpoint path pattern, verbatim.
endpoint_path_re='^(/\*|/[^*?&%]*(/\*)?)$'

# The CRD pattern of the endpoint and AutoConfig duration fields (a Go duration), verbatim.
go_duration_re='^(0|(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$'

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Read every kind once, so every check sees the same snapshot.
for kind in endpoints gateways autoconfigs backendpolicies; do
	if [[ $# -gt 0 ]]; then
		cat "$1/$kind.json" >"$work/$kind.json"
	else
		kubectl get "krakend$kind.gateway.krakend.io" -A -o json >"$work/$kind.json"
	fi
done

jq_opts=(-r --arg path_re "$endpoint_path_re" --arg go_re "$go_duration_re")

jq_lib='
# The API server anchors ^ and $ at the ends of the text only; Oniguruma also
# anchors them at line breaks, so anchor the CRD pattern at the ends explicitly.
def crd_test($re): test($re | sub("^\\^"; "\\A") | sub("\\$$"; "\\z"));
# Why a duration string breaks its CRD rules, or nothing: $re is its pattern.
def dur_problem($re; $max; $label):
  if crd_test($re) | not then "\($label) \(.)"
  elif length > $max then "\($label) is longer than \($max) characters"
  else empty end;
def report(kind): select(.v | length > 0) | "\(kind) \(.id): \(.v | unique | join("; "))";
'

jq "${jq_opts[@]}" "$jq_lib"'
.items[] | {id: "\(.metadata.namespace)/\(.metadata.name)", v: [
  (if ((.spec.endpoints // []) | length) == 0 then "spec.endpoints is empty" else empty end),
  (if (.spec.gatewayRef.name // "") == "" then "spec.gatewayRef.name is empty" else empty end),
  ((.spec.endpoints // []) | group_by([.endpoint, .method])[] | select(length > 1)
    | "duplicate entry \(.[0].method) \(.[0].endpoint)"),
  ((.spec.endpoints // []) | to_entries[] | .key as $i | .value as $e | "spec.endpoints[\($i)]" as $p | (
    (if ($e.endpoint // "" | crd_test($path_re)) then empty else "\($p).endpoint \($e.endpoint)" end),
    (if (($e.backends // []) | length) == 0 then "\($p).backends is empty" else empty end),
    ($e.timeout // empty | dur_problem($go_re; 64; "\($p).timeout"))
  ))
]} | report("KrakenDEndpoint")' "$work/endpoints.json"
