// Package forecast defines the forecasting contract and the backends that
// satisfy it.
//
// Backends are intentionally interchangeable. A foundation model is a means,
// not the point: presage should be useful with no model server at all, and
// there should always be a cheap baseline to measure an expensive model
// against. A forecasting project without a baseline cannot tell improvement
// from noise.
package forecast

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// Series is an evenly spaced univariate time series.
type Series struct {
	// ID identifies the series for batching and logging.
	ID string
	// Values are ordered oldest to newest, evenly spaced by Resolution.
	Values []float64
	// Resolution is the spacing between consecutive values.
	Resolution time.Duration
	// End is the timestamp of the final value.
	End time.Time
}

// Request asks for a forecast.
type Request struct {
	Series Series
	// Horizon is the number of steps ahead to forecast.
	Horizon int
	// Quantiles requested, each in (0, 1). Backends that cannot produce
	// quantiles return only Point and leave Quantiles empty.
	Quantiles []float64
}

// Result is a forecast.
type Result struct {
	// Point is the point forecast, Horizon values long.
	Point []float64
	// Quantiles maps each requested quantile to a Horizon-length series.
	// Empty when the backend has no quantile support.
	Quantiles map[float64][]float64
	// Backend that produced this, e.g. "TimesFM".
	Backend string
	// Model identifier, when the backend has one.
	Model string
	// Latency the backend reported, if any.
	Latency time.Duration
}

// Backend produces forecasts.
type Backend interface {
	// Name is the backend type, matching the ForecastBackend CRD.
	Name() string
	// Forecast returns a forecast, or an error. Implementations must respect
	// ctx cancellation: presage would rather have no forecast than a late one.
	Forecast(ctx context.Context, req Request) (*Result, error)
}

var (
	// ErrInsufficientHistory means the series is too short for the backend.
	ErrInsufficientHistory = errors.New("forecast: insufficient history")
	// ErrNoQuantiles means quantiles were required but the backend has none.
	ErrNoQuantiles = errors.New("forecast: backend produced no quantiles")
)

// StepsFor converts a duration into a whole number of series steps, rounding
// up, with a floor of one. A lead time shorter than one step must still look
// one step ahead, or presage silently degenerates into a reactive autoscaler.
func StepsFor(d, resolution time.Duration) int {
	if resolution <= 0 {
		return 1
	}
	steps := int(math.Ceil(float64(d) / float64(resolution)))
	if steps < 1 {
		return 1
	}
	return steps
}

// At samples a forecast at a given step (1-based: step 1 is one resolution
// into the future). Values past the end of the horizon clamp to the last
// available point rather than erroring, so a slightly short horizon degrades
// instead of failing.
func (r *Result) At(step int) (point float64, quantiles map[float64]float64, err error) {
	if r == nil || len(r.Point) == 0 {
		return 0, nil, fmt.Errorf("forecast: empty result")
	}
	i := step - 1
	if i < 0 {
		i = 0
	}
	if i >= len(r.Point) {
		i = len(r.Point) - 1
	}
	quantiles = make(map[float64]float64, len(r.Quantiles))
	for q, series := range r.Quantiles {
		j := i
		if j >= len(series) {
			j = len(series) - 1
		}
		if j >= 0 {
			quantiles[q] = series[j]
		}
	}
	return r.Point[i], quantiles, nil
}

// QuantileAt returns the forecast for one quantile at one step, falling back
// to the point forecast when the backend produced no quantiles. The fallback
// is what makes a point-only backend usable, at the cost of the asymmetric
// policy collapsing to a single number.
func (r *Result) QuantileAt(q float64, step int) (float64, error) {
	point, qs, err := r.At(step)
	if err != nil {
		return 0, err
	}
	if v, ok := qs[q]; ok {
		return v, nil
	}
	return point, nil
}

// empiricalQuantile returns the q-th quantile of xs using linear interpolation
// between order statistics. xs is sorted in place.
func empiricalQuantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	if q <= 0 {
		return xs[0]
	}
	if q >= 1 {
		return xs[len(xs)-1]
	}
	pos := q * float64(len(xs)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return xs[lo]
	}
	frac := pos - float64(lo)
	return xs[lo]*(1-frac) + xs[hi]*frac
}
