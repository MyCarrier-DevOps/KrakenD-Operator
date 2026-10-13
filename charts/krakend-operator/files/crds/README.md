# Generated directory — do not hand-edit

The `*.yaml` files in this directory are generated from `operator/config/crd/bases`
(which is itself generated from the Go CRD types under `operator/api/`).

`templates/crds.yaml` renders these files unchanged, apart from the chart labels and
`helm.sh/resource-policy: keep`, which stops `helm uninstall` and `helm rollback` from
deleting the CRDs and, with them, every KrakenD object.

Do not edit the CRD YAML files here directly — edit the Go types instead and run
`make manifests` (from `operator/`), which regenerates `operator/config/crd/bases`
and copies the result into this directory, removing copies of CRDs that no longer
exist. Hand-edits to the `*.yaml` files here are discarded the next time
`make manifests` runs, and `make verify-manifests` will fail the build if this
directory drifts from the generated source.

This README is not touched by the generator (only `*.yaml` files are copied here).
