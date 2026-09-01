#!/usr/bin/env python3
"""Validate the example manifests against the generated CRD schemas.

Catches field-name drift between the Go types and the docs, which is the
failure mode that makes an example worse than no example.
"""

from __future__ import annotations

import pathlib
import re
import sys

import yaml
from jsonschema import Draft4Validator

# Go's time.ParseDuration understands ns, us/\u00b5s, ms, s, m, h -- and nothing
# larger. "14d" looks obviously correct, is accepted by a CRD schema (it is
# just a string), and then fails to decode at runtime with
# `unknown unit "d" in duration "14d"`, taking the watch down with it.
GO_DURATION = re.compile(r"^-?(\d+(\.\d+)?(ns|us|\u00b5s|ms|s|m|h))+$")
DURATION_SHAPED = re.compile(r"^-?\d+(\.\d+)?[a-z\u00b5]{1,2}$")


def bad_durations(node, path=""):
    """Yield (path, value) for strings that look like durations but are not."""
    if isinstance(node, dict):
        for k, v in node.items():
            yield from bad_durations(v, f"{path}.{k}" if path else str(k))
    elif isinstance(node, list):
        for i, v in enumerate(node):
            yield from bad_durations(v, f"{path}[{i}]")
    elif isinstance(node, str):
        if DURATION_SHAPED.match(node) and not GO_DURATION.match(node):
            yield path, node

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


def check_crd_defaults() -> int:
    """CRD defaults are applied by the API server and must decode in Go."""
    failures = 0
    for path in sorted((ROOT / "config" / "crd").glob("*.yaml")):
        crd = yaml.safe_load(path.read_text())
        for where, value in bad_durations(crd):
            if "default" in where or "example" in where:
                print(f"  FAIL  {path.name}: {where} = {value!r} is not a Go duration")
                failures += 1
    if not failures:
        print("  ok    CRD defaults are all valid Go durations")
    return failures


def main() -> int:
    schemas = load_schemas()
    failures = check_crd_defaults()
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
            for where, value in bad_durations(doc):
                failures += 1
                print(f"  FAIL  {path.name}: {where} = {value!r} is not a Go duration "
                      f"(no day unit; use hours)")
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
