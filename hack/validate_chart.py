#!/usr/bin/env python3
"""Render the Helm chart across value permutations and assert its invariants.

`helm lint` only checks that templates parse. The properties that actually
matter here are semantic: that disabling Agones really removes the RBAC for a
group that may not exist on the cluster, that the webhook Service selects
every controller replica rather than just the leader, and that a
half-configured TLS block fails at render time instead of at runtime.
"""

from __future__ import annotations

import pathlib
import subprocess
import sys

import yaml

ROOT = pathlib.Path(__file__).resolve().parent.parent
CHART = ROOT / "charts" / "presage"


def render(**values) -> list[dict]:
    args = ["helm", "template", "presage", str(CHART), "--namespace", "presage-system"]
    for k, v in values.items():
        args += ["--set", f"{k}={v}"]
    out = subprocess.run(args, capture_output=True, text=True)
    if out.returncode != 0:
        raise RuntimeError(f"helm template failed: {out.stderr.strip()}")
    return [d for d in yaml.safe_load_all(out.stdout) if d]


def render_expecting_failure(**values) -> str:
    args = ["helm", "template", "presage", str(CHART), "--namespace", "presage-system"]
    for k, v in values.items():
        args += ["--set", f"{k}={v}"]
    out = subprocess.run(args, capture_output=True, text=True)
    if out.returncode == 0:
        raise AssertionError(f"expected a render failure for {values}")
    return out.stderr


def by_kind(docs: list[dict], kind: str) -> list[dict]:
    return [d for d in docs if d.get("kind") == kind]


def one(docs: list[dict], kind: str, name_contains: str = "") -> dict:
    found = [d for d in by_kind(docs, kind) if name_contains in d["metadata"]["name"]]
    assert len(found) == 1, f"expected exactly 1 {kind}~{name_contains}, got {len(found)}"
    return found[0]


CHECKS: list[tuple[str, callable]] = []


def check(name):
    def wrap(fn):
        CHECKS.append((name, fn))
        return fn
    return wrap


@check("default install produces a working controller")
def _():
    docs = render()
    dep = one(docs, "Deployment")
    c = dep["spec"]["template"]["spec"]["containers"][0]
    assert "--leader-elect" in c["args"], "leader election should be on by default"
    assert any(a.startswith("--agones-webhook=true") for a in c["args"])
    assert c["securityContext"]["readOnlyRootFilesystem"] is True
    assert dep["spec"]["template"]["spec"]["securityContext"]["runAsNonRoot"] is True


@check("the forecaster is off by default")
def _():
    docs = render()
    assert not [d for d in by_kind(docs, "Deployment") if "forecaster" in d["metadata"]["name"]], (
        "the TimesFM forecaster must be opt-in; the in-process baseline is the "
        "right starting point and costs nothing"
    )


@check("disabling Agones removes every agones.dev RBAC rule")
def _():
    docs = render(**{"rbac.agones": "false"})
    role = one(docs, "ClusterRole")
    groups = {g for rule in role["rules"] for g in rule["apiGroups"]}
    assert "agones.dev" not in groups, (
        "RBAC must not reference a group that may not exist on the cluster"
    )


@check("presage never gets write access to workloads themselves")
def _():
    docs = render()
    role = one(docs, "ClusterRole")
    for rule in role["rules"]:
        if "apps" in rule["apiGroups"]:
            writes = {"update", "patch", "create", "delete"} & set(rule["verbs"])
            for res in rule["resources"]:
                if writes and not res.endswith("/scale"):
                    raise AssertionError(
                        f"write verbs {sorted(writes)} on {res}; presage changes "
                        "replica counts through /scale only"
                    )


@check("presage only ever reads Agones Fleets")
def _():
    docs = render()
    role = one(docs, "ClusterRole")
    for rule in role["rules"]:
        if "agones.dev" in rule["apiGroups"]:
            assert set(rule["verbs"]) <= {"get", "list", "watch"}, (
                "Agones must remain the sole writer of Fleet replicas"
            )


@check("the webhook Service selects all controller replicas, not just the leader")
def _():
    docs = render()
    svc = one(docs, "Service", "webhook")
    dep = one(docs, "Deployment")
    selector = svc["spec"]["selector"]
    labels = dep["spec"]["template"]["metadata"]["labels"]
    assert selector.items() <= labels.items(), "webhook Service does not select the controller pods"
    assert selector.get("app.kubernetes.io/component") == "controller"


@check("forecaster liveness does not depend on the model")
def _():
    docs = render(**{"forecaster.enabled": "true"})
    dep = one(docs, "Deployment", "forecaster")
    c = dep["spec"]["template"]["spec"]["containers"][0]
    assert c["livenessProbe"]["httpGet"]["path"] == "/healthz", (
        "liveness must not gate on /readyz: a 200M checkpoint takes tens of "
        "seconds to load and the kubelet would restart it mid-download forever"
    )
    assert c["readinessProbe"]["httpGet"]["path"] == "/readyz"


@check("the forecaster pins a checkpoint revision by default")
def _():
    docs = render(**{"forecaster.enabled": "true"})
    dep = one(docs, "Deployment", "forecaster")
    env = {e["name"]: e.get("value") for e in dep["spec"]["template"]["spec"]["containers"][0]["env"]}
    rev = env.get("PRESAGE_MODEL_REVISION", "")
    assert len(rev) == 40, f"expected a pinned 40-char revision, got {rev!r}"


@check("half-configured TLS fails at render time")
def _():
    err = render_expecting_failure(**{"agonesWebhook.tls.enabled": "true"})
    assert "secretName is required" in err, err


@check("metrics can be turned off entirely")
def _():
    docs = render(**{"metrics.enabled": "false"})
    assert not [d for d in by_kind(docs, "Service") if "metrics" in d["metadata"]["name"]]
    c = one(docs, "Deployment")["spec"]["template"]["spec"]["containers"][0]
    assert "--metrics-bind-address=0" in c["args"]


@check("CRDs ship with the chart")
def _():
    crds = sorted((CHART / "crds").glob("*.yaml"))
    names = {yaml.safe_load(p.read_text())["metadata"]["name"] for p in crds}
    assert names == {
        "predictivescalers.scaling.presage.sh",
        "forecastbackends.scaling.presage.sh",
    }, f"unexpected CRDs in the chart: {names}"


@check("chart CRDs match the generated ones")
def _():
    # Helm never upgrades CRDs, so a chart CRD drifting behind config/crd would
    # install a schema that rejects fields the controller writes.
    for generated in sorted((ROOT / "config" / "crd").glob("*.yaml")):
        shipped = CHART / "crds" / generated.name
        assert shipped.exists(), f"{generated.name} is missing from the chart"
        assert shipped.read_text() == generated.read_text(), (
            f"{generated.name} in the chart has drifted from config/crd; "
            "run `make chart-sync-crds`"
        )


def main() -> int:
    failures = 0
    for name, fn in CHECKS:
        try:
            fn()
            print(f"  ok    {name}")
        except Exception as exc:  # noqa: BLE001
            failures += 1
            print(f"  FAIL  {name}\n          {exc}")
    print(f"\n{len(CHECKS) - failures} passed, {failures} failed")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
