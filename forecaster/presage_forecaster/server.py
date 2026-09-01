"""HTTP surface for the forecaster.

The wire format is deliberately plain JSON rather than gRPC: it keeps the Go
controller free of a protobuf toolchain, and it means the endpoint can be
poked with curl when a forecast looks wrong at 3am.
"""

from __future__ import annotations

import contextlib
import logging
import math
import threading
import time
from typing import Annotated, Any, AsyncIterator

from fastapi import FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse
from pydantic import BaseModel, Field, field_validator

from .config import Config
from .model import QuantileHeadUnavailable, TimesFMModel

log = logging.getLogger(__name__)


# A NaN or Inf reaching the model produces a NaN forecast, which the
# controller would reject anyway -- but far downstream, with a much worse
# error message. Rejecting it at the edge is enforced with pydantic's own
# constraint rather than a custom validator, so the rejection renders as a
# normal 422 instead of failing to serialise.
FiniteFloat = Annotated[float, Field(allow_inf_nan=False)]


class SeriesIn(BaseModel):
    id: str = Field(min_length=1)
    values: list[FiniteFloat] = Field(min_length=1)
    resolution_seconds: float = Field(gt=0)


class ForecastRequest(BaseModel):
    series: list[SeriesIn] = Field(min_length=1)
    horizon: int = Field(gt=0)
    quantiles: list[float] = Field(default_factory=list)

    @field_validator("quantiles")
    @classmethod
    def _in_range(cls, v: list[float]) -> list[float]:
        for q in v:
            if not 0.0 < q < 1.0:
                raise ValueError(f"quantile {q} must be strictly between 0 and 1")
        return v


class ForecastOut(BaseModel):
    id: str
    point: list[float]
    quantiles: dict[str, list[float]] = Field(default_factory=dict)


class ForecastResponse(BaseModel):
    model: str
    # Which weights produced this. Reported on every response rather than only
    # on /v1/model, so a recorded forecast carries its own provenance and a
    # mid-flight model swap is visible after the fact.
    revision: str | None = None
    latency_ms: float
    forecasts: list[ForecastOut]


def create_app(cfg: Config | None = None, model: TimesFMModel | None = None) -> FastAPI:
    """Build the app.

    ``model`` is injectable so the HTTP surface -- validation, status codes,
    error mapping -- can be tested without torch or a 200M checkpoint.
    """
    cfg = cfg or Config.from_env()
    owns_model = model is None
    model = model or TimesFMModel(cfg)

    @contextlib.asynccontextmanager
    async def lifespan(_: FastAPI) -> AsyncIterator[None]:
        if owns_model:
            # Load off the event loop so the process becomes live immediately
            # and only becomes *ready* once the weights are resident. A 200M
            # model takes tens of seconds to pull and compile; blocking
            # startup on it makes the pod look wedged to the kubelet.
            threading.Thread(target=model.load, name="model-load", daemon=True).start()
        yield

    app = FastAPI(title="presage-forecaster", version="0.1.0", lifespan=lifespan)

    @app.exception_handler(RequestValidationError)
    async def _on_validation_error(_: Request, exc: RequestValidationError) -> JSONResponse:
        """Render validation failures as 422 rather than 500.

        FastAPI's default handler echoes the offending input back in the error
        body. When the offending input is exactly what this endpoint most
        needs to reject -- a NaN or an Inf -- that body is itself not valid
        JSON, and the 422 turns into a 500. Sanitising the echo keeps a
        malformed request a client error.
        """
        return JSONResponse({"error": "invalid request", "detail": _sanitize(exc.errors())},
                            status_code=422)

    @app.get("/healthz")
    def healthz() -> dict[str, str]:
        return {"status": "ok"}

    @app.get("/readyz")
    def readyz() -> JSONResponse:
        if model.ready:
            return JSONResponse({"status": "ready", "model": model.name})
        return JSONResponse(
            {"status": "loading", "error": model.load_error},
            status_code=503,
        )

    @app.get("/v1/model")
    def model_info() -> dict[str, object]:
        return {
            "model": cfg.model,
            "revision": cfg.model_revision,
            "ready": model.ready,
            "max_context": cfg.max_context,
            "max_horizon": cfg.max_horizon,
            "quantile_head": cfg.use_quantile_head,
        }

    @app.post("/v1/forecast")
    def forecast(req: ForecastRequest) -> JSONResponse:
        if len(req.series) > cfg.max_batch:
            return _error(f"batch of {len(req.series)} exceeds max_batch {cfg.max_batch}", 413)

        total = sum(len(s.values) for s in req.series)
        if total > cfg.max_total_points:
            return _error(
                f"request carries {total} points, over max_total_points {cfg.max_total_points}",
                413,
            )

        started = time.monotonic()
        try:
            results = model.forecast(
                series=[s.values for s in req.series],
                horizon=req.horizon,
                quantiles=req.quantiles,
            )
        except QuantileHeadUnavailable as exc:
            return _error(str(exc), 501)
        except ValueError as exc:
            return _error(str(exc), 400)
        except RuntimeError as exc:
            # Not-ready is retryable; the controller should back off, and a
            # Chain-policy caller should fall through to its next entry.
            return _error(str(exc), 503)
        except Exception as exc:  # noqa: BLE001
            log.exception("forecast failed")
            return _error(f"{type(exc).__name__}: {exc}", 500)

        latency_ms = (time.monotonic() - started) * 1000.0
        payload = ForecastResponse(
            model=cfg.model,
            revision=cfg.model_revision,
            latency_ms=latency_ms,
            forecasts=[
                ForecastOut(id=s.id, point=r.point, quantiles=r.quantiles)
                for s, r in zip(req.series, results)
            ],
        )
        return JSONResponse(payload.model_dump())

    return app


def _error(message: str, status: int) -> JSONResponse:
    return JSONResponse({"error": message}, status_code=status)


def _sanitize(value: Any) -> Any:
    """Make a pydantic error structure JSON-encodable.

    Replaces non-finite floats with their textual form and renders anything
    else exotic (exception objects in `ctx`, for instance) as a string.
    """
    if isinstance(value, float):
        return value if math.isfinite(value) else repr(value)
    if isinstance(value, dict):
        return {str(k): _sanitize(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [_sanitize(v) for v in value]
    if isinstance(value, (str, int, bool)) or value is None:
        return value
    return str(value)


def main() -> None:
    import uvicorn

    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    cfg = Config.from_env()
    uvicorn.run(create_app(cfg), host=cfg.host, port=cfg.port, log_level="info")


if __name__ == "__main__":
    main()
