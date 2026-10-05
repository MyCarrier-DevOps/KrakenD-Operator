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

# The CRD pattern of the gateway duration fields (one integer and one unit), verbatim.
single_unit_re='^[0-9]+(ns|ms|us|µs|s|m|h)$'

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

jq_opts=(-r --arg path_re "$endpoint_path_re" --arg go_re "$go_duration_re" --arg one_re "$single_unit_re")

jq_lib='
# The API server anchors ^ and $ at the ends of the text only; Oniguruma also
# anchors them at line breaks, so anchor the CRD pattern at the ends explicitly.
def crd_test($re): test($re | sub("^\\^"; "\\A") | sub("\\$$"; "\\z"));
# Why a duration string breaks its CRD rules, or nothing: $re is its pattern.
# True when a pattern-valid duration does not fit in 64 bits of nanoseconds,
# which time.ParseDuration (and so the CRD duration() rule) rejects. Each
# component is compared exactly, as digits, against the most its unit allows;
# the sum is compared as a float, which is exact to within a microsecond.
def unit_max: {"ns": "9223372036854775807", "us": "9223372036854775", "µs": "9223372036854775",
  "μs": "9223372036854775", "ms": "9223372036854", "s": "9223372036", "m": "153722867", "h": "2562047"};
def unit_ns: {"ns": 1, "us": 1e3, "µs": 1e3, "μs": 1e3, "ms": 1e6, "s": 1e9, "m": 6e10, "h": 3.6e12};
def digits_exceed($max): sub("^0+(?=.)"; "") | (length > ($max | length)) or (length == ($max | length) and . > $max);
def overflows:
  [scan("([0-9]*)(\\.[0-9]*)?(ns|us|µs|μs|ms|s|m|h)")
    | {int: (.[0] | if . == "" then "0" else . end), frac: (.[1] // ""), unit: .[2]}] as $parts
  | any($parts[]; .int as $i | .unit as $u | $i | digits_exceed(unit_max[$u]))
    or ([$parts[] | ((.int + .frac) | tonumber) * unit_ns[.unit]] | add // 0) > 9223372036854775807;
def dur_problem($re; $max; $label):
  if crd_test($re) | not then "\($label) \(.)"
  elif length > $max then "\($label) is longer than \($max) characters"
  elif overflows then "\($label) \(.) does not fit in 64 bits of nanoseconds"
  else empty end;
# Why a value is outside an enum: $set lists the values the CRD allows.
def enum_problem($label; $set): select(IN($set[]) | not) | "\($label) \(.)";
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
    ($e.timeout // empty | dur_problem($go_re; 64; "\($p).timeout")),
    ($e.cacheTTL // empty | dur_problem($go_re; 64; "\($p).cacheTTL")),
    ($e.outputEncoding // empty
      | enum_problem("\($p).outputEncoding"; ["json", "json-collection", "yaml", "fast-json", "xml", "negotiate", "string", "no-op"])),
    (($e.backends // []) | to_entries[] | .key as $j | .value as $b | "\($p).backends[\($j)]" as $q | (
      ($b.encoding // empty | enum_problem("\($q).encoding"; ["json", "safejson", "fast-json", "xml", "rss", "string", "no-op", "yaml"])),
      ($b.sd // empty | enum_problem("\($q).sd"; ["static", "dns", "dns-shared"])),
      ($b.method // empty
        | enum_problem("\($q).method"; ["GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD", "CONNECT", "TRACE"])),
      (if $b.policyRef != null and ($b.policyRef.name // "") == "" then "\($q).policyRef.name is empty" else empty end)
    ))
  ))
]} | report("KrakenDEndpoint")' "$work/endpoints.json"

jq "${jq_opts[@]}" "$jq_lib"'
.items[] | .spec as $s | {id: "\(.metadata.namespace)/\(.metadata.name)", v: [
  ($s.config.timeout // empty | dur_problem($one_re; 64; "spec.config.timeout")),
  ($s.config.cacheTTL // empty | dur_problem($one_re; 64; "spec.config.cacheTTL")),
  ($s.config.dnsCacheTTL // empty | dur_problem($one_re; 64; "spec.config.dnsCacheTTL")),
  ($s.config.cors.maxAge // empty | select(crd_test($one_re) | not) | "spec.config.cors.maxAge \(.)"),
  ($s.config.port // empty | select(. < 1 or . > 65535) | "spec.config.port \(.)")
]} | report("KrakenDGateway")' "$work/gateways.json"
