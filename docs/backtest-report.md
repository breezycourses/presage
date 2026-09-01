# Backtest

- steps scored: **4033** at 5m0s resolution (336h0m0s of history)
- lead time: **10m0s** (2 steps)
- capacity: **80** signal units per replica
- decisions every **15m0s**

| strategy | avg replicas | cost vs reactive | short on | unmet (total) | p95 unmet | max unmet | scale ops | errors |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| reactive(buffer=10%) | 11.52 | baseline | 1.5% of steps | 1773 | 0.0 | 203.1 | 629 | 0 |
| static(14) | 14.00 | +21.4% | 7.8% of steps | 23237 | 54.4 | 205.6 | 1 | 0 |
| predictive(SeasonalNaive) | 12.47 | +8.2% | 0.2% of steps | 205 | 0.0 | 82.7 | 301 | 0 |
| predictive(TimesFM) | 12.23 | +6.1% | 0.2% of steps | 225 | 0.0 | 82.7 | 269 | 0 |
| oracle (perfect foresight) | 11.51 | -0.1% | 0.8% of steps | 1069 | 0.0 | 203.1 | 646 | 0 |

## Read this way

**Cost** is average provisioned replicas. **Short on** is the share of steps where
demand exceeded provisioned capacity — the service-quality cost of being late.
A strategy is only better if it improves one without giving back the other.

### predictive(SeasonalNaive)

Bought 1568 less unmet demand for 8.2% more replicas. Whether that trade is worth it is a product decision, not a technical one.

**At equal cost.** A reactive policy tuned to the same spend (19.5% buffer, 12.47 replicas)
would have been short on **0.6%** of steps against this strategy's **0.2%**, and carried 707 more unmet demand overall.
Forecasting is earning its keep here.

### predictive(TimesFM)

Bought 1548 less unmet demand for 6.1% more replicas. Whether that trade is worth it is a product decision, not a technical one.

**At equal cost.** A reactive policy tuned to the same spend (15.6% buffer, 12.17 replicas)
would have been short on **0.8%** of steps against this strategy's **0.2%**, and carried 766 more unmet demand overall.
Forecasting is earning its keep here.

