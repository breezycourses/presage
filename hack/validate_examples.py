#!/usr/bin/env python3
"""Validate the example manifests against the generated CRD schemas.

Catches field-name drift between the Go types and the docs, which is the
failure mode that makes an example worse than no example.
"""

from __future__ import annotations

import pathlib
import sys

import yaml
from jsonschema import Draft4Validator

ROOT = pathlib.Path(__file__).resolve().parent.parent


def load_schemas() -> dict[tuple[str, str], dict]:
    schemas: dict[tuple[str, str], dict] = {}
    for path in (ROOT / "config" / "crd").glob("*.yaml"):
        crd = yaml.safe_load(path.read_text())
        group = crd["spec"]["group"]
        kind = crd["spec"]["names"]["kind"]
        for version in crd["spec"]["versions"]:
            api_version = f"{group}/{version['name']}"
            schemas[(api_version, kind)] = version["schema"]["openAPIV3Schema"]
    return schemas


def main() -> int:
    schemas = load_schemas()
    failures = 0
    checked = 0
    skipped = 0

    for path in sorted((ROOT / "examples").glob("*.yaml")):
        for doc in yaml.safe_load_all(path.read_text()):
            if not doc:
                continue
            key = (doc.get("apiVersion"), doc.get("kind"))
            if key not in schemas:
                # Foreign kinds (Agones FleetAutoscaler) have no local schema.
                print(f"  skip  {path.name}: {key[1]} (no local CRD)")
                skipped += 1
                continue

            errors = sorted(
                Draft4Validator(schemas[key]).iter_errors(doc),
                key=lambda e: list(e.absolute_path),
            )
            if errors:
                failures += 1
                print(f"  FAIL  {path.name}: {doc['metadata']['name']} ({key[1]})")
                for err in errors:
                    loc = ".".join(str(p) for p in err.absolute_path) or "<root>"
                    print(f"          {loc}: {err.message}")
            else:
                checked += 1
                print(f"  ok    {path.name}: {doc['metadata']['name']} ({key[1]})")

    print(f"\n{checked} valid, {failures} invalid, {skipped} skipped")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
