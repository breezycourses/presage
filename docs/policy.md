# The decision layer

A forecast is not a replica count. This is the part that turns one into the
other, and it is where presage's actual opinions live.

Source: [`internal/policy/policy.go`](../internal/policy/policy.go).

## The order of operations

1. **Size to the target quantile.** `replicas = ceil((forecast + headroom) / perReplica)`,
   where `forecast` is the `targetQuantile` of the predictive distribution
   evaluated at the lead time.
2. **Lower-bound by the reactive floor.**
3. **Gate scale-downs** — first on forecast confidence, then on the
   stabilization window. Scale-ups are never gated.
4. **Rate-limit the step**, then clamp to `[minReplicas, maxReplicas]`.

`status.breakdown.constraint` reports which of these bound the result.

## One quantile sets capacity

Capacity tracks `targetQuantile` (default p90) in **both** directions. A more
uncertain forecast has a fatter upper tail and therefore provisions more.

This is worth stating because an earlier revision did it differently and was
wrong. That version put a dead band between p50 and p90 and only acted outside
it. Two things were wrong with that:

* It made a *more uncertain* forecast produce *less* movement. Uncertainty
  should buy headroom, not paralysis.
* It silently suppressed scale-ups whenever current replicas sat inside the
  band — defeating the lead-time protection that is the entire reason to
  forecast.

The regression test is
`TestEvaluate_UncertaintyBuysCapacityNotInaction`.

## The lower quantile only guards scale-downs

`scaleDownQuantile` never sets the target. Its sole job is to measure how
confident the forecast is:

```
spread = (p90 - p50) / max(p50, 1)
```

If that exceeds `scaleDownUncertaintyGuard.maxRelativeSpread`, the scale-down
is refused. Releasing capacity is the expensive direction to be wrong in, so
it is gated on confidence. Adding capacity never is.

## The reactive floor

A conventional buffer computation runs alongside the forecast:

```
reactive = ceil(currentDemand / perReplica) + buffer
recommendation = max(predictive, reactive)
```

With it enabled, **forecast error can only ever cause over-provisioning.** A
badly wrong low forecast cannot starve the workload, which makes presage
strictly safer than the reactive policy it replaces. That is the single
property most worth preserving, and it is why the floor defaults to on.

Two honest caveats:

* `maxReplicas` and `maxScaleUpRate` are hard constraints and can bind below
  the floor. The floor removes *forecast error* as a cause of
  under-provisioning; it does not override limits you configured.
* Disabling the floor is what unlocks scale-to-zero, at the cost of the
  guarantee.

## Asymmetry, everywhere

| | Scale up | Scale down |
| --- | --- | --- |
| Quantile | target (p90) | target (p90) |
| Confidence gate | none | `maxRelativeSpread` |
| Stabilization window | none | `scaleDownWindow` |
| Rate limit | `maxScaleUpRate` | `maxScaleDownRate` |

Under-provisioning costs users; over-provisioning costs money. Those are not
symmetric, so the policy is not either.

## Stabilization

A scale-down must be justified continuously for `scaleDownWindow` before it is
applied. The tracker keys off the recommendation *before* the gates, so
holding at the current count does not reset the window it is waiting on — and
if demand recovers mid-window the timer **restarts** rather than resuming, so
a workload that dips intermittently never accumulates its way into a
scale-down.

## Rate limits and small numbers

A percentage rate resolves to a fractional replica at low counts. Every
resolved step is rounded up with a floor of one, so a workload at zero
replicas with a `100%` rate can still reach one instead of deadlocking at
`0 × 100% = 0`.
