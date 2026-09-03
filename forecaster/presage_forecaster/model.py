"""TimesFM wrapper.

Two things here are load-bearing and easy to get wrong:

1.  TimesFM 2.5 returns its quantile forecast as ``(batch, horizon, 10)``
    where index 0 is the **mean** and indices 1..9 are the 10th..90th
    percentiles. Treating index 0 as p0, or index 9 as p100, silently shifts
    every quantile by one decile -- which would look plausible on a dashboard
    and be wrong everywhere.
2.  ``compile()`` is expensive and fixes the context/horizon. It happens once,
    at startup, never on the request path.
"""

from __future__ import annotations

import logging
import threading
import time
from dataclasses import dataclass
from typing import Sequence

import numpy as np

from .config import Config

log = logging.getLogger(__name__)

# Index 0 of the quantile output is the mean; 1..9 are the deciles.
_DECILE_OFFSET = 1
_N_DECILES = 9


@dataclass
class Forecast:
    """One series' forecast."""

    point: list[float]
    quantiles: dict[str, list[float]]


class QuantileHeadUnavailable(RuntimeError):
    """Raised when quantiles are requested but the head is not enabled."""


class TimesFMModel:
    """A loaded, compiled TimesFM model.

    Inference is serialised behind a lock. Torch will happily be re-entered
    from multiple threads and then produce nondeterministic garbage under
    memory pressure; a forecaster that answers a 30s control loop does not
    need the concurrency badly enough to risk that.
    """

    def __init__(self, cfg: Config) -> None:
        self._cfg = cfg
        self._lock = threading.Lock()
        self._model = None
        self._ready = False
        self._load_error: str | None = None
        self._done = threading.Event()

    @property
    def ready(self) -> bool:
        return self._ready

    @property
    def load_error(self) -> str | None:
        return self._load_error

    @property
    def done_event(self) -> threading.Event:
        return self._done

    @property
    def name(self) -> str:
        return self._cfg.model

    @property
    def revision(self) -> str | None:
        return self._cfg.model_revision

    def load(self) -> None:
        """Load and compile the model. Safe to call once, at startup.

        Sets ``_done`` on completion -- success *or* failure -- so that a
        monitoring loop can reliably detect when this thread finishes.
        """
        cfg = self._cfg
        try:
            import timesfm  # imported lazily so config errors surface first
            import torch

            torch.set_float32_matmul_precision("high")

            log.info("loading %s (revision=%s)", cfg.model, cfg.model_revision or "main")
            started = time.monotonic()
            kwargs = {}
            if cfg.model_revision:
                kwargs["revision"] = cfg.model_revision
            model = timesfm.TimesFM_2p5_200M_torch.from_pretrained(cfg.model, **kwargs)

            log.info(
                "compiling (max_context=%d, max_horizon=%d, quantile_head=%s)",
                cfg.max_context,
                cfg.max_horizon,
                cfg.use_quantile_head,
            )
            model.compile(
                timesfm.ForecastConfig(
                    max_context=cfg.max_context,
                    max_horizon=cfg.max_horizon,
                    normalize_inputs=cfg.normalize_inputs,
                    use_continuous_quantile_head=cfg.use_quantile_head,
                    force_flip_invariance=cfg.force_flip_invariance,
                    infer_is_positive=cfg.infer_is_positive,
                    fix_quantile_crossing=cfg.fix_quantile_crossing,
                )
            )
            self._model = model
            self._ready = True
            log.info("model ready in %.1fs", time.monotonic() - started)
        except Exception as exc:  # noqa: BLE001 - surfaced via /readyz
            self._load_error = f"{type(exc).__name__}: {exc}"
            log.exception("failed to load model")
        finally:
            self._done.set()

    def forecast(
        self,
        series: Sequence[Sequence[float]],
        horizon: int,
        quantiles: Sequence[float],
    ) -> list[Forecast]:
        """Forecast a batch of series.

        Each input is truncated to the compiled context length, keeping the
        most recent points.
        """
        if not self._ready or self._model is None:
            raise RuntimeError(f"model not ready: {self._load_error or 'still loading'}")
        if horizon > self._cfg.max_horizon:
            raise ValueError(
                f"horizon {horizon} exceeds compiled max_horizon {self._cfg.max_horizon}"
            )
        if quantiles and not self._cfg.use_quantile_head:
            raise QuantileHeadUnavailable(
                "quantiles requested but the model was compiled without the "
                "continuous quantile head (set PRESAGE_USE_QUANTILE_HEAD=true)"
            )

        inputs = [
            np.asarray(s[-self._cfg.max_context :], dtype=np.float32) for s in series
        ]

        with self._lock:
            point, quantile = self._model.forecast(horizon=horizon, inputs=inputs)

        point = np.asarray(point)
        results: list[Forecast] = []
        for i in range(len(inputs)):
            qs: dict[str, list[float]] = {}
            if quantiles and quantile is not None:
                arr = np.asarray(quantile)[i]  # (horizon, 10)
                for q in quantiles:
                    qs[_format_quantile(q)] = _extract_quantile(arr, q).tolist()
            results.append(Forecast(point=point[i].tolist(), quantiles=qs))
        return results


def _format_quantile(q: float) -> str:
    """Render a quantile as a stable key, matching what the caller asked for."""
    return f"{q:g}"


def _extract_quantile(arr: np.ndarray, q: float) -> np.ndarray:
    """Pull quantile ``q`` out of a ``(horizon, 10)`` TimesFM quantile block.

    Index 0 is the mean and 1..9 are the 10th..90th percentiles, so a decile
    maps to ``round(q * 10)``. Non-decile quantiles are linearly interpolated
    between the neighbouring deciles, and anything outside [0.1, 0.9] clamps
    to the nearest available decile -- the model simply does not carry a p95,
    and extrapolating one would invent confidence it never expressed.
    """
    scaled = q * 10.0
    lo_decile = int(np.floor(scaled))
    hi_decile = int(np.ceil(scaled))

    lo_decile = min(max(lo_decile, 1), _N_DECILES)
    hi_decile = min(max(hi_decile, 1), _N_DECILES)

    lo = arr[:, _DECILE_OFFSET + lo_decile - 1]
    if lo_decile == hi_decile:
        return lo
    hi = arr[:, _DECILE_OFFSET + hi_decile - 1]
    frac = scaled - lo_decile
    return lo * (1.0 - frac) + hi * frac
