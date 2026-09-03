# Operations

## Metrics

All exported on the controller's `:8080/metrics`.

| Metric | Type | Use |
| --- | --- | --- |
| `presage_recommended_replicas` | gauge | What presage would apply |
| `presage_current_replicas` | gauge | What the target actually has |
| `presage_predictive_replicas` | gauge | The forecast's unconstrained opinion |
| `presage_reactive_replicas` | gauge | What a plain buffer policy would have chosen |
| `presage_forecast_value` | gauge | Forecast demand at the lead time, by quantile |
| `presage_signal_value` | gauge | Latest observed signal |
| `presage_lead_time_seconds` | gauge | Horizon actually used |
| `presage_signal_gap_steps` | gauge | Steps that were gap-filled |
| `presage_constraint_total` | counter | Evaluations by binding constraint |
| `presage_scale_total` | counter | Applied scales, by direction |
| `presage_reconcile_total` | counter | Reconciles, by outcome |
| `presage_forecast_duration_seconds` | histogram | Backend latency |
| `presage_forecast_errors_total` | counter | Forecast failures, by backend |
| `presage_agones_webhook_requests_total` | counter | Webhook requests, by outcome |
| `presage_crossing_quantiles_total` | counter | Quantile crossing detected, by signal |

## The two questions worth alerting on

**Is presage still deciding anything?** A scaler that reconciles successfully
but is permanently pinned by one constraint is not autoscaling; it is a
constant with extra steps.

```promql
# Pinned at maxReplicas for an hour — the ceiling is too low, and the
# reactive floor cannot protect the workload above it.
sum by (namespace, name) (
  rate(presage_constraint_total{constraint="MaxReplicas"}[1h])
) / sum by (namespace, name) (rate(presage_constraint_total[1h])) > 0.95
```

```promql
# The forecast never wins: everything comes from the reactive floor.
# presage is doing reactive autoscaling with a model attached.
sum by (namespace, name) (
  rate(presage_constraint_total{constraint="ReactiveFloor"}[6h])
) / sum by (namespace, name) (rate(presage_constraint_total[6h])) > 0.9
```

**Is Agones falling through?** The fall-through is the designed failure mode,
so it is not an incident — but a sustained fall-through means you are running
buffer autoscaling and thinking you are running presage.

```promql
sum by (namespace, name) (rate(presage_agones_webhook_requests_total{served="false"}[15m]))
  / sum by (namespace, name) (rate(presage_agones_webhook_requests_total[15m])) > 0.5
```

Check `reason`: `no recommendation`, `stale`, and `shadow mode` mean different
things.

## Why did it pick that number?

`status.breakdown.constraint` answers it directly:

| Constraint | Meaning |
| --- | --- |
| `""` | The forecast bound it. Working as intended. |
| `ReactiveFloor` | Present demand exceeded the forecast; the floor protected you. |
| `ForecastUncertainty` | A scale-down was refused because the p50–p90 spread was too wide. |
| `ScaleDownWindow` | A scale-down is pending and waiting out its window. |
| `MaxScaleUpRate` / `MaxScaleDownRate` | The step was rate-limited. |
| `MinReplicas` / `MaxReplicas` | A hard bound. |

`status.conditions[?(@.type=="Ready")].message` carries a prose trace of the
same decision.

## Troubleshooting

**`Ready=False`, reason `EvaluationFailed`.** The message names the cause.
presage refuses to resize a workload from bad data, so it leaves the target
untouched and reports rather than guessing.

**"query returned N series, expected exactly 1".** Aggregate it. Picking one
of several would make the autoscaler's input depend on response ordering.

**"signal is N% gap-filled".** A series that is mostly interpolation is not
evidence. Either the query is wrong, the scrape interval is coarser than
`spec.signal.resolution`, or the target only just appeared.

**"horizon N exceeds backend maxHorizon M".** Lower the lead time, coarsen the
resolution, or recompile the model server with a larger `PRESAGE_MAX_HORIZON`.
presage errors rather than quietly forecasting less far ahead than you asked,
because that would make the configured lead time a lie.

**Recommendation never changes.** Check `presage_signal_value` is moving at
all, then `status.breakdown.constraint`.

**The forecaster never becomes ready.** It pulls ~1GB on first start.
`readinessProbe.failureThreshold` is 60 for that reason. Liveness probes
`/healthz`, which does not depend on the model — gating liveness on the model
would have the kubelet restart the pod mid-download, forever. Give it a
persistent cache volume to avoid re-downloading on every restart.

## Upgrades

Helm does not upgrade CRDs. After a chart upgrade that changes the API:

```bash
kubectl apply -f https://raw.githubusercontent.com/GrowlyX/presage/v0.1.0/config/crd/scaling.presage.sh_predictivescalers.yaml
kubectl apply -f https://raw.githubusercontent.com/GrowlyX/presage/v0.1.0/config/crd/scaling.presage.sh_forecastbackends.yaml
```

## Turning it off safely

Set `mode: Shadow`. The workload keeps its current replica count and presage
keeps publishing recommendations, so you can see what it *would* have done
while it does nothing.

Deleting a `PredictiveScaler` leaves the target at whatever size it had; for
Agones it also stops the webhook answering immediately, so the Chain falls
through to its fallback rather than waiting for staleness.
