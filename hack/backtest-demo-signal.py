#!/usr/bin/env python3
"""A Prometheus-shaped endpoint serving a synthetic player-count signal.

The charts in the README have to come from somewhere reproducible. Pointing
them at a production metrics store would make them unverifiable and would leak
traffic shape; generating them from a fixed seed means anyone can regenerate
the identical figure and check the claim.

The signal is deliberately realistic for a game network rather than easy:
a strong daily cycle, a weekday/weekend difference, a slow growth trend,
occasional evening spikes, and multiplicative noise. Seasonal-naive is a hard
baseline on exactly this shape, which is the point -- an easy signal would
flatter the forecaster.

    python3 hack/backtest-demo-signal.py [port]
"""

from __future__ import annotations

import json
import math
import random
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs

SEED = 20260901
BASE = 620.0


def value(i: int, step: float) -> float:
    """Signal at bucket i, where each bucket is `step` seconds."""
    per_day = 86400 / step
    per_week = per_day * 7
    day_phase = 2 * math.pi * (i % per_day) / per_day
    week_pos = (i % per_week) / per_week

    # Evening peak, small hours trough.
    daily = 340 * math.sin(day_phase - 1.35)
    # Weekends busier and flatter.
    weekend = 150 if week_pos > 5 / 7 else 0
    # Slow organic growth.
    trend = 0.018 * i

    rng = random.Random(SEED + i)
    noise = rng.gauss(0, 34)
    # Occasional evening spike: a streamer, an event, a reddit post.
    spike = 260 if rng.random() < 0.004 and 0.55 < (i % per_day) / per_day < 0.95 else 0

    return max(BASE + daily + weekend + trend + noise + spike, 20.0)


class Handler(BaseHTTPRequestHandler):
    def _json(self, payload):
        body = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        form = parse_qs(self.rfile.read(n).decode())
        start = float(form["start"][0])
        step = float(form.get("step", ["300"])[0])

        if self.path.startswith("/api/v1/query_range"):
            end = float(form["end"][0])
            values, t, i = [], start, 0
            while t <= end:
                values.append([t, f"{value(i, step):.3f}"])
                t += step
                i += 1
            self._json({"status": "success", "data": {"resultType": "matrix",
                        "result": [{"metric": {"fleet": "lobby"}, "values": values}]}})
        else:
            self._json({"status": "success", "data": {"resultType": "vector",
                        "result": [{"metric": {}, "value": [start, f"{value(0, step):.3f}"]}]}})

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 9099
    HTTPServer(("127.0.0.1", port), Handler).serve_forever()
