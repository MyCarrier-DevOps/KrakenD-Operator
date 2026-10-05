#!/usr/bin/env bash
# Runs the admission audit against its fixtures and diffs its output with the
# expected lines. Offline: it needs only bash, jq and diff.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

diff -u "$here/testdata/audit/expected.txt" <("$here/audit-admission-rules.sh" "$here/testdata/audit")
echo "audit fixtures: ok"
