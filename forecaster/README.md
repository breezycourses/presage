# presage-forecaster

TimesFM model server for [presage](../README.md).

```bash
uv venv && source .venv/bin/activate
uv pip install -e '.[torch,dev]'
presage-forecaster
```

Configuration is environment-only and fixed at startup, because TimesFM is
compiled for a specific context and horizon:

| Variable | Default | Notes |
| --- | --- | --- |
| `PRESAGE_MODEL` | `google/timesfm-2.5-200m-pytorch` | HF checkpoint |
| `PRESAGE_MAX_CONTEXT` | `4096` | input points; 2.5 supports up to 16384 |
| `PRESAGE_MAX_HORIZON` | `64` | output points |
| `PRESAGE_USE_QUANTILE_HEAD` | `true` | the 30M continuous quantile head |
| `PRESAGE_PORT` | `8080` | |

At 5-minute resolution, `max_context=4096` is ~14 days of history and
`max_horizon=64` is ~5 hours ahead.
