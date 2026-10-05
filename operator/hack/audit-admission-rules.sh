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

jq_lib='
def report(kind): select(.v | length > 0) | "\(kind) \(.id): \(.v | unique | join("; "))";
'

jq -r "$jq_lib"'
.items[] | {id: "\(.metadata.namespace)/\(.metadata.name)", v: [
  (if ((.spec.endpoints // []) | length) == 0 then "spec.endpoints is empty" else empty end),
  (if (.spec.gatewayRef.name // "") == "" then "spec.gatewayRef.name is empty" else empty end)
]} | report("KrakenDEndpoint")' "$work/endpoints.json"
