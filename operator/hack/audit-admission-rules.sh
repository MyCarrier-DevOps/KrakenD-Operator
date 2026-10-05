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

# The CRD pattern of a resource quantity, verbatim.
quantity_re='^(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?$'

# The Enterprise-only extra_config namespaces per level, which the renderer
# strips from a CE render and admission rejects on a CE gateway.
ee_only="$(dirname "$0")/../internal/renderer/eeonly_namespaces.json"

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

jq_opts=(-r --arg path_re "$endpoint_path_re" --arg go_re "$go_duration_re" --arg one_re "$single_unit_re"
	--arg qty_re "$quantity_re" --slurpfile ee "$ee_only")

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
def duration_ns:
  [scan("([0-9]*)(\\.[0-9]*)?(ns|us|µs|μs|ms|s|m|h)")
    | ((if .[0] == "" then "0" else .[0] end) + (.[1] // "") | tonumber) * unit_ns[.[2]]] | add // 0;
def overflows:
  [scan("([0-9]*)(\\.[0-9]*)?(ns|us|µs|μs|ms|s|m|h)")
    | {int: (.[0] | if . == "" then "0" else . end), frac: (.[1] // ""), unit: .[2]}] as $parts
  | any($parts[]; .int as $i | .unit as $u | $i | digits_exceed(unit_max[$u]))
    or ([$parts[] | ((.int + .frac) | tonumber) * unit_ns[.unit]] | add // 0) > 9223372036854775807;
def dur_shape_problem($re; $max; $label):
  if crd_test($re) | not then "\($label) \(.)"
  elif length > $max then "\($label) is longer than \($max) characters"
  else empty end;
def dur_problem($re; $max; $label):
  dur_shape_problem($re; $max; $label)
  // if overflows then "\($label) \(.) does not fit in 64 bits of nanoseconds" else empty end;
# Why a value is outside an enum: $set lists the values the CRD allows.
def enum_problem($label; $set): select(IN($set[]) | not) | "\($label) \(.)";
# True for a pattern-valid quantity that resource.ParseQuantity rejects, which
# the CRD isQuantity() rule rejects too: an exponent with a fraction, or one
# outside the range of a 64-bit integer.
def quantity_undecodable:
  ([capture("[eE](?<sign>[+-]?)(?<exp>[0-9.]+)$")] | .[0]) as $e
  | $e != null and (($e.exp | contains("."))
    or ($e.exp | digits_exceed(if $e.sign == "-" then "9223372036854775808" else "9223372036854775807" end)));
# Why a quantity breaks its CRD rules, or nothing; a number is always valid.
def quantity_problem($label):
  if type != "string" then empty
  elif crd_test($qty_re) | not then "\($label) \(.)"
  elif length > 64 then "\($label) is longer than 64 characters"
  elif quantity_undecodable then "\($label) \(.) is not a quantity Kubernetes can decode"
  else empty end;
# The keys of an extra_config object that are in $names.
def eeonly($names): [(. // {}) | keys[] | select(IN($names[]))];
# The values the endpoint and AutoConfig enum fields allow.
def output_encodings: ["json", "json-collection", "yaml", "fast-json", "xml", "negotiate", "string", "no-op"];
def backend_encodings: ["json", "safejson", "fast-json", "xml", "rss", "string", "no-op", "yaml"];
def discoveries: ["static", "dns", "dns-shared"];
def backend_methods: ["GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD", "CONNECT", "TRACE"];
# The rules of a backend entry of a KrakenDEndpoint or an AutoConfig, at path $q.
def backend_problems($q):
  (.encoding // empty | enum_problem("\($q).encoding"; backend_encodings)),
  (.sd // empty | enum_problem("\($q).sd"; discoveries)),
  (.method // empty | enum_problem("\($q).method"; backend_methods)),
  (if .policyRef != null and (.policyRef.name // "") == "" then "\($q).policyRef.name is empty" else empty end);
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
      | enum_problem("\($p).outputEncoding"; output_encodings)),
    (($e.backends // []) | to_entries[] | .key as $j | .value | backend_problems("\($p).backends[\($j)]"))
  ))
]} | report("KrakenDEndpoint")' "$work/endpoints.json"

jq "${jq_opts[@]}" "$jq_lib"'
.items[] | .spec as $s | {id: "\(.metadata.namespace)/\(.metadata.name)", v: [
  ($s.config.timeout // empty | dur_problem($one_re; 64; "spec.config.timeout")),
  ($s.config.cacheTTL // empty | dur_problem($one_re; 64; "spec.config.cacheTTL")),
  ($s.config.dnsCacheTTL // empty | dur_problem($one_re; 64; "spec.config.dnsCacheTTL")),
  ($s.config.cors.maxAge // empty | select(crd_test($one_re) | not) | "spec.config.cors.maxAge \(.)"),
  ($s.config.port // empty | select(. < 1 or . > 65535) | "spec.config.port \(.)"),
  ($s.config.outputEncoding // empty
    | enum_problem("spec.config.outputEncoding"; ["json", "fast-json", "json-collection", "xml", "negotiate", "string", "no-op"])),
  ($s.config.router.healthPath // empty | select(crd_test("^/") | not) | "spec.config.router.healthPath \(.)"),
  ($s.redis.connectionPool.dialTimeout // empty
    | dur_problem($one_re; 64; "spec.redis.connectionPool.dialTimeout")),
  (($s.license.externalSecret.enabled // false) as $es | ($s.license.secretRef != null) as $sr | (
    (if $s.edition == "EE" and ($es | not) and ($sr | not) then "EE without a license source" else empty end),
    (if $s.edition == "EE" and ($es | not) and $sr and (($s.license.secretRef.name // "") == "")
     then "EE license secretRef has an empty name" else empty end),
    (if $s.edition == "CE" and ($es or $sr) then "CE with a license source" else empty end),
    (if $es and $sr then "both license sources set" else empty end)
  )),
  (if ($s.openapi.enabled // false)
      and ((if ($s.openapi.port // 0) > 0 then $s.openapi.port else 8090 end)
        == (if ($s.config.port // 0) > 0 then $s.config.port else 8080 end))
   then "spec.openapi.port equals the gateway port" else empty end),
  (if ([($s.plugins.sources // [])[] | select(.persistentVolumeClaimRef != null)] | length) > 1
   then "more than one PVC plugin source" else empty end),
  (if (($s.plugins.sources // []) | length) > 32 then "more than 32 plugin sources" else empty end),
  (if ($s.postRestartJob.enabled // false) and (($s.postRestartJob.script // "") == "")
   then "spec.postRestartJob.script is empty" else empty end),
  (if $s.redis.connectionPool.password != null then "spec.redis.connectionPool.password is not supported yet" else empty end),
  (if $s.redis.connectionPool.tls != null then "spec.redis.connectionPool.tls is not supported yet" else empty end),
  (if $s.edition == "EE" and $s.dragonfly.authentication.passwordFromSecret != null
   then "spec.dragonfly.authentication.passwordFromSecret is not supported yet with edition EE" else empty end),
  ($s.postRestartJob.tmpSizeLimit // empty | quantity_problem("spec.postRestartJob.tmpSizeLimit")),
  (if $s.edition == "CE" then ($s.config.extraConfig | eeonly($ee[0].enterpriseOnly.service))[]
     | "spec.config.extraConfig \(.) is Enterprise-only on a CE gateway" else empty end),
  (if $s.edition == "CE" and $s.redis != null then "spec.redis is Enterprise-only on a CE gateway" else empty end),
  (if $s.edition == "CE" and $s.config.documentation != null
   then "spec.config.documentation is Enterprise-only on a CE gateway" else empty end),
  (if $s.edition == "CE" and ($s.openapi.enabled // false) then "spec.openapi.enabled is Enterprise-only on a CE gateway" else empty end),
  (if $s.edition == "CE" and ($s.dragonfly.enabled // false)
   then "spec.dragonfly.enabled is Enterprise-only on a CE gateway" else empty end)
]} | report("KrakenDGateway")' "$work/gateways.json"

jq "${jq_opts[@]}" "$jq_lib"'
.items[] | .spec as $s | {id: "\(.metadata.namespace)/\(.metadata.name)", v: [
  (if (.metadata.name | length) > 63 then "name longer than 63 characters" else empty end),
  (if ($s.gatewayRef.name // "") == "" then "spec.gatewayRef.name is empty" else empty end),
  (if (($s.openapi.url // "") != "") == ($s.openapi.configMapRef != null)
   then "need exactly one of openapi.url or openapi.configMapRef" else empty end),
  (if $s.openapi.configMapRef != null and (($s.urlTransform.hostMapping // []) | length) == 0
   then "configMapRef without urlTransform.hostMapping" else empty end),
  (if $s.openapi.auth.bearerTokenSecret != null and $s.openapi.auth.basicAuthSecret != null
   then "both auth secrets set" else empty end),
  (if $s.trigger == "Periodic"
      and ($s.periodic.interval // "" | crd_test($go_re) and (overflows | not) and duration_ns < 3e10)
   then "periodic.interval below 30s" else empty end),
  (if $s.trigger == "Periodic" and $s.periodic == null then "trigger Periodic without a periodic block" else empty end),
  ($s.periodic.interval // empty | if $s.trigger == "Periodic"
     then dur_problem($go_re; 32; "spec.periodic.interval")
     else dur_shape_problem($go_re; 32; "spec.periodic.interval") end),
  ($s.additionalEndpointsBasePath // empty | select(crd_test("^/") | not) | "spec.additionalEndpointsBasePath \(.)"),
  (if ($s.additionalEndpointsBasePath // "") != "" and ($s.urlTransform.addPathPrefix // "") != ""
   then "additionalEndpointsBasePath with urlTransform.addPathPrefix" else empty end),
  (if (($s.additionalEndpoints // []) | length) > 256 then "more than 256 additionalEndpoints" else empty end),
  (($s.additionalEndpoints // []) | map(.method //= "GET") | group_by([.endpoint, .method])[] | select(length > 1)
    | "duplicate additionalEndpoint \(.[0].method) \(.[0].endpoint)"),
  (if (($s.overrides // []) | length) > 1024 then "more than 1024 overrides" else empty end),
  (($s.additionalEndpoints // []) | to_entries[] | .key as $i | .value as $a | "spec.additionalEndpoints[\($i)]" as $p | (
    (if ($a.endpoint // "" | crd_test($path_re)) then empty else "\($p).endpoint \($a.endpoint)" end),
    (if (($a.backends // []) | length) > 0
        and ((($a.host // "") != "") or (($a.backendUrlPattern // "") != "") or (($a.encoding // "") != ""))
     then "\($p) mixes backends with the shorthand" else empty end),
    ($a.timeout // empty | dur_problem($go_re; 64; "\($p).timeout")),
    ($a.cacheTTL // empty | dur_problem($go_re; 64; "\($p).cacheTTL")),
    ($a.outputEncoding // empty | enum_problem("\($p).outputEncoding"; output_encodings)),
    ($a.encoding // empty | enum_problem("\($p).encoding"; backend_encodings)),
    (($a.backends // []) | to_entries[] | .key as $j | .value | backend_problems("\($p).backends[\($j)]"))
  )),
  (($s.overrides // []) | to_entries[] | .key as $i | .value as $o | "spec.overrides[\($i)]" as $p | (
    ($o.method // empty | enum_problem("\($p).method"; ["GET", "POST", "PUT", "PATCH", "DELETE"])),
    (($o.backends // [])[] | select(.index < 0) | "\($p).backends index \(.index)"),
    ($o.timeout // empty | dur_problem($go_re; 64; "\($p).timeout")),
    ($o.cacheTTL // empty | dur_problem($go_re; 64; "\($p).cacheTTL")),
    ($o.concurrentCalls // empty | select(. < 1) | "\($p).concurrentCalls \(.)"),
    ($o.outputEncoding // empty | enum_problem("\($p).outputEncoding"; output_encodings)),
    ($o.endpoint // empty | select(crd_test($path_re) | not) | "\($p).endpoint \(.)"),
    (if $o.policyRef != null and ($o.policyRef.name // "") == "" then "\($p).policyRef.name is empty" else empty end)
  )),
  (($s.overrides // []) | group_by(.operationId | ascii_downcase | gsub("[^a-z0-9-]"; "-") | gsub("^-+|-+$"; ""))[]
    | select(length > 1) | "overrides collide: \([.[].operationId] | join(", "))"),
  ($s.defaults.endpoint.timeout // empty | dur_problem($go_re; 64; "spec.defaults.endpoint.timeout")),
  ($s.defaults.endpoint.cacheTTL // empty | dur_problem($go_re; 64; "spec.defaults.endpoint.cacheTTL")),
  ($s.defaults.endpoint.outputEncoding // empty
    | enum_problem("spec.defaults.endpoint.outputEncoding"; output_encodings)),
  ($s.defaults.backend.encoding // empty | enum_problem("spec.defaults.backend.encoding"; backend_encodings)),
  ($s.defaults.backend.sd // empty | enum_problem("spec.defaults.backend.sd"; discoveries)),
  (if $s.defaults.policyRef != null and ($s.defaults.policyRef.name // "") == ""
   then "spec.defaults.policyRef.name is empty" else empty end)
]} | report("KrakenDAutoConfig")' "$work/autoconfigs.json"

# Two entries a gateway would route as one: the same method and the same path
# once parameter names are erased. KrakenD cannot register both.
jq "${jq_opts[@]}" "$jq_lib"'
[.items[] | . as $o
  | "\(.spec.gatewayRef.namespace // .metadata.namespace)/\(.spec.gatewayRef.name)" as $gw
  | (.spec.endpoints // [])[]
  | {gw: $gw, owner: "\($o.metadata.namespace)/\($o.metadata.name)",
     key: "\(.method) \(.endpoint | gsub("/\\{[a-zA-Z0-9_-]+\\}"; "/{}"))", route: "\(.method) \(.endpoint)"}]
| group_by([.gw, .key])[] | select((unique_by([.owner, .route]) | length) > 1)
| "gateway \(.[0].gw): \([.[] | "\(.route) (\(.owner))"] | join(" vs "))"' "$work/endpoints.json"
