# How presage compares

## Against other predictive autoscalers

| | Model | Self-hosted | Predictive distribution | Agones |
| --- | --- | --- | --- | --- |
| [PredictKube](https://keda.sh/docs/2.20/scalers/predictkube/) | hosted SaaS API | no | no | no |
| [Predictive HPA](https://github.com/jthomperoo/predictive-horizontal-pod-autoscaler) | Holt-Winters / linear regression | yes | no | no |
| KEDA + Prophet (community tutorials) | Prophet | yes | partial | no |
| **presage** | TimesFM 2.5, zero-shot | **yes** | **yes** | **yes** |

The two columns that matter most are the middle ones.

**Self-hosted** means your traffic shape never leaves the cluster. PredictKube
is an open scaler in front of a hosted model: you ship it your metrics.

**Predictive distribution** is what makes a risk-based policy possible at all.
A point forecast can tell you "about 1,600 players". A distribution tells you
"1,600 expected, 1,740 at the 90th percentile", which is the number you
actually want to provision against — and how far apart those two numbers are
tells you how much to trust either.

## Against a plain HPA

If your pods are ready in a second or two, use an HPA. presage exists for
workloads where lead time is long enough to hurt: a JVM game server loading a
world, a model server pulling weights, anything waiting on a warm cache.

Do not point both at the same workload. Two controllers writing the same
replica count will fight every sync period.

## Against Agones' Schedule policy

Agones already has a `Schedule` policy for events you know about — and it is
*better* than a forecast for those. A scheduled event is a fact; a forecast is
a guess. If you know the tournament starts at 19:00 on Saturday, schedule it.

presage covers what nobody wrote down: the drift, the growth trend, the
day-to-day shape, the slow shift in when your players actually log on.

They compose. Put a `Schedule` entry ahead of the presage webhook in the same
`Chain`, with a `Buffer` after it as the fallback.

## Against just over-provisioning

Often the honest answer. If your workload is small, or your traffic is flat, or
a 30% buffer costs less than the engineering time to tune anything, run the
buffer and get on with your life.

[The backtest](backtesting.md) will tell you which situation you are in. It
compares presage against a reactive policy *tuned to the same spend*, and if
forecasting is not buying anything it says so in those words.

## Compatibility

| | |
| --- | --- |
| Kubernetes | ≥ 1.27 |
| Agones | ≥ 1.30 for the `Chain` policy; the webhook alone works with any version supporting `Webhook` |
| Metrics | Prometheus, Thanos, Mimir, Cortex, VictoriaMetrics |
| Model | TimesFM 2.5 (200M), pinned revision, Apache-2.0 |
| Architectures | `linux/amd64`, `linux/arm64` |
