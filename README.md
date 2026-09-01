<p align="center">
  <img src="docs/assets/banner.png" alt="presage — forecast-driven autoscaling for Kubernetes and Agones" width="100%">
</p>

<p align="center">
  <a href="https://github.com/GrowlyX/presage/actions/workflows/ci.yaml"><img src="https://github.com/GrowlyX/presage/actions/workflows/ci.yaml/badge.svg" alt="CI"></a>
  <a href="https://artifacthub.io/packages/helm/presage/presage"><img src="https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/presage" alt="Artifact Hub"></a>
  <a href="https://goreportcard.com/report/github.com/GrowlyX/presage"><img src="https://goreportcard.com/badge/github.com/GrowlyX/presage" alt="Go Report Card"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue.svg" alt="Apache 2.0"></a>
  <img src="https://img.shields.io/badge/kubernetes-%E2%89%A5%201.27-326ce5?logo=kubernetes&logoColor=white" alt="Kubernetes >= 1.27">
  <img src="https://img.shields.io/badge/status-alpha-orange.svg" alt="Alpha">
</p>

presage is an autoscaler that provisions for the demand a workload will have
**once new replicas are actually ready**, rather than for demand it has already
observed. It reads a time series from any Prometheus-compatible store,
forecasts it with [TimesFM](https://github.com/google-research/timesfm) —
Google Research's open time-series foundation model, used zero-shot with no
per-cluster training — and turns the resulting predictive distribution into a
replica count.

It drives anything exposing the Kubernetes `scale` subresource, and it drives
[Agones](https://agones.dev) Fleets through the `FleetAutoscaler` webhook,
where Agones remains the sole writer of Fleet replicas.

> **Status: alpha.** The API is `v1alpha1` and will change. Every
> `PredictiveScaler` defaults to `Shadow` mode, which publishes a
> recommendation and changes nothing. That is the intended way to adopt it.

## Why

A reactive autoscaler observes demand at time *T* and starts a replica that
becomes useful at *T + lead*. It is therefore **structurally always one lead
time behind**.

When lead time is a second, nobody notices. When it is two minutes — a JVM
game server loading a world, a model server pulling weights, a pod waiting on
a warm cache — every demand ramp is served late by construction, and the usual
fix is to over-provision permanently until the lateness stops hurting.

presage forecasts demand **at** *T + lead* and sizes for that instead.

```
        demand
          │                    ╭─────  actual
          │                ╭───╯
          │            ╭───╯
          │        ╭───╯    ← a reactive scaler is still here
          │    ╭───╯
          └────┴───────┴───────────────▶ time
               T      T+lead
                 └─ presage sizes for this point
```

## Functionality overview

### Forecasting, without training anything

TimesFM 2.5 is used zero-shot: it has seen enough time series that it
forecasts an unfamiliar one directly, so there is no training pipeline, no
per-workload model, and nothing to retrain when traffic shifts. The 30M
continuous quantile head gives a **predictive distribution** rather than a
point estimate, which is what makes a risk-based scaling policy possible at
all.

The checkpoint revision is pinned by default, and the revision that produced
a forecast is recorded on the object — so a behaviour change that came from
the weights can be told apart from one that came from configuration.

### A baseline in the box

`SeasonalNaive` runs in the controller process: no model server, no
accelerator, no checkpoint download. It is not a placeholder. On clean
weekly-seasonal traffic — which describes most player-facing load — it is a
hard baseline, and a foundation model that cannot beat it on your workload is
not earning its inference cost there.

Both backends sit behind one interface, so that comparison is a config change
rather than a project. A forecasting project that cannot measure itself
against a baseline cannot tell improvement from noise.

### A decision layer with explicit safety properties

A forecast is not a replica count. The layer between them is where presage's
opinions live, and they are deliberate:

* **One quantile sets capacity**, in both directions. A more uncertain
  forecast has a fatter upper tail and therefore provisions *more*.
  Uncertainty should buy headroom, not hesitation.
* **The lower quantile only guards scale-downs.** Releasing capacity is the
  expensive direction to be wrong in, so it is gated on the forecast being
  confident. Adding capacity never is.
* **The reactive floor makes forecast error one-directional.** A conventional
  buffer computation runs alongside the forecast and lower-bounds the result,
  so a badly wrong forecast can only ever *over*-provision. That makes presage
  strictly safer than the reactive policy it replaces.
* **Shadow mode is the default.** A forecasting autoscaler that cannot be
  evaluated before it is trusted will not be trusted.

Every recommendation reports the constraint that bound it, so "why did it pick
that number" is answered by `kubectl describe` rather than by reading logs.

### Agones, with a fallback that actually fires

presage never writes Fleet replicas. It answers the `FleetAutoscaler` webhook
Agones already polls, from a cached recommendation, and returns an **error**
whenever it cannot speak authoritatively — cold start, stale forecast, Shadow
mode, deleted scaler, or a non-leader replica.

That matters because of the `Chain` policy: an error makes Agones fall through
to a plain `Buffer` entry. A well-formed "don't scale" would be worse — Agones
would treat it as authoritative and the fallback would never run. So the
failure mode of a presage outage is *the Fleet reverts to buffer autoscaling*,
not *the Fleet freezes*.

The model is deliberately not on that path. Agones polls every 30 seconds; a
200M-parameter model there would put its tail latency inside Agones' control
loop.

### An evaluation harness, not just a claim

`cmd/backtest` replays your own history through the reactive policy, both
forecast backends, and an oracle with perfect foresight — running the real
policy engine, not a reimplementation of it.

Its headline is an **iso-cost** comparison, because any strategy can reduce
unmet demand by provisioning more. The harness bisects the reactive buffer
until it spends the same as presage, then asks which one is short less often.
If the answer is "within noise", the report says so and tells you to run the
cheaper thing.

Lead time is modelled, with replicas arriving as cohorts. Without that a
reactive strategy looks flawless, and the whole exercise would be theatre.

### Observability

presage exports what it recommended, what the forecast alone said, what a
reactive policy would have said, and which constraint bound the result — so
Shadow mode is genuinely evaluable rather than a promise.

## Getting started

```bash
helm install presage oci://ghcr.io/growlyx/charts/presage \
  --namespace presage-system --create-namespace
```

* [Getting started](docs/getting-started.md) — install, run in Shadow, decide
* [The decision layer](docs/policy.md) — how a forecast becomes a replica count
* [Architecture](docs/architecture.md) — the pieces and why they are separate
* [Agones](docs/agones.md) — Fleet autoscaling and the Chain fallback
* [Backtesting](docs/backtesting.md) — score strategies against your own history
* [Operations](docs/operations.md) — metrics, alerts, troubleshooting
* [API reference](docs/api-reference.md) — generated from the CRDs
* [Examples](examples/)

## Compatibility

| | |
| --- | --- |
| Kubernetes | ≥ 1.27 |
| Agones | ≥ 1.30 (`Chain` policy); the webhook alone works with any version supporting `Webhook` |
| Metrics | Prometheus, Thanos, Mimir, Cortex, VictoriaMetrics |
| Model | TimesFM 2.5 (200M), pinned revision, Apache-2.0 |

Images are published for `linux/amd64` and `linux/arm64`.

## Prior art

| | Model | Self-hosted | Quantiles | Agones |
| --- | --- | --- | --- | --- |
| [PredictKube](https://keda.sh/docs/2.20/scalers/predictkube/) | hosted SaaS API | no | no | no |
| [Predictive HPA](https://github.com/jthomperoo/predictive-horizontal-pod-autoscaler) | Holt-Winters / linear | yes | no | no |
| KEDA + Prophet (tutorials) | Prophet | yes | partial | no |
| **presage** | TimesFM 2.5, zero-shot | **yes** | **yes** | **yes** |

Agones' own `Schedule` policy already covers *known* recurring events you write
down by hand, and is better than a forecast for those — a scheduled event is a
fact, not a prediction. presage covers the drift, the growth trend, and the
day-to-day shape nobody wrote down. They compose in a single `Chain`.

## Honest limitations

* **Zero-shot may only tie the baseline** on a clean weekly curve. Run both and
  use the cheap one if it wins. The covariate path (TimesFM's XReg — events,
  releases, streamer traffic) is where the real lift is, and it is not wired
  up yet.
* **Never run on a real cluster.** envtest has no kubelet, so nothing has
  scheduled a pod or measured a true lead time.
* **Single-signal only.** One PromQL series per scaler.
* **KEDA and HPA adapters are not implemented.** The `scale` subresource path
  covers most of what they would.

## Development

```bash
make verify           # vet, gofmt, unit tests (-race), forecaster, examples, chart
make test-e2e         # controller against a real apiserver + etcd (envtest)
make test-model-e2e   # forecaster against the real TimesFM checkpoint (~1GB)
```

There are three test layers because they catch different things, and the e2e
layers exist because unit tests across packages share assumptions and can be
wrong together. See [CONTRIBUTING.md](CONTRIBUTING.md).

## Community

presage is early and has one maintainer. Issues and pull requests are the way
to reach it; see [MAINTAINERS.md](MAINTAINERS.md) and
[GOVERNANCE.md](GOVERNANCE.md) for what that means in practice, including how
that is meant to change.

* [Contributing](CONTRIBUTING.md)
* [Code of conduct](CODE_OF_CONDUCT.md)
* [Security policy](SECURITY.md) — report vulnerabilities privately
* [Discussions](https://github.com/breezycourses/presage/discussions) for questions

## Model provenance

presage does not vendor model weights. The TimesFM 2.5 checkpoint is pulled
from Hugging Face at startup, and the forecaster **pins a revision** by
default:

```
google/timesfm-2.5-200m-pytorch @ 1d952420fba87f3c6dee4f240de0f1a0fbc790e3
```

Verified 2026-09-01: `apache-2.0`, ungated — the same licence as the code, so
nothing here is encumbered by the weights. Re-check at any time with
`make check-model-license`, which fails if the licence changes or the
checkpoint becomes gated.

Pinning is the default because an unpinned model can change under a running
cluster with no signal at all. Set `PRESAGE_MODEL_REVISION=main` to track
upstream deliberately. Whichever you choose, the revision that produced a
forecast is recorded on the `PredictiveScaler` status, so a behaviour change
that came from the weights can be told apart from one that came from config.

## Licence

Apache-2.0. See [LICENSE](LICENSE).

TimesFM is a separate project, also Apache-2.0, and is not vendored here — the
checkpoint is pulled at runtime from a pinned revision. See
[docs/architecture.md](docs/architecture.md) for what that means operationally.
