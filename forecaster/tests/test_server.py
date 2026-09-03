"""Tests for the HTTP surface, with a stub model (no torch required)."""

import asyncio
import json
import sys
import threading

import pytest
from fastapi.testclient import TestClient

from presage_forecaster.config import Config
from presage_forecaster.model import Forecast, QuantileHeadUnavailable
from presage_forecaster.server import create_app, _watch_model_load


class StubModel:
    def __init__(self, ready=True, raises=None, load_error=None):
        self._ready = ready
        self._raises = raises
        self.name = "stub-model"
        self._load_error = load_error
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

    def load(self):
        pass

    def forecast(self, series, horizon, quantiles):
        if self._raises:
            raise self._raises
        return [
            Forecast(
                point=[1.0] * horizon,
                quantiles={f"{q:g}": [2.0] * horizon for q in quantiles},
            )
            for _ in series
        ]


def client(model):
    return TestClient(create_app(Config(), model=model))


def test_forecast_reports_model_revision():
    """Provenance must ride on the response, not only on /v1/model.

    A recorded forecast has to be explainable later: if the weights changed,
    the number it produced should say so.
    """
    cfg = Config(model_revision="deadbeef")
    resp = TestClient(create_app(cfg, model=StubModel())).post(
        "/v1/forecast",
        json={
            "series": [{"id": "a", "values": [1, 2], "resolution_seconds": 300}],
            "horizon": 2,
        },
    )
    assert resp.status_code == 200
    assert resp.json()["revision"] == "deadbeef"


def test_unpinned_revision_is_reported_as_null():
    cfg = Config(model_revision=None)
    resp = TestClient(create_app(cfg, model=StubModel())).post(
        "/v1/forecast",
        json={
            "series": [{"id": "a", "values": [1, 2], "resolution_seconds": 300}],
            "horizon": 2,
        },
    )
    assert resp.json()["revision"] is None


def test_forecast_round_trip():
    resp = client(StubModel()).post(
        "/v1/forecast",
        json={
            "series": [{"id": "lobby", "values": [1, 2, 3], "resolution_seconds": 300}],
            "horizon": 3,
            "quantiles": [0.5, 0.9],
        },
    )
    assert resp.status_code == 200
    body = resp.json()
    assert body["forecasts"][0]["id"] == "lobby"
    assert body["forecasts"][0]["quantiles"]["0.9"] == [2.0, 2.0, 2.0]
    assert body["latency_ms"] >= 0


def test_readyz_is_503_while_loading():
    resp = client(StubModel(ready=False)).get("/readyz")
    assert resp.status_code == 503


def test_healthz_is_ok_while_loading():
    # Liveness must not depend on the model, or the kubelet restarts the pod
    # mid-download and it never finishes loading.
    resp = client(StubModel(ready=False)).get("/healthz")
    assert resp.status_code == 200


def test_model_not_ready_is_retryable_503():
    model = StubModel(raises=RuntimeError("model not ready: still loading"))
    resp = client(model).post(
        "/v1/forecast",
        json={
            "series": [{"id": "a", "values": [1, 2], "resolution_seconds": 300}],
            "horizon": 2,
        },
    )
    assert resp.status_code == 503


def test_missing_quantile_head_is_501():
    model = StubModel(raises=QuantileHeadUnavailable("no head"))
    resp = client(model).post(
        "/v1/forecast",
        json={
            "series": [{"id": "a", "values": [1, 2], "resolution_seconds": 300}],
            "horizon": 2,
            "quantiles": [0.9],
        },
    )
    assert resp.status_code == 501


@pytest.mark.parametrize(
    "payload",
    [
        {"series": [], "horizon": 2},
        {"series": [{"id": "a", "values": [], "resolution_seconds": 300}], "horizon": 2},
        {"series": [{"id": "a", "values": [1], "resolution_seconds": 0}], "horizon": 2},
        {"series": [{"id": "a", "values": [1], "resolution_seconds": 300}], "horizon": 0},
        {
            "series": [{"id": "a", "values": [1], "resolution_seconds": 300}],
            "horizon": 2,
            "quantiles": [1.5],
        },
    ],
)
def test_invalid_requests_are_rejected(payload):
    assert client(StubModel()).post("/v1/forecast", json=payload).status_code == 422


def test_non_finite_values_are_rejected():
    # NaN would propagate all the way to the controller before being caught.
    # The 422 body must itself be valid JSON: FastAPI's default handler echoes
    # the offending input, and a raw NaN there turns the 422 into a 500.
    resp = client(StubModel()).post(
        "/v1/forecast",
        content='{"series":[{"id":"a","values":[1,NaN],"resolution_seconds":300}],"horizon":2}',
        headers={"Content-Type": "application/json"},
    )
    assert resp.status_code == 422
    body = resp.json()  # raises if the error body is not JSON-encodable
    assert body["error"] == "invalid request"
    assert "nan" in json.dumps(body["detail"]).lower()


def test_batch_limit_enforced():
    cfg = Config(max_batch=2)
    app = create_app(cfg, model=StubModel())
    resp = TestClient(app).post(
        "/v1/forecast",
        json={
            "series": [
                {"id": f"s{i}", "values": [1, 2], "resolution_seconds": 300} for i in range(3)
            ],
            "horizon": 2,
        },
    )
    assert resp.status_code == 413


def test_watch_model_load_exits_on_load_error():
    model = StubModel(load_error="Something went wrong", ready=False)

    with pytest.raises(SystemExit) as exc_info:
        asyncio.run(_watch_model_load(model, timeout=5.0))

    assert exc_info.value.code == 1


def test_watch_model_load_exits_on_timeout():
    model = StubModel(ready=False)

    with pytest.raises(SystemExit) as exc_info:
        asyncio.run(_watch_model_load(model, timeout=0.1))

    assert exc_info.value.code == 1


def test_watch_model_load_succeeds_when_model_ready():
    model = StubModel(ready=True)
    model._done.set()

    asyncio.run(_watch_model_load(model, timeout=5.0))
