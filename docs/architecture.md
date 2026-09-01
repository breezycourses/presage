# Architecture

## The problem this shape solves

A reactive autoscaler observes demand at time *T* and starts a replica that
becomes useful at *T + lead*. It is structurally always one lead time behind.
presage forecasts demand **at** *T + lead* and provisions for that.

Everything below follows from that one idea plus the requirement that being
wrong must stay survivable.

## Components

```
 PredictiveScaler (namespaced CRD)      ForecastBackend (cluster-scoped CRD)
        │                                        │
        │  spec.signal                           │  TimesFM | SeasonalNaive
        ▼                                        ▼
 ┌────────────────┐   range query      ┌──────────────────────┐
 │ metrics client │◄──────────────────►│ Prometheus-compatible│
 └───────┬────────┘                    └──────────────────────┘
         │  evenly spaced series
         ▼
 ┌────────────────┐   HTTP             ┌──────────────────────┐
 │ forecast       │◄──────────────────►│ presage-forecaster   │
 │ backend        │                    │ (TimesFM, Python)    │
 └───────┬────────┘                    └──────────────────────┘
         │  p50 / p90 at the lead time
         ▼
 ┌────────────────┐
 │ policy engine  │  quantile → replicas, reactive floor, uncertainty
 │ (pure Go)      │  guard, stabilization window, rate limits, clamps
 └───────┬────────┘
         │  replicas + the constraint that bound them
         ▼
 ┌─────────────────────────────┬──────────────────────────────┐
 │ scale subresource           │ Agones FleetAutoscaler       │
 │ (Deployment, StatefulSet,   │ webhook — presage publishes, │
 │  any CRD implementing it)   │ Agones writes                │
 └─────────────────────────────┴──────────────────────────────┘
```

## Why these are separate

**The policy engine has no Kubernetes or forecasting imports.** It takes plain
numbers and returns a replica count. That is the part most likely to be wrong
and the part whose wrongness is most expensive, so it is a pure function that
can be exhaustively tested. It is the highest-covered package in the repo, and
that is not a coincidence.

**The model runs in a separate process.** The controller stays a small Go
binary with no Python or accelerator dependency, and the model server can be
restarted, scaled, or swapped without touching the component that writes to
the Kubernetes API.

**The model is never on the request path of a control loop.** Agones polls its
webhook every 30 seconds by default. A 200M-parameter model there would put
its tail latency inside Agones' control loop. The controller refreshes
forecasts on its own slower cadence; the webhook serves a cached value.

**Backends are interchangeable.** A foundation model is a means, not the
point. `SeasonalNaive` runs in-process and is a genuinely hard baseline on
weekly-seasonal traffic. Both sit behind one interface so comparing them is a
config change rather than a project — and a project that cannot compare its
model to a baseline cannot tell improvement from noise.

## Signal resolution is the tuning knob

TimesFM sees a fixed number of *points*, so resolution decides how far back it
can look:

| Resolution | 4096 points | 16384 points (2.5 max) |
| --- | --- | --- |
| 1m | ~2.8 days | ~11 days |
| 5m | **~14 days** | ~57 days |
| 15m | ~43 days | ~170 days |

Weekly seasonality needs two to three weeks in view. 5m is the right default;
1m looks higher-fidelity and quietly discards the weekly signal.

## What presage is not

It is not a metrics store, an ML serving platform, or a replacement for HPA
where reactive scaling already works. If your lead time is negligible, presage
has nothing to offer you.
