# Contributing to presage

Thanks for considering it. This document covers the practical mechanics; the
design reasoning lives in [docs/architecture.md](docs/architecture.md), and
reading that first will save you time on anything touching the policy engine.

## Getting set up

You need Go 1.25+, [uv](https://docs.astral.sh/uv/), Helm, and Docker.

```bash
git clone https://github.com/breezycourses/presage.git
cd presage
go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
make verify
```

`make verify` is what CI runs: vet, gofmt, unit tests with the race detector,
forecaster tests, example schema validation, and chart validation.

## The test layers, and what each is for

presage has three, and they exist because they catch different things.

**Unit tests** (`make test`) cover each package against fakes it defines
itself. They are fast and they are where the policy engine's properties are
pinned. Their structural weakness: every package's fakes are built from the
same assumptions the production code makes, so they can agree with each other
and all be wrong together.

**Controller e2e** (`make test-e2e`) runs the controller against a real
API server and etcd via envtest. This is what catches the assumptions unit
tests share — it has already found a completely broken scale-subresource path
and a gap-ratio guard that was measuring against the wrong denominator. If you
change anything about how presage talks to the API server, add a case here.

**Model e2e** (`make test-model-e2e`) runs the forecaster against the real
TimesFM checkpoint. It downloads about 1GB, so it is opt-in via
`PRESAGE_E2E=1` and runs in CI only on pushes to `main`. It is the only test
that can catch TimesFM changing its output layout.

A change that only passes unit tests is not necessarily wrong, but if it
touches the API server, the model, or the chart, ask which layer would have
caught it being wrong.

## What good looks like here

**Tests assert behaviour, not implementation.** The valuable tests in this
repo are the ones with a name that states a property — the reactive floor
never under-provisions, uncertainty buys capacity rather than inaction, a
deleted scaler stops answering the webhook. Prefer those to line coverage.

**Comments explain why, not what.** The code says what. Comments earn their
place by recording a decision, a hazard, or a non-obvious constraint — why
liveness must not probe `/readyz`, why the webhook errors instead of returning
`scale: false`. If a comment restates the line below it, delete it.

**Safety changes need a test that fails without them.** Anything touching the
reactive floor, the rate limits, `maxReplicas`, or the Agones fallback path
changes what happens to someone's production workload when presage is wrong.

**New API fields need a reason a default cannot serve.** Every field is
permanent surface area. `v1alpha1` gives us room to change our minds, but not
an excuse to add things speculatively.

## Pull requests

* Branch from `main`. One logical change per PR.
* Run `make verify` before pushing. Run `make test-e2e` if you touched the
  controller.
* If you changed the API types, run `make generate` and `make chart-sync-crds`
  and commit the results. CI fails on drift, because a stale CRD installs a
  schema that rejects fields the controller writes.
* Explain *why* in the PR description. The diff already shows what.
* Say what you did not do. A PR that names its own gaps is easier to trust
  than one that implies completeness.

Commit messages: a short imperative subject, a blank line, then prose
explaining the reasoning. Reference issues with `Fixes #123`.

## Reporting bugs

Include the presage version, Kubernetes version, the `PredictiveScaler` (with
secrets redacted), and `kubectl describe` output — `status.breakdown` names the
constraint that bound the recommendation, which usually answers "why did it
pick that number" immediately.

For security issues, see [SECURITY.md](SECURITY.md) instead. Do not open a
public issue.

## Proposing a design change

Open an issue before the pull request for anything that changes the
`v1alpha1` API, the default behaviour of the policy engine, or the safety
properties in the README. Those are expensive to reverse once deployed.

## Releasing

See [RELEASING.md](RELEASING.md).

## Licence

Contributions are licensed under Apache-2.0, matching the project. By opening a
pull request you agree your contribution may be distributed under that licence.
