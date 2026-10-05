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
