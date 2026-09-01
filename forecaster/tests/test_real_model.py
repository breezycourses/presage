"""End-to-end test against the real TimesFM checkpoint.

Opt-in: it downloads ~1GB of weights and takes minutes to compile. Run with

    PRESAGE_E2E=1 pytest tests/test_real_model.py -v

This is the only test that can catch the failure mode the unit tests cannot:
the quantile block layout being different from what the docs describe. Every
other test asserts against a synthetic block built from the same assumption
the production code makes, so they would all agree with each other and all be
wrong together.
"""

from __future__ import annotations

import os

import numpy as np
import pytest

pytestmark = pytest.mark.skipif(
    os.environ.get("PRESAGE_E2E") != "1",
    reason="set PRESAGE_E2E=1 to run against the real checkpoint",
)

HORIZON = 24
CONTEXT = 512

# Layout assertions must run on a series with genuine uncertainty. On a pure
# sine TimesFM's predictive distribution is nearly degenerate (p90-p10 of about
# 1.0 on a value of 51), and the separately-parameterised mean head can sit a
# fraction outside that band without anything being wrong. Noise makes the
# distribution wide enough for the assertions to mean something.
NOISE_SD = 15.0


@pytest.fixture(scope="module")
def model():
    from presage_forecaster.config import Config
    from presage_forecaster.model import TimesFMModel

    m = TimesFMModel(Config(max_context=CONTEXT, max_horizon=HORIZON))
    m.load()
    assert m.ready, f"model failed to load: {m.load_error}"
    return m


def daily_sine(n: int, period: int = 288, amplitude: float = 50, offset: float = 100):
    return [offset + amplitude * np.sin(2 * np.pi * (i % period) / period) for i in range(n)]


def noisy_daily_sine(n: int, sd: float = NOISE_SD, seed: int = 0):
    rng = np.random.default_rng(seed)
    return list(np.asarray(daily_sine(n)) + rng.normal(0, sd, n))


def test_quantile_block_layout_matches_our_assumption(model):
    """The load-bearing assumption: (batch, horizon, 10), index 0 = mean.

    If TimesFM ever reorders that block, every forecast presage makes shifts by
    a decile, and nothing else in the test suite notices -- every other test
    builds its synthetic block from the same assumption the production code
    makes, so they would agree with each other and all be wrong together.
    """
    raw_model = model._model
    point, quantile = raw_model.forecast(
        horizon=HORIZON, inputs=[np.asarray(noisy_daily_sine(CONTEXT), dtype=np.float32)]
    )
    point, quantile = np.asarray(point), np.asarray(quantile)

    assert point.shape == (1, HORIZON), f"unexpected point shape {point.shape}"
    assert quantile.shape == (1, HORIZON, 10), f"unexpected quantile shape {quantile.shape}"

    block = quantile[0]

    # Columns 1..9 must be non-decreasing: they are p10..p90.
    for h in range(HORIZON):
        deciles = block[h, 1:10]
        assert np.all(np.diff(deciles) >= -1e-3), (
            f"deciles not ordered at step {h}: {deciles}"
        )

    # The sharpest available check, and the reason this test earns its runtime:
    # the point forecast is the *median*, so it must coincide with column 5.
    # Any reordering of the block breaks this immediately, whereas a range
    # check on the mean would tolerate an off-by-one.
    for h in range(HORIZON):
        assert abs(point[0, h] - block[h, 5]) < 1e-2, (
            f"step {h}: point forecast {point[0, h]:.4f} does not match column 5 "
            f"{block[h, 5]:.4f}; the quantile block layout may have changed"
        )

    # Column 0 is the mean, so on a distribution with real width it belongs
    # inside [p10, p90] -- not below it, which is what a p0 column would look
    # like. The tolerance is scaled by the spread rather than absolute, because
    # a near-degenerate distribution can legitimately put the separately
    # parameterised mean head a hair outside the band.
    for h in range(HORIZON):
        p10, p90 = block[h, 1], block[h, 9]
        tol = 0.05 * (p90 - p10)
        assert p10 - tol <= block[h, 0] <= p90 + tol, (
            f"column 0 is outside [p10, p90] at step {h}; it may not be the mean"
        )


