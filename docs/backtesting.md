# Backtesting

Replay a real signal through several scaling strategies and see what each
would have done. This answers "would presage have been better than what we do
today" in minutes, instead of running Shadow mode forward for weeks and
getting one sample of one configuration.

```bash
go run ./cmd/backtest \
  -address http://vmselect.monitoring:8481/select/0/prometheus \
  -query 'sum(agones_fleets_replicas_count{fleet_name="lobby",type="allocated"})' \
  -window 504h -resolution 5m \
  -lead-time 2m -interval 1m \
  -per-replica 1 -min-replicas 3 -max-replicas 40 \
  -timesfm http://presage-forecaster:8080
```

Omit `-timesfm` to compare only the in-process baseline, which needs no model
server.

## What it compares

| Strategy | What it is |
| --- | --- |
| `reactive(buffer=…)` | A conventional buffer autoscaler. What presage replaces. |
| `static(n)` | A hard-coded replica count, if you pass `-static`. |
| `predictive(SeasonalNaive)` | The real policy engine, in-process baseline forecaster. |
| `predictive(TimesFM)` | The real policy engine, real model server. |
| `oracle` | Perfect foresight. Cheats deliberately — it sets the scale of the prize. |

The predictive strategies run the **actual** forecasting code and the **actual**
policy engine, so a backtest reflects what would run rather than a
reimplementation that could drift from it.

## Why the iso-cost comparison is the headline

Any strategy can reduce unmet demand by provisioning more. "presage had 86%
less unmet demand" is not a result if presage also ran 10% more replicas — a
reactive policy with a bigger buffer would have done the same for the same
money, possibly better.

So the harness bisects the reactive buffer until its average replica count
matches each predictive strategy, and reports the comparison **at equal
spend**:

```
**At equal cost.** A reactive policy tuned to the same spend (19.5% buffer, 11.44 replicas)
would have been short on 1.3% of steps against this strategy's 0.1%, and carried
667 more unmet demand overall.
```

That is a claim worth acting on. If the report instead says forecasting is
"within noise", believe it and run the cheaper thing.

## Lead time is modelled, and it matters

Without it a reactive strategy looks flawless: it observes demand and is
credited with serving it instantly. Since lead time is the entire reason
forecasting is worth anything, a backtest that omits it would report that
presage is pointless.

Replicas arrive as **cohorts**: when the desired count rises from A to B, the
B−A new pods start booting and pods already booting keep their own ETAs — they
do not restart. Modelling it as a single pending target instead means
re-asserting the same count each reconcile pushes the arrival back a step every
time, so on any rising signal the scale-up never lands and every strategy looks
equally bad.

Scale-downs are immediate, because releasing a replica is fast.

Note that even perfect foresight cannot beat lead time from a cold start: its
first cohort cannot land before `t + lead`. That is why `minReplicas` exists.

## Reading the numbers

* **avg replicas** — the cost proxy.
* **short on** — share of steps where demand exceeded provisioned capacity.
* **unmet (total)** — summed shortfall, so many small misses and one large one
  are distinguishable when read against `max unmet`.
* **scale ops** — churn. Connection draining and cold caches are real costs
  that neither headline number captures.
* **errors** — failed decisions, which hold the previous count exactly as the
  controller does.

## Caveats

* The signal is replayed as observed, which means it is the demand that
  occurred **under whatever scaling you were running at the time**. If that
  system shed load, the trace understates true demand and every strategy is
  scored against a signal already shaped by its predecessor. There is no fix
  for this short of a load test; be aware of it before treating a result as
  precise.
* Warmup reserves two full seasons of history that strategies may read but are
  not scored on, so the naive baseline has whole cycles to repeat.
* `-interval` should match the `interval` you would actually run. Deciding
  more often than reality flatters every strategy.
