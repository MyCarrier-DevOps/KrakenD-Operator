#!/usr/bin/env bash
# Lists stored KrakenD objects that violate the admission rules introduced with
# complete admission: the CRD schema and CEL rules, and the rules the webhooks
# add (route conflicts between KrakenDEndpoints of one gateway, Enterprise-only
# extra_config namespaces on CE gateways). Read-only: it only runs `kubectl get`
# and keeps a temporary copy of the objects (mode 0700) that is removed on exit.
#
# Usage: hack/audit-admission-rules.sh        read the current kube context
#        hack/audit-admission-rules.sh DIR    read DIR/{endpoints,gateways,autoconfigs,backendpolicies}.json
#
# Prints one line per object or conflict. No output means none of the checks
# below found anything, not that every rule is covered: other reserved paths
# under /__debug, /__echo and /__health (the gateway's own health path is
# checked), unnamed /* wildcards on CE gateways, unknown urlPattern placeholders
# and cross-method auto_options clashes are not checked.
# Checks:
#   KrakenDEndpoint / KrakenDGateway / KrakenDAutoConfig   the CRD schema and CEL rules
#   ... .host[N] ...                                       a backend host (or an endpoint path) holding whitespace or a
#                                                          control character, which krakend prints verbatim in its errors
#   gateway ns/name: A vs B                                 entries one gateway would route as one
#   KrakenDAutoConfig ns/name: endpoints share a route ...  endpoints it controls that one route serves
#   KrakenDEndpoint ...: ... health path of gateway ...    a GET on the gateway's health endpoint
#   ... Enterprise-only on CE gateway ...                   extra_config KrakenD CE ignores
set -euo pipefail

# The CRD's endpoint path pattern, verbatim.
endpoint_path_re='^(/\*|/[^*?&%\x00-\x20\x7F]*(/\*)?)$'

# The CRD pattern of a backend host item, verbatim.
host_re='^[^\x00-\x20\x7F]+$'

# The CRD pattern of the endpoint and AutoConfig duration fields (a Go duration), verbatim.
go_duration_re='^(0|(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$'

# The CRD pattern of the gateway duration fields (one integer and one unit), verbatim.
single_unit_re='^[0-9]+(ns|ms|us|µs|s|m|h)$'

# The CRD pattern of tmpSizeLimit, verbatim: a quantity whose exponent has at
# most two digits, so Kubernetes is never asked to parse a pathological one.
quantity_re='^(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?[0-9]{1,2}))?$'

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

jq_opts=(-r --arg path_re "$endpoint_path_re" --arg host_re "$host_re" --arg go_re "$go_duration_re" --arg one_re "$single_unit_re"
	--arg qty_re "$quantity_re" --slurpfile ee "$ee_only")

jq_lib='
# The API server anchors ^ and $ at the ends of the text only; Oniguruma lets $
# match before a final line break, so anchor the CRD pattern at the ends
# explicitly. A value that is not a string never matches.
def crd_test($re): type == "string" and test($re | sub("^\\^"; "\\A") | sub("\\$$"; "\\z"));
# Largest whole count of each unit that fits in 64 bits of nanoseconds, and the
# nanoseconds in one.
def unit_max: {"ns": "9223372036854775807", "us": "9223372036854775", "µs": "9223372036854775",
  "μs": "9223372036854775", "ms": "9223372036854", "s": "9223372036", "m": "153722867", "h": "2562047"};
