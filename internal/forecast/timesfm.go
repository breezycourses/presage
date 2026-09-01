package forecast

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// TimesFM calls a presage-forecaster server running Google Research's TimesFM.
//
// The model is not embedded in the controller for two reasons: the controller
// stays a small Go binary with no Python or accelerator dependency, and the
// model server can be scaled, restarted, or swapped without touching the
// component that writes to the Kubernetes API.
type TimesFM struct {
	// Endpoint is the base URL of the forecaster, without a trailing slash.
	Endpoint string
	// MaxContext is the number of input points the server was compiled for.
	// Longer series are truncated to the most recent MaxContext points.
	MaxContext int
	// MaxHorizon is the number of output points the server was compiled for.
	MaxHorizon int
	// Client is the HTTP client to use. Required.
	Client *http.Client
}

// Name implements Backend.
func (t *TimesFM) Name() string { return "TimesFM" }

type timesfmSeries struct {
	ID                string    `json:"id"`
	Values            []float64 `json:"values"`
	ResolutionSeconds float64   `json:"resolution_seconds"`
}

type timesfmRequest struct {
	Series    []timesfmSeries `json:"series"`
	Horizon   int             `json:"horizon"`
	Quantiles []float64       `json:"quantiles,omitempty"`
}

type timesfmForecast struct {
	ID        string               `json:"id"`
	Point     []float64            `json:"point"`
	Quantiles map[string][]float64 `json:"quantiles"`
}

type timesfmResponse struct {
	Model     string            `json:"model"`
	LatencyMS float64           `json:"latency_ms"`
	Forecasts []timesfmForecast `json:"forecasts"`
	Error     string            `json:"error"`
}

// Forecast implements Backend.
func (t *TimesFM) Forecast(ctx context.Context, req Request) (*Result, error) {
	if t.Client == nil {
		return nil, fmt.Errorf("forecast: TimesFM backend has no HTTP client")
	}
	if t.Endpoint == "" {
		return nil, fmt.Errorf("forecast: TimesFM backend has no endpoint")
	}

	values := req.Series.Values
	if t.MaxContext > 0 && len(values) > t.MaxContext {
		values = values[len(values)-t.MaxContext:]
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("%w: series is empty", ErrInsufficientHistory)
	}

	horizon := req.Horizon
	if t.MaxHorizon > 0 && horizon > t.MaxHorizon {
		// Silently forecasting less far than asked would make the lead time a
		// lie, so this is an error rather than a clamp.
		return nil, fmt.Errorf("forecast: horizon %d exceeds backend maxHorizon %d; "+
			"either lower the lead time, coarsen the signal resolution, or recompile the model server",
			horizon, t.MaxHorizon)
	}

	body, err := json.Marshal(timesfmRequest{
		Series: []timesfmSeries{{
			ID:                req.Series.ID,
			Values:            values,
			ResolutionSeconds: req.Series.Resolution.Seconds(),
		}},
		Horizon:   horizon,
		Quantiles: req.Quantiles,
	})
	if err != nil {
		return nil, fmt.Errorf("forecast: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.Endpoint+"/v1/forecast", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("forecast: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := t.Client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("forecast: call forecaster: %w", err)
	}
	defer resp.Body.Close()

	// Cap the read: a misconfigured endpoint should not be able to exhaust the
	// controller's memory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("forecast: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("forecast: forecaster returned %s: %s",
			resp.Status, truncate(string(raw), 512))
	}

	var out timesfmResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("forecast: decode response: %w", err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("forecast: forecaster error: %s", out.Error)
	}
	if len(out.Forecasts) != 1 {
		return nil, fmt.Errorf("forecast: expected 1 forecast, got %d", len(out.Forecasts))
	}

	f := out.Forecasts[0]
	if len(f.Point) == 0 {
		return nil, fmt.Errorf("forecast: forecaster returned an empty point forecast")
	}

	quantiles := make(map[float64][]float64, len(f.Quantiles))
	for k, series := range f.Quantiles {
		q, err := strconv.ParseFloat(k, 64)
		if err != nil {
			return nil, fmt.Errorf("forecast: unparseable quantile key %q: %w", k, err)
		}
		quantiles[q] = series
	}

	return &Result{
		Point:     f.Point,
		Quantiles: quantiles,
		Backend:   t.Name(),
		Model:     out.Model,
		Latency:   time.Duration(out.LatencyMS * float64(time.Millisecond)),
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
