# presage

Forecast-driven autoscaling for Kubernetes and [Agones](https://agones.dev),
using [TimesFM](https://github.com/google-research/timesfm) — Google Research's
open time-series foundation model — with no per-cluster training.

> **Status: alpha.** The API is `v1alpha1` and will change. Every
> `PredictiveScaler` defaults to `Shadow` mode, which publishes a
> recommendation and changes nothing. That is the intended way to adopt it.

## The problem

A reactive autoscaler observes demand at time *T* and starts a replica that
becomes useful at *T + lead*. It is therefore **structurally always one lead
time behind**. When lead time is a second, nobody notices. When it is two
minutes — a JVM game server loading a world, a model server pulling weights, a
pod waiting on a warm cache — every demand ramp is served late by construction,
and the usual fix is to permanently over-provision until the lateness stops
hurting.

presage forecasts demand **at** *T + lead* and provisions for that instead.

```
        demand
          │                    ╭─────  actual
          │                ╭───╯
          │            ╭───╯
          │        ╭───╯    ← reactive scaler is still here
          │    ╭───╯
          └────┴───────┴───────────────▶ time
               T      T+lead
                 └─ presage sizes for this point
```

## How it works

```
 PredictiveScaler (CRD)
        │
        ├── signal ──────▶ Prometheus / Thanos / Mimir / VictoriaMetrics
        │                  range query → evenly spaced series
        │
        ├── forecast ────▶ ForecastBackend
        │                    • TimesFM 2.5 (200M, zero-shot, quantile head)
        │                    • SeasonalNaive (in-process baseline)
        │
        ├── policy ──────▶ quantile → replicas, reactive floor,
        │                  uncertainty guard, stabilization, rate limits
        │
        └── target ──────▶ • any `scale` subresource (Deployment, …)
                           • Agones Fleet, via the FleetAutoscaler webhook
```

## Quickstart

```bash
kubectl apply -k config
kubectl apply -f examples/00-forecastbackend-seasonal-naive.yaml
kubectl apply -f examples/03-deployment.yaml
```

That runs with **no model server** — `SeasonalNaive` forecasts in-process. Add
TimesFM when you have evidence it beats the baseline on your workload:

```bash
kubectl apply -f examples/01-forecastbackend-timesfm.yaml
```

Then watch the recommendation against reality:

```
presage_recommended_replicas   # what presage would do
presage_current_replicas       # what is actually running
presage_predictive_replicas    # the forecast's unconstrained opinion
presage_reactive_replicas      # what a plain buffer policy would have chosen
```

Flip `mode: Enforce` when the first line has looked right for a few weeks —
including at least one weekend.

## Agones

Agones stays the only writer of Fleet replicas. presage answers the
`FleetAutoscaler` webhook Agones already polls, from a cached recommendation.

The important part is the `Chain` policy:

```yaml
policy:
  type: Chain
  chain:
    - id: predictive
      type: Webhook
      webhook:
        service: {name: presage-webhook, namespace: presage-system, path: /scale/default/lobby}
    - id: fallback
      type: Buffer
      buffer: {bufferSize: 2, minReplicas: 3, maxReplicas: 40}
```

When presage is down, wedged, in `Shadow` mode, still cold-starting, or holding
a recommendation older than `maxRecommendationAge`, the webhook returns an
**error** — and Agones falls through to plain buffer autoscaling. Returning a
well-formed "don't scale" would be worse: Agones would treat it as an
authoritative decision and the fallback would never run.

The model is deliberately **not** on this path. Agones polls every 30s; a
200M-parameter model there would put its tail latency inside Agones' control
loop. The controller refreshes forecasts on its own slower cadence.

See [examples/02-agones-fleet.yaml](examples/02-agones-fleet.yaml).

## Design decisions

**One quantile sets capacity.** An earlier revision used a dead band between
p50 and p90 and only acted outside it. That is backwards: it makes a *more
uncertain* forecast produce *less* movement, and it silently suppresses
scale-ups, defeating the lead-time protection that justifies forecasting at
all. Capacity now tracks `targetQuantile` alone, so wider uncertainty means a
fatter upper tail means more capacity. Uncertainty should buy headroom, not
hesitation.

**The lower quantile guards scale-downs, nothing else.** Releasing capacity is
the expensive direction to be wrong in, so it is gated on the forecast being
confident: if `(p90 − p50) / max(p50, 1)` exceeds `maxRelativeSpread`, the
scale-down is refused. Adding capacity is never gated this way.

**The reactive floor makes forecast error one-directional.** A conventional
buffer computation runs alongside the forecast and lower-bounds the result. With
it enabled, a badly wrong forecast can only ever *over*-provision — which makes
presage strictly safer than the reactive policy it replaces, and is why it is on
by default. (`maxReplicas` and `maxScaleUpRate` remain hard limits and can bind
below the floor; the floor removes forecast error as a cause of
under-provisioning, not operator-configured limits.)

**A baseline ships in the box.** `SeasonalNaive` is not a placeholder. On clean
weekly-seasonal traffic — which describes most player-facing load — it is a
hard baseline, and a foundation model that cannot beat it on your workload is
not earning its inference cost there. Both backends sit behind the same
interface so that comparison is a one-line config change.

**Shadow by default.** A forecasting autoscaler that cannot be evaluated before
it is trusted will not be trusted.

## Resolution is the tuning knob

TimesFM sees a fixed number of *points*, so the signal resolution decides how
far back it can look:

| Resolution | 4096 points | 16384 points (2.5 max) |
| --- | --- | --- |
| 1m | ~2.8 days | ~11 days |
| 5m | **~14 days** | ~57 days |
| 15m | ~43 days | ~170 days |

Weekly seasonality needs at least two or three weeks in view. **5m is the right
default** for most workloads; 1m looks higher-fidelity and quietly throws away
the weekly signal.

## Prior art

| | Model | Self-hosted | Quantiles | Agones |
| --- | --- | --- | --- | --- |
| [PredictKube](https://keda.sh/docs/2.20/scalers/predictkube/) | hosted SaaS API | no | no | no |
| [Predictive HPA](https://github.com/jthomperoo/predictive-horizontal-pod-autoscaler) | Holt-Winters / linear | yes | no | no |
| KEDA + Prophet (tutorials) | Prophet | yes | partial | no |
| **presage** | TimesFM 2.5, zero-shot | **yes** | **yes** | **yes** |

Agones' own `Schedule` policy already covers *known* recurring events you write
down by hand. presage covers the drift, the growth trend, and the day-to-day
shape you did not.

## Testing

```bash
make verify           # vet, gofmt, unit tests (-race), forecaster tests, example schemas
make test-e2e         # controller against a real apiserver + etcd (envtest)
make test-model-e2e   # forecaster against the real TimesFM checkpoint (~1GB)
```

`make test-e2e` runs the controller against a real API server and covers what
unit tests structurally cannot — every component's unit tests build their fakes
from the same assumptions the production code makes, so they can agree with
each other and all be wrong together. It asserts:

- Shadow mode publishes a recommendation and provably does not scale
- Enforce mode scales a real Deployment through the `scale` subresource
- the reactive floor holds when the forecast is badly wrong (quiet a season
  ago, busy now) and the constraint is reported as `ReactiveFloor`
- presage never writes Agones `Fleet.spec.replicas`, and the webhook serves
  the recommendation the reconciler published
- Shadow mode makes the Agones webhook refuse, so a Chain policy falls through
- deleting a scaler stops the webhook immediately and releases the finalizer
- a misconfiguration surfaces on the object, and a failing scaler leaves the
  target untouched
- a mostly-gap-filled signal is refused rather than forecast from

`make test-model-e2e` is the only test that can catch TimesFM changing its
output layout. It asserts the block is `(batch, horizon, 10)`, that the deciles
are ordered, that **the point forecast coincides with column 5** (the median —
any reordering breaks this immediately), and that the p10–p90 spread widens
with input noise rather than being a constant.

## Honest limitations

- **Zero-shot may only tie the baseline** on a clean weekly curve. Run both,
  compare, and use the cheap one if it wins. The covariate path (TimesFM's
  XReg — events, releases, streamer traffic) is where the real lift is, and it
  is not wired up yet.
- **No backtest harness yet.** Evaluating over historical metrics currently
  means running `Shadow` mode forward in real time.
- **Never run on a real cluster.** envtest has no kubelet, so nothing has
  scheduled a pod or measured a true lead time.
- **Single-signal only.** One PromQL series per scaler.
- **KEDA and HPA adapters are not implemented.** The `scale` subresource path
  covers most of what they would.

## Development

```bash
make verify          # vet, gofmt, Go tests, forecaster tests
make generate        # regenerate deepcopy, CRDs, RBAC
make validate-examples
```

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

Apache-2.0. TimesFM is a separate project, also Apache-2.0, and is not vendored
here.