def unit_ns: {"ns": 1, "us": 1e3, "µs": 1e3, "μs": 1e3, "ms": 1e6, "s": 1e9, "m": 6e10, "h": 3.6e12};
def digits_exceed($max): sub("^0+(?=.)"; "") | (length > ($max | length)) or (length == ($max | length) and . > $max);
# The components of a pattern-valid duration: whole part, fraction and unit.
def duration_parts:
  [scan("([0-9]*)(\\.[0-9]*)?(ns|us|µs|μs|ms|s|m|h)")
    | {int: (.[0] | if . == "" then "0" else . end), frac: (.[1] // ""), unit: .[2]}];
def duration_ns: [duration_parts[] | ((.int + .frac) | tonumber) * unit_ns[.unit]] | add // 0;
# True when a pattern-valid duration does not fit in 64 bits of nanoseconds,
# which time.ParseDuration (and so the CRD duration() rule) rejects. Each
# component is compared exactly, as digits, against the most its unit allows;
# the sum of several components or of a fraction is compared as a float, which
# is exact only to about a microsecond near the 292-year limit. A value of one
# whole unit is exact.
def overflows:
  any(duration_parts[]; .int as $i | .unit as $u | $i | digits_exceed(unit_max[$u]))
  or duration_ns > 9223372036854775807;
# Why a duration breaks the pattern or maxLength ($max) of its field, or nothing.
def dur_shape_problem($re; $max; $label):
  if crd_test($re) | not then "\($label) \(tojson)"
  elif length > $max then "\($label) is longer than \($max) characters"
  else empty end;
# The same, plus the overflow the CRD duration() rule rejects.
def dur_problem($re; $max; $label):
  dur_shape_problem($re; $max; $label),
  (select(crd_test($re) and overflows) | "\($label) \(tojson) does not fit in 64 bits of nanoseconds");
# Why a value is outside an enum: $set lists the values the CRD allows.
def enum_problem($label; $set): select(IN($set[]) | not) | "\($label) \(tojson)";
# Why a quantity breaks its CRD rules, or nothing; a number is always valid.
def quantity_problem($label):
  if type != "string" then empty
  elif crd_test($qty_re) | not then "\($label) \(tojson)"
  elif length > 64 then "\($label) is longer than 64 characters"
  else empty end;
# The keys of an extra_config object that are in $names.
# The Enterprise-only namespaces of $names in an extra_config at $level that a
# CE render drops: for a namespace CE partly honors (ceHonoredKeys at that
# level), only when the block holds a key CE does not honor, or is not an
# object (renderer.CEDrops).
def eeonly($level; $names):
  (. // {}) as $ec
  | [$ec | keys[] | select(IN($names[])) | . as $ns
    | ($ee[0].ceHonoredKeys[$level][$ns]) as $honored
    | select($honored == null or ($ec[$ns] | type) != "object"
        or ([$ec[$ns] | keys[] | select(IN($honored[]) | not)] | length > 0))];
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
  ((.host // []) | to_entries[] | select(.value | crd_test($host_re) | not)
    | "\($q).host[\(.key)] \(.value | tojson)"),
  (if .policyRef != null and (.policyRef.name // "") == "" then "\($q).policyRef.name is empty" else empty end);
# The route KrakenD registers for an endpoint path: parameter names erased and
# the path cleaned the way its router cleans it (renderer.ConflictKey).
def clean_path:
  split("/") | reduce .[] as $seg ([];
    if $seg == "" or $seg == "." then .
    elif $seg == ".." then .[:-1]
    else . + [$seg] end) | "/" + join("/");
def conflict_key:
  gsub("/\\{[a-zA-Z0-9_-]+\\}"; "/{}") | . as $shape | clean_path as $clean
  | if ($shape | endswith("/")) and ($clean | endswith("/") | not) then $clean + "/" else $clean end;
# The path a gateway serves its health endpoint on, or null when it is disabled.
# A raw router block replaces the typed one whole (the renderer merges raw
# extra_config last), and the route check reads health_path only from the
# merged block, which the Go decoder matches to its keys regardless of case; a
# block that is not an object, or has a key of the wrong type, counts as empty.
# The folding here is ASCII lower-casing, so it can differ from the Go decoder
# for a block that holds the same key in two spellings (for example
# Health_Path: 5 beside health_path: "/a") and for Unicode simple folding
# (a long s in a key). Those are not chased.
def health_path:
  (.spec.config.extraConfig // {}) as $x
  | if ($x | type) != "object" or ($x | has("router") | not)
    then (.spec.config.router.healthPath // "" | if . == "" then "/__health" else . end)
    else ($x.router | if type == "object" then with_entries(.key |= ascii_downcase) else . end) as $r
      | if ($r | type) != "object" or (($r.health_path // "") | type) != "string"
           or (($r.disable_health // false) | type) != "boolean" or (($r.auto_options // false) | type) != "boolean"
        then "/__health"
        elif $r.disable_health == true then null
        elif ($r.health_path | type) == "string" and $r.health_path != "" then $r.health_path
        else "/__health" end
    end;
def report(kind): select(.v | length > 0) | "\(kind) \(.id): \(.v | unique | join("; "))";
'

jq "${jq_opts[@]}" "$jq_lib"'
.items[] | {id: "\(.metadata.namespace)/\(.metadata.name)", v: [
  (if ((.spec.endpoints // []) | length) == 0 then "spec.endpoints is empty" else empty end),
  (if (.spec.gatewayRef.name // "") == "" then "spec.gatewayRef.name is empty" else empty end),
  (if ((.spec.endpoints // []) | length) > 1024 then "more than 1024 entries" else empty end),
  ((.spec.endpoints // []) | group_by([.endpoint, .method])[] | select(length > 1)
    | "duplicate entry \(.[0].method) \(.[0].endpoint | tojson)"),
  ((.spec.endpoints // []) | to_entries[] | .key as $i | .value as $e | "spec.endpoints[\($i)]" as $p | (
    (if ($e.endpoint // "" | crd_test($path_re)) then empty else "\($p).endpoint \($e.endpoint | tojson)" end),
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
  ($s.config.cors.maxAge // empty | select(crd_test($one_re) | not) | "spec.config.cors.maxAge \(tojson)"),
  ($s.config.port // empty | select(. < 1 or . > 65535) | "spec.config.port \(.)"),
  ($s.config.outputEncoding // empty
    | enum_problem("spec.config.outputEncoding"; ["json", "fast-json", "json-collection", "xml", "negotiate", "string", "no-op"])),
  ($s.config.router.healthPath // empty | select(crd_test("^/") | not) | "spec.config.router.healthPath \(tojson)"),
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
  (if $s.edition == "CE" then ($s.config.extraConfig | eeonly("service"; $ee[0].enterpriseOnly.service))[]
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
  ($s.additionalEndpointsBasePath // empty | select(crd_test("^/") | not) | "spec.additionalEndpointsBasePath \(tojson)"),
  (if ($s.additionalEndpointsBasePath // "") != "" and ($s.urlTransform.addPathPrefix // "") != ""
   then "additionalEndpointsBasePath with urlTransform.addPathPrefix" else empty end),
  (if (($s.additionalEndpoints // []) | length) > 256 then "more than 256 additionalEndpoints" else empty end),
  (($s.additionalEndpoints // []) | map(.method //= "GET") | group_by([.endpoint, .method])[] | select(length > 1)
    | "duplicate additionalEndpoint \(.[0].method) \(.[0].endpoint | tojson)"),
  (if (($s.overrides // []) | length) > 1024 then "more than 1024 overrides" else empty end),
  (($s.additionalEndpoints // []) | to_entries[] | .key as $i | .value as $a | "spec.additionalEndpoints[\($i)]" as $p | (
    (if ($a.endpoint // "" | crd_test($path_re)) then empty else "\($p).endpoint \($a.endpoint | tojson)" end),
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
    ($o.endpoint // empty | select(crd_test($path_re) | not) | "\($p).endpoint \(tojson)"),
    (if $o.policyRef != null and ($o.policyRef.name // "") == "" then "\($p).policyRef.name is empty" else empty end)
  )),
  # Overrides collide when they generate one endpoint name: autoconfig.OperationEndpointName is the AutoConfig
  # name, a dash and SanitizeName(operationId), cut to 253 characters. Go lowercases U+212A and U+0130 to
  # ASCII, jq does not.
  (.metadata.name as $acName | ($s.overrides // []) | group_by(
      $acName + "-" + (.operationId | gsub("\u212a"; "k") | gsub("\u0130"; "i") | ascii_downcase
        | gsub("[^a-z0-9-]"; "-") | gsub("^-+|-+$"; "")) | .[0:253] | gsub("-+$"; ""))[]
    | select(length > 1) | "overrides collide: \([.[].operationId | tojson] | join(", "))"),
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
     key: "\(.method) \(.endpoint | conflict_key)", route: "\(.method) \(.endpoint | tojson)",
     ctrl: ([$o.metadata.ownerReferences // [] | .[] | select(.controller == true) | .uid][0]
       | if . == null then null else "\($o.metadata.namespace)/\(.)" end)}]
| group_by([.gw, .key])[] | select(. as $g | [$g[] as $a | $g[] as $b
    | select(($a.owner != $b.owner or $a.route != $b.route)
      and ($a.owner == $b.owner or $a.ctrl == null or $a.ctrl != $b.ctrl))] | length > 0)
| "gateway \(.[0].gw | tojson): \([.[] | "\(.route) (\(.owner))"] | join(" vs "))"' "$work/endpoints.json"

# Two endpoints one AutoConfig controls that share a route (the same method and
# path shape): the gateway serves only one, so the AutoConfig controller holds
# every other it still generates as ConfigValidationFailed (Synced False,
# OperationsFailed) and keeps its stale endpoints until the pair is resolved.
# The audit cannot tell a stale endpoint from a generated one: a pair where one
# is the old endpoint of an operation being renamed is not held, and the old one
# is deleted once the new one is written.
jq "${jq_opts[@]}" "$jq_lib"'
[.items[] | . as $o
  | ([$o.metadata.ownerReferences // [] | .[] | select(.controller == true and .kind == "KrakenDAutoConfig")][0]) as $ref
  | select($ref != null)
  | "\(.spec.gatewayRef.namespace // .metadata.namespace)/\(.spec.gatewayRef.name)" as $gw
  | (.spec.endpoints // [])[]
  | {gw: $gw, ac: "\($o.metadata.namespace)/\($ref.name)", uid: $ref.uid,
     owner: "\($o.metadata.namespace)/\($o.metadata.name)",
     key: "\(.method) \(.endpoint | conflict_key)", route: "\(.method) \(.endpoint | tojson)"}]
| group_by([.ac, .uid, .gw, .key])[] | select([.[].owner] | unique | length > 1)
| "KrakenDAutoConfig \(.[0].ac): endpoints share a route and the operator holds all but the one the gateway serves, unless one of them is a rename in flight: \([.[] | "\(.route) (\(.owner))"] | join(" vs "))"' "$work/endpoints.json"

# GET on a gateway health path: krakend check accepts it, and the controller's
# route check rejects the render, so the gateway keeps its last applied config.
jq "${jq_opts[@]}" --slurpfile gws "$work/gateways.json" "$jq_lib"'
($gws[0].items | map({key: "\(.metadata.namespace)/\(.metadata.name)", value: health_path}) | from_entries) as $health
| .items[] | . as $e
| "\(.spec.gatewayRef.namespace // .metadata.namespace)/\(.spec.gatewayRef.name)" as $gw
| (.spec.endpoints // [])[]
| select(.method == "GET" and $health[$gw] != null and (.endpoint | conflict_key) == ($health[$gw] | conflict_key))
| "KrakenDEndpoint \($e.metadata.namespace)/\($e.metadata.name): GET \(.endpoint | tojson) is the health path of gateway \($gw)"' "$work/endpoints.json"

# Enterprise-only extra_config namespaces on CE gateways: krakend check accepts
# them and KrakenD CE silently ignores them. A policy counts when an endpoint of
# a CE gateway references it.
jq "${jq_opts[@]}" --slurpfile gws "$work/gateways.json" --slurpfile pols "$work/backendpolicies.json" "$jq_lib"'
($ee[0].enterpriseOnly.endpoint - $ee[0].ceRenderDrops.endpoint) as $entry_ee
| ($gws[0].items | map(select(.spec.edition == "CE") | {key: "\(.metadata.namespace)/\(.metadata.name)", value: true})
  | from_entries) as $ce
| ($pols[0].items | map({key: "\(.metadata.namespace)/\(.metadata.name)", value: .}) | from_entries) as $pol
| [.items[] | . as $e
  | "\(.spec.gatewayRef.namespace // .metadata.namespace)/\(.spec.gatewayRef.name)" as $gw
  | select($ce[$gw])
  | (([(.spec.endpoints // []) | to_entries[] | .key as $i | .value as $en
      | (($en.extraConfig | eeonly("endpoint"; $entry_ee)[] | "spec.endpoints[\($i)].extraConfig \(.)"),
         (($en.backends // []) | to_entries[] | .key as $j | .value.extraConfig | eeonly("backend"; $ee[0].enterpriseOnly.backend)[]
           | "spec.endpoints[\($i)].backends[\($j)].extraConfig \(.)"))]
  | select(length > 0)
  | "KrakenDEndpoint \($e.metadata.namespace)/\($e.metadata.name): Enterprise-only on CE gateway \($gw): \(join(", "))"),
     ((.spec.endpoints // [])[] | (.backends // [])[] | .policyRef // empty
      | "\(.namespace // $e.metadata.namespace)/\(.name)" as $key
      | ($pol[$key] // empty) | (.spec.raw | eeonly("backend"; $ee[0].enterpriseOnly.backend)) as $ns | select($ns | length > 0)
      | "KrakenDBackendPolicy \($key): Enterprise-only on CE gateway \($gw): spec.raw \($ns | join(", "))"))]
| unique[]' "$work/endpoints.json"
