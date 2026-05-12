#!/usr/bin/env python3
"""Generate kubeconform-compatible JSON Schemas from the CRDs in ../crds.

For each version of each CRD it writes one file into this directory:

    {kind}_{version}.json

Use with kubeconform, e.g.:

    kubeconform \
      -schema-location default \
      -schema-location 'charts/krakend-operator/schemas/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
      my-manifests.yaml
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import yaml

HERE = Path(__file__).resolve().parent
CRDS_DIR = HERE.parent / "crds"


def build_schema(open_api_schema: dict) -> dict:
    schema = json.loads(json.dumps(open_api_schema))  # deep copy
    schema["$schema"] = "http://json-schema.org/draft-07/schema#"
    return schema


def process_crd(crd_path: Path) -> list[Path]:
    with crd_path.open() as fh:
        crd = yaml.safe_load(fh)

    if crd.get("kind") != "CustomResourceDefinition":
        return []

    kind = crd["spec"]["names"]["kind"].lower()
    written: list[Path] = []

    for version in crd["spec"]["versions"]:
        version_name = version["name"]
        open_api = version.get("schema", {}).get("openAPIV3Schema")
        if not open_api:
            continue

        out_path = HERE / f"{kind}_{version_name}.json"
        out_path.write_text(
            json.dumps(build_schema(open_api), indent=2, sort_keys=True) + "\n"
        )
        written.append(out_path)

    return written


def main() -> int:
    if not CRDS_DIR.is_dir():
        print(f"CRD directory not found: {CRDS_DIR}", file=sys.stderr)
        return 1

    crd_files = sorted(CRDS_DIR.glob("*.yaml"))
    if not crd_files:
        print(f"No CRD YAML files found in {CRDS_DIR}", file=sys.stderr)
        return 1

    all_written: list[Path] = []
    for crd_file in crd_files:
        all_written.extend(process_crd(crd_file))

    print(f"Wrote {len(all_written)} schema files to {HERE}:")
    for path in all_written:
        print(f"  - {path.relative_to(HERE)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
