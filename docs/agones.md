# Agones

## presage never writes Fleet replicas

Agones stays the single writer. presage answers the `FleetAutoscaler` webhook
Agones already polls, from a recommendation the controller refreshed on its
own schedule.

This is not deference for its own sake. It is what makes the failure mode
survivable.

## The Chain policy is the point

```yaml
apiVersion: autoscaling.agones.dev/v1
kind: FleetAutoscaler
metadata:
  name: lobby
spec:
  fleetName: lobby
  policy:
    type: Chain
    chain:
      - id: predictive
        type: Webhook
        webhook:
          service:
            name: presage-webhook
            namespace: presage-system
            path: /scale/default/lobby
            port: 8000
      - id: fallback
        type: Buffer
        buffer: {bufferSize: 2, minReplicas: 3, maxReplicas: 40}
  sync:
    type: FixedInterval
    fixedInterval: {seconds: 30}
```

The webhook returns an **error**, not a well-formed response, whenever it
cannot speak authoritatively:

| Situation | Response |
| --- | --- |
| No recommendation yet (cold start) | `503` |
| Recommendation older than `maxRecommendationAge` | `503` |
| Scaler is in `Shadow` mode | `503` |
| Scaler was deleted | `503` |
| This replica is not the leader | `503` |
| Fresh recommendation, `Enforce` mode | `200` with replicas |

Every `503` makes Agones fall through to `fallback`. A well-formed
`scale: false` would be *worse*: Agones would treat it as an authoritative
decision and the fallback would never run.

So the failure mode of a presage outage is "the Fleet reverts to buffer
autoscaling" rather than "the Fleet freezes at its last size".

**Without the fallback entry, that safety property does not exist.** Do not
deploy the webhook entry on its own.

## Every replica serves the webhook

Only the leader reconciles, so only the leader holds fresh recommendations.
The Service still load-balances across all replicas, and a non-leader answers
`503` — which is the fall-through signal, and a much better outcome than a
connection refused.

## Path routing

`/scale/<namespace>/<name>` identifies the Fleet, so one Service backs many
FleetAutoscalers. A bare `/scale` falls back to the namespace and name in the
request body, which is what the stock Agones examples send.

## The signal

Agones exports Fleet state itself, so this works with no instrumentation on
the game server:

```promql
sum(agones_fleets_replicas_count{fleet_name="lobby", type="allocated"})
```

A real player count leads allocations slightly and is worth switching to once
you have one. With `Counters and Lists` enabled, per-GameServer player counts
are first-class in Agones and make a better signal still.

## Lead time

Measure it. For a JVM game server loading a world it is minutes, not seconds,
and it is the whole reason predictive scaling wins here:

```yaml
leadTime:
  source: Observed
  observed:
    query: |
      histogram_quantile(0.95,
        sum by (le) (rate(agones_gameservers_state_duration_bucket{state="Scheduled"}[6h])))
  min: 30s
  max: 10m
```

The clamps are not decoration: an `Observed` query returning nonsense must not
be able to collapse the horizon to zero, which would silently turn presage
into a reactive autoscaler.

## What about the Schedule policy?

Agones' `Schedule` policy already covers *known* recurring events you write
down by hand, and it is better than a forecast for those — a scheduled event
is a fact, not a prediction. presage covers the drift, the growth trend, and
the day-to-day shape nobody wrote down. They compose: put a `Schedule` entry
ahead of the presage webhook in the same `Chain`.
