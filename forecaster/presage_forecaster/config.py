"""Runtime configuration, read once from the environment.

Everything here is fixed at process start because TimesFM is *compiled* for a
specific context and horizon: changing either at request time would silently
recompile and stall the request path. Config changes are a pod restart.
"""

from __future__ import annotations

import os
from dataclasses import dataclass


def _env_int(name: str, default: int) -> int:
    raw = os.environ.get(name)
    if raw is None or raw == "":
        return default
    try:
        return int(raw)
    except ValueError as exc:
        raise ValueError(f"{name} must be an integer, got {raw!r}") from exc


def _env_bool(name: str, default: bool) -> bool:
    raw = os.environ.get(name)
    if raw is None or raw == "":
        return default
    return raw.strip().lower() in {"1", "true", "yes", "on"}


@dataclass(frozen=True)
class Config:
    """Forecaster configuration."""

    model: str = "google/timesfm-2.5-200m-pytorch"

    # Compiled input/output sizes. Together with the caller's sample
    # resolution these decide how far back the model sees and how far ahead it
    # can speak: 4096 points at 5m resolution is ~14 days of context.
    # TimesFM 2.5 supports a context up to 16384.
    max_context: int = 4096
    max_horizon: int = 64

    # The 30M continuous quantile head. Without it there is no predictive
    # distribution, and presage's whole risk-based policy collapses to a point
    # estimate.
    use_quantile_head: bool = True

    # Model-side guards that mirror assumptions the controller also enforces.
    normalize_inputs: bool = True
    force_flip_invariance: bool = True
    infer_is_positive: bool = True
    fix_quantile_crossing: bool = True

    # Largest batch accepted in one request, and the largest total number of
    # input points, as a crude memory guard on an endpoint that may be exposed
    # cluster-wide.
    max_batch: int = 64
    max_total_points: int = 1_000_000

    host: str = "0.0.0.0"
    port: int = 8080

    @classmethod
    def from_env(cls) -> "Config":
        return cls(
            model=os.environ.get("PRESAGE_MODEL", cls.model),
            max_context=_env_int("PRESAGE_MAX_CONTEXT", cls.max_context),
            max_horizon=_env_int("PRESAGE_MAX_HORIZON", cls.max_horizon),
            use_quantile_head=_env_bool("PRESAGE_USE_QUANTILE_HEAD", cls.use_quantile_head),
            normalize_inputs=_env_bool("PRESAGE_NORMALIZE_INPUTS", cls.normalize_inputs),
            force_flip_invariance=_env_bool(
                "PRESAGE_FORCE_FLIP_INVARIANCE", cls.force_flip_invariance
            ),
            infer_is_positive=_env_bool("PRESAGE_INFER_IS_POSITIVE", cls.infer_is_positive),
            fix_quantile_crossing=_env_bool(
                "PRESAGE_FIX_QUANTILE_CROSSING", cls.fix_quantile_crossing
            ),
            max_batch=_env_int("PRESAGE_MAX_BATCH", cls.max_batch),
            max_total_points=_env_int("PRESAGE_MAX_TOTAL_POINTS", cls.max_total_points),
            host=os.environ.get("PRESAGE_HOST", cls.host),
            port=_env_int("PRESAGE_PORT", cls.port),
        )
