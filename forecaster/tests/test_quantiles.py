"""Tests for the quantile block layout.

This is the highest-risk piece of glue in the forecaster: an off-by-one in the
decile mapping produces forecasts that look entirely reasonable on a dashboard
and are wrong by a full decile everywhere.
"""

import numpy as np
import pytest

from presage_forecaster.model import _extract_quantile, _format_quantile

HORIZON = 4


def block() -> np.ndarray:
    """A (horizon, 10) TimesFM quantile block with identifiable columns.

    Column 0 is the mean, deliberately given an absurd value so that any code
    path that mistakes it for a quantile fails loudly. Columns 1..9 are the
    10th..90th percentiles, encoded as 10..90.
    """
    arr = np.zeros((HORIZON, 10), dtype=np.float64)
    arr[:, 0] = -999.0
    for decile in range(1, 10):
        arr[:, decile] = decile * 10.0
    return arr


@pytest.mark.parametrize(
    "q,expected",
    [
        (0.1, 10.0),
        (0.2, 20.0),
        (0.5, 50.0),
        (0.8, 80.0),
        (0.9, 90.0),
    ],
)
def test_deciles_map_exactly(q, expected):
    got = _extract_quantile(block(), q)
    assert got.shape == (HORIZON,)
    assert np.allclose(got, expected), f"q={q} -> {got[0]}, want {expected}"


def test_non_decile_interpolates():
    # 0.85 sits halfway between p80 (80) and p90 (90).
    assert np.allclose(_extract_quantile(block(), 0.85), 85.0)
    # 0.55 sits halfway between p50 (50) and p60 (60).
    assert np.allclose(_extract_quantile(block(), 0.55), 55.0)


@pytest.mark.parametrize("q", [0.01, 0.05, 0.95, 0.99])
def test_out_of_range_clamps_to_nearest_decile(q):
    """The model carries no p95. Clamping is honest; extrapolating is not."""
    got = _extract_quantile(block(), q)
    assert np.allclose(got, 10.0) or np.allclose(got, 90.0)


def test_mean_column_is_never_returned():
    for q in np.arange(0.05, 1.0, 0.01):
        got = _extract_quantile(block(), float(q))
        assert not np.any(got < 0), f"q={q} leaked the mean column"


def test_quantile_key_formatting_is_stable():
    # The Go client parses these back with strconv.ParseFloat, so they must
    # round-trip and must not gain trailing zeros.
    assert _format_quantile(0.9) == "0.9"
    assert _format_quantile(0.5) == "0.5"
    assert _format_quantile(0.95) == "0.95"


def test_default_revision_is_pinned():
    """An unpinned model means the weights can change under a running cluster
    with no signal. The default must be a concrete revision."""
    from presage_forecaster.config import Config

    cfg = Config()
    assert cfg.model_revision, "the default checkpoint revision must be pinned"
    assert len(cfg.model_revision) == 40, "expected a full git SHA"


def test_revision_can_be_unpinned_deliberately(monkeypatch):
    from presage_forecaster.config import Config

    monkeypatch.setenv("PRESAGE_MODEL_REVISION", "main")
    assert Config.from_env().model_revision is None

    monkeypatch.setenv("PRESAGE_MODEL_REVISION", "abc123")
    assert Config.from_env().model_revision == "abc123"
