#!/usr/bin/env python3
"""Check that the pinned TimesFM checkpoint is still Apache-2.0 and ungated.

presage does not vendor the weights, so their licence is not covered by this
repository's. It was Apache-2.0 when checked, but "someone checked once" is not
a property you can rely on -- a model card can be relicensed or gated without
any signal reaching a downstream project.

Deliberately not part of `make verify`: it needs network access, and a
transient Hugging Face outage should not fail an unrelated pull request.
"""

from __future__ import annotations

import json
import sys
import urllib.error
import urllib.request

MODEL = "google/timesfm-2.5-200m-pytorch"
PINNED_REVISION = "1d952420fba87f3c6dee4f240de0f1a0fbc790e3"
EXPECTED_LICENSE = "apache-2.0"

API = f"https://huggingface.co/api/models/{MODEL}"


def main() -> int:
    try:
        with urllib.request.urlopen(API, timeout=30) as resp:
            data = json.load(resp)
    except (urllib.error.URLError, TimeoutError) as exc:
        print(f"could not reach Hugging Face: {exc}", file=sys.stderr)
        return 2  # distinct from a licence failure

    license_ = (data.get("cardData") or {}).get("license")
    gated = data.get("gated")
    sha = data.get("sha")

    print(f"model:    {MODEL}")
    print(f"license:  {license_}")
    print(f"gated:    {gated}")
    print(f"head sha: {sha}")
    print(f"pinned:   {PINNED_REVISION}")

    failures = []
    if license_ != EXPECTED_LICENSE:
        failures.append(f"license is {license_!r}, expected {EXPECTED_LICENSE!r}")
    if gated:
        failures.append(f"checkpoint is now gated ({gated!r}); it was ungated when pinned")

    if failures:
        for f in failures:
            print(f"FAIL: {f}", file=sys.stderr)
        return 1

    if sha != PINNED_REVISION:
        # Not a failure: upstream moving on is normal. Worth saying out loud,
        # because it means the pin is now behind and someone should decide
        # whether to move it.
        print(f"\nnote: upstream has moved past the pinned revision")
        print(f"      presage still loads {PINNED_REVISION}")

    print("\nOK: checkpoint is Apache-2.0 and ungated")
    return 0


if __name__ == "__main__":
    sys.exit(main())
