#!/usr/bin/env bash
# Runs the admission audit against its fixtures and diffs its output with the
# expected lines. Offline: it needs only bash, jq and diff.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

diff -u "$here/testdata/audit/expected.txt" <("$here/audit-admission-rules.sh" "$here/testdata/audit")

# The patterns and enum lists the audit checks must be the ones in the CRDs.
crds="$here/../config/crd/bases"
flat="$(cat "$crds"/*.yaml | tr -s ' \n' ' ')"
for var in endpoint_path_re go_duration_re single_unit_re quantity_re; do
	pattern="$(sed -n "s/^$var='\(.*\)'\$/\1/p" "$here/audit-admission-rules.sh")"
	[[ -n "$pattern" ]] && grep -qxE -- "(- )?pattern: $(sed 's/[][\.*^$|?+(){}]/\\&/g' <<<"$pattern")" <(cat "$crds"/*.yaml | sed 's/^ *//') || {
		echo "$var is not a pattern in the CRDs: $pattern" >&2
		exit 1
	}
done
# Every CRD field the audit checks must carry the pattern the audit applies to
# it: the number of fields of that name carrying the pattern is pinned.
pattern_of() { sed -n "s/^$1='\\(.*\\)'\$/\\1/p" "$here/audit-admission-rules.sh"; }
for pin in "endpoints timeout 1 go_duration_re" "endpoints cacheTTL 1 go_duration_re" "endpoints endpoint 1 endpoint_path_re" \
	"autoconfigs timeout 3 go_duration_re" "autoconfigs cacheTTL 3 go_duration_re" "autoconfigs interval 1 go_duration_re" \
	"autoconfigs endpoint 2 endpoint_path_re" "gateways timeout 1 single_unit_re" "gateways cacheTTL 1 single_unit_re" \
	"gateways dnsCacheTTL 1 single_unit_re" "gateways maxAge 1 single_unit_re" "gateways dialTimeout 1 single_unit_re" \
	"gateways tmpSizeLimit 1 quantity_re"; do
	read -r kind key count var <<<"$pin"
	# The pattern goes through the environment: awk -v would process its escapes.
	got="$(want="$(pattern_of "$var")" awk -v key="$key" '
		{ line = $0; sub(/^ +/, "", line); match($0, /^ */); indent = RLENGTH }
		inside && indent <= keyindent { inside = 0 }
		line == key ":" { inside = 1; keyindent = indent; counted = 0 }
		inside && !counted && (line == "pattern: " ENVIRON["want"] || line == "- pattern: " ENVIRON["want"]) { n++; counted = 1 }
		END { print n + 0 }' "$crds/gateway.krakend.io_krakend$kind.yaml")"
	[[ "$got" == "$count" ]] || {
		echo "$got $key fields of the $kind CRD carry $var, want $count" >&2
		exit 1
	}
done
while IFS= read -r list; do
	items="$(grep -o '"[^"]*"' <<<"$list" | tr -d '"' | sed 's/^/- /' | tr '\n' ' ')"
	grep -qE -- "enum: ${items}[a-zA-Z]" <<<"$flat" || {
		echo "no CRD enum matches the audit's list: $list" >&2
		exit 1
	}
done < <(grep -o '\[\("[^"]*"\(, \)\?\)\+\]' "$here/audit-admission-rules.sh")

# Cluster mode: a stub kubectl serves the fixtures and records its arguments.
# The audit must ask for each kind once, and only with `get`.
stub="$(mktemp -d)"
trap 'rm -rf "$stub"' EXIT
cat >"$stub/kubectl" <<STUB
#!/usr/bin/env bash
echo "\$*" >>"$stub/calls"
[[ "\$1" == get ]] || exit 1
resource="\${2%.gateway.krakend.io}"
cat "$here/testdata/audit/\${resource#krakend}.json"
STUB
chmod +x "$stub/kubectl"
diff -u "$here/testdata/audit/expected.txt" <(PATH="$stub:$PATH" "$here/audit-admission-rules.sh")
[[ "$(sort "$stub/calls")" == "$(printf 'get krakendautoconfigs.gateway.krakend.io -A -o json\nget krakendbackendpolicies.gateway.krakend.io -A -o json\nget krakendendpoints.gateway.krakend.io -A -o json\nget krakendgateways.gateway.krakend.io -A -o json')" ]] || {
	echo "unexpected kubectl calls:" >&2
	cat "$stub/calls" >&2
	exit 1
}
echo "audit fixtures: ok"
