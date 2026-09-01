package forecast

import (
	"context"
	"fmt"
	"math"
	"time"
)

// SeasonalNaive forecasts by repeating the same point from previous seasons,
// with prediction intervals taken from the empirical distribution of one
// season's naive error.
//
// It is here as the control, not as a placeholder. On a workload with clean
// weekly seasonality -- which describes most player-facing traffic -- this is
// a genuinely hard baseline, and any foundation model that cannot beat it on
// a given workload is not earning its inference cost there. Keeping it in the
// same interface makes that comparison a config change rather than a project.
type SeasonalNaive struct {
	// Season is the periodicity to repeat, typically 24h or 168h.
	Season time.Duration
	// Cycles is how many past seasons to average the point forecast over.
	Cycles int
}

// Name implements Backend.
func (s *SeasonalNaive) Name() string { return "SeasonalNaive" }

// Forecast implements Backend.
func (s *SeasonalNaive) Forecast(_ context.Context, req Request) (*Result, error) {
	start := time.Now()

	res := req.Series.Resolution
	if res <= 0 {
		return nil, fmt.Errorf("forecast: series resolution must be > 0")
	}
	season := s.Season
	if season <= 0 {
		season = 168 * time.Hour
	}
	cycles := s.Cycles
	if cycles < 1 {
		cycles = 1
	}

	period := int(math.Round(float64(season) / float64(res)))
	if period < 1 {
		return nil, fmt.Errorf("forecast: season %s is shorter than resolution %s", season, res)
	}

	v := req.Series.Values
	n := len(v)
	// One full season plus one point is the minimum that lets us compute a
	// single naive residual.
	if n < period+1 {
		return nil, fmt.Errorf("%w: have %d points, need > %d for a %s season at %s resolution",
			ErrInsufficientHistory, n, period, season, res)
	}

	// Use as many whole cycles as the history actually contains.
	usable := n / period
	if usable < 1 {
		usable = 1
	}
	if cycles > usable {
		cycles = usable
	}

	point := make([]float64, req.Horizon)
	for h := 0; h < req.Horizon; h++ {
		// Wrap horizons longer than one season back around the season.
		offset := h % period
		var sum float64
		var count int
		for c := 1; c <= cycles; c++ {
			idx := n - c*period + offset
			if idx >= 0 && idx < n {
				sum += v[idx]
				count++
			}
		}
		if count == 0 {
			point[h] = v[n-1]
			continue
		}
		point[h] = sum / float64(count)
	}

	// Residuals of the one-season-ago naive predictor over the observed
	// history. These stand in for the predictive distribution.
	residuals := make([]float64, 0, n-period)
	for i := period; i < n; i++ {
		residuals = append(residuals, v[i]-v[i-period])
	}

	quantiles := make(map[float64][]float64, len(req.Quantiles))
	for _, q := range req.Quantiles {
		// Copy: empiricalQuantile sorts in place.
		buf := make([]float64, len(residuals))
		copy(buf, residuals)
		offset := empiricalQuantile(buf, q)

		series := make([]float64, req.Horizon)
		for h := range series {
			series[h] = math.Max(point[h]+offset, 0)
		}
		quantiles[q] = series
	}

	for h := range point {
		point[h] = math.Max(point[h], 0)
	}

	return &Result{
		Point:     point,
		Quantiles: quantiles,
		Backend:   s.Name(),
		Model:     fmt.Sprintf("seasonal-naive(season=%s,cycles=%d)", season, cycles),
		Latency:   time.Since(start),
	}, nil
}
