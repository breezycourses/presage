# Getting started

## Install

```bash
helm install presage oci://ghcr.io/growlyx/charts/presage \
  --namespace presage-system --create-namespace
```

Or from source:

```bash
kubectl apply -k config
```

The TimesFM forecaster is **off** by default. That is deliberate: the
in-process `SeasonalNaive` backend costs nothing and is the number a
foundation model has to beat before it earns a node.

## 1. A forecast backend

```yaml
apiVersion: scaling.presage.sh/v1alpha1
kind: ForecastBackend
metadata:
  name: default
spec:
  type: SeasonalNaive
  seasonalNaive:
    season: 168h   # weekly
    cycles: 3
```

## 2. A scaler, in Shadow mode

```yaml
apiVersion: scaling.presage.sh/v1alpha1
kind: PredictiveScaler
metadata:
  name: api
spec:
  mode: Shadow                    # the default: publishes, changes nothing
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: api
  signal:
    prometheus:
      address: http://prometheus-operated.monitoring:9090
      query: sum(rate(http_requests_total{app="api"}[5m]))
    resolution: 5m
    history: 14d
  leadTime:
    source: Static
    static: 90s                   # measure this; do not guess it
  capacity:
    perReplica: "250"             # requests/sec one replica serves
  policy:
    minReplicas: 2
    maxReplicas: 50
    targetQuantile: "0.9"
```

## 3. Read the result

```bash
kubectl get predictivescalers
```

```
NAME   MODE     TARGET   CURRENT   RECOMMENDED   READY
api    Shadow   api      4         7             True
```

`kubectl describe` shows more, including the constraint that bound the number:

```yaml
status:
  currentReplicas: 4
  recommendedReplicas: 7
  breakdown:
    predictive: 7
    reactive: 5
    constraint: ""        # empty means the forecast bound it
  lastForecast:
    backend: SeasonalNaive
    leadTimeSeconds: 90
    point: "1620.400"
    quantiles: {"0.5": "1620.400", "0.9": "1738.100"}
```

## 4. Decide whether to trust it

Compare, over a few weeks including at least one weekend:

```promql
presage_recommended_replicas   # what presage would do
presage_current_replicas       # what actually ran
presage_predictive_replicas    # the forecast's unconstrained opinion
presage_reactive_replicas      # what a plain buffer policy would have chosen
```

The question is not "is the forecast accurate" but "would following it have
been better than what happened". If `recommended` tracks `reactive` almost
exactly, forecasting is buying you nothing on this workload — which is a
useful answer, and cheaper to learn in Shadow mode than in production.

## 5. Enforce

```bash
kubectl patch predictivescaler api --type=merge -p '{"spec":{"mode":"Enforce"}}'
```

Do not point an HPA at the same workload. Two controllers writing the same
replica count will fight every sync period.

## Next

* [The decision layer](policy.md) — what the numbers mean
* [Agones](agones.md) — Fleet autoscaling
* [Operations](operations.md) — metrics and troubleshooting