def test_extract_quantile_against_real_output(model):
    from presage_forecaster.model import _extract_quantile

    raw_model = model._model
    _, quantile = raw_model.forecast(
        horizon=HORIZON, inputs=[np.asarray(noisy_daily_sine(CONTEXT), dtype=np.float32)]
    )
    block = np.asarray(quantile)[0]

    p10 = _extract_quantile(block, 0.1)
    p50 = _extract_quantile(block, 0.5)
    p90 = _extract_quantile(block, 0.9)

    assert np.all(p10 <= p50 + 1e-3), "p10 above p50"
    assert np.all(p50 <= p90 + 1e-3), "p50 above p90"
    # A genuinely wide spread. If these collapse, the quantile head is not
    # active and presage's whole risk policy silently degenerates to a point
    # estimate -- which would still "work" and would still be wrong.
    assert np.mean(p90 - p10) > 1.0, "p90 and p10 are near-identical; quantile head inactive?"

    # And the spread must reflect the *input* uncertainty, not be a constant.
    # A clean series should produce a visibly tighter band than a noisy one.
    _, clean_q = raw_model.forecast(
        horizon=HORIZON, inputs=[np.asarray(daily_sine(CONTEXT), dtype=np.float32)]
    )
    clean_block = np.asarray(clean_q)[0]
    clean_spread = np.mean(
        _extract_quantile(clean_block, 0.9) - _extract_quantile(clean_block, 0.1)
    )
    assert clean_spread < np.mean(p90 - p10), (
        f"clean series spread ({clean_spread:.3f}) is not tighter than noisy "
        f"({np.mean(p90 - p10):.3f}); the model is not expressing uncertainty"
    )


def test_forecasts_a_clean_periodic_series(model):
    """Sanity: on an exactly periodic series the model should be roughly right.

    This is a smoke test, not a benchmark. It exists to catch a model that
    loaded but is producing garbage -- wrong normalisation, wrong dtype, a
    silently failed compile.
    """
    series = daily_sine(CONTEXT)
    results = model.forecast(series=[series], horizon=HORIZON, quantiles=[0.5])

    got = np.asarray(results[0].point)
    want = np.asarray(daily_sine(CONTEXT + HORIZON)[CONTEXT:])

    assert got.shape == (HORIZON,)
    assert np.all(np.isfinite(got)), "forecast contains non-finite values"

    # Normalised RMSE against the true continuation, scaled by the series
    # amplitude. A model that has understood a pure sine should be well under
    # 25%; random output would be far above it.
    nrmse = float(np.sqrt(np.mean((got - want) ** 2)) / 50.0)
    assert nrmse < 0.25, f"forecast is not tracking a clean sine: nRMSE={nrmse:.3f}"


def test_server_round_trip_with_real_model(model):
    """The full HTTP path, with the real model behind it."""
    from fastapi.testclient import TestClient

    from presage_forecaster.config import Config
    from presage_forecaster.server import create_app

    app = create_app(Config(max_context=CONTEXT, max_horizon=HORIZON), model=model)
    resp = TestClient(app).post(
        "/v1/forecast",
        json={
            "series": [
                {"id": "lobby", "values": daily_sine(CONTEXT), "resolution_seconds": 300}
            ],
            "horizon": HORIZON,
            "quantiles": [0.5, 0.9],
        },
    )
    assert resp.status_code == 200, resp.text
    body = resp.json()

    assert body["model"] == "google/timesfm-2.5-200m-pytorch"
    forecast = body["forecasts"][0]
    assert forecast["id"] == "lobby"
    assert len(forecast["point"]) == HORIZON
    # The keys the Go client parses back with strconv.ParseFloat.
    assert set(forecast["quantiles"]) == {"0.5", "0.9"}
    assert len(forecast["quantiles"]["0.9"]) == HORIZON
    assert all(
        q90 >= q50 - 1e-3
        for q50, q90 in zip(forecast["quantiles"]["0.5"], forecast["quantiles"]["0.9"])
    ), "p90 below p50 over the wire"
