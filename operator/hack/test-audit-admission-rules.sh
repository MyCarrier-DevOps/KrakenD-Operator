#!/usr/bin/env bash
# Runs the admission audit against its fixtures and diffs its output with the
# expected lines. Offline: it needs only bash, jq and diff.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

diff -u "$here/testdata/audit/expected.txt" <("$here/audit-admission-rules.sh" "$here/testdata/audit")

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
