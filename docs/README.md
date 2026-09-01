# presage documentation

| | |
| --- | --- |
| [Getting started](getting-started.md) | Install, run a scaler in Shadow mode, read the result |
| [Architecture](architecture.md) | What the pieces are and why they are separate |
| [The decision layer](policy.md) | How a forecast becomes a replica count, and the safety properties |
| [Agones](agones.md) | Fleet autoscaling, and the Chain fallback that makes it safe |
| [Backtesting](backtesting.md) | Score strategies against your own history |
| [Comparison](comparison.md) | Versus HPA, KEDA, PredictKube, Agones Schedule |
| [Operations](operations.md) | Metrics, alerts, and answering "why did it pick that number" |
| [API reference](api-reference.md) | Generated from the CRDs |

Start with [the decision layer](policy.md) if you want to understand what
presage is actually doing. It is the part that matters and the part most
likely to surprise you.
