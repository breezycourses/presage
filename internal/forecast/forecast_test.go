package forecast

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// dailySine builds `days` days of a smooth daily cycle at 5m resolution.
func dailySine(days int, amplitude, offset float64) []float64 {
	const perDay = 288 // 24h at 5m
	v := make([]float64, days*perDay)
	for i := range v {
		phase := 2 * math.Pi * float64(i%perDay) / perDay
		v[i] = offset + amplitude*math.Sin(phase)
	}
	return v
}

func TestStepsFor(t *testing.T) {
	tests := []struct {
		lead, res time.Duration
		want      int
	}{
		{2 * time.Minute, 5 * time.Minute, 1}, // rounds up
		{5 * time.Minute, 5 * time.Minute, 1},
		{6 * time.Minute, 5 * time.Minute, 2},
		{0, 5 * time.Minute, 1}, // never zero steps
		{time.Hour, 5 * time.Minute, 12},
		{time.Minute, 0, 1}, // degenerate resolution
	}
	for _, tt := range tests {
		if got := StepsFor(tt.lead, tt.res); got != tt.want {
			t.Errorf("StepsFor(%s, %s) = %d, want %d", tt.lead, tt.res, got, tt.want)
		}
	}
}

func TestSeasonalNaive_RecoversADailyCycle(t *testing.T) {
	sn := &SeasonalNaive{Season: 24 * time.Hour, Cycles: 3}
	values := dailySine(5, 50, 100)

	res, err := sn.Forecast(context.Background(), Request{
		Series:    Series{ID: "t", Values: values, Resolution: 5 * time.Minute},
		Horizon:   12,
		Quantiles: []float64{0.5, 0.9},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Point) != 12 {
		t.Fatalf("want 12 points, got %d", len(res.Point))
	}
	// The series is exactly periodic, so a seasonal-naive forecast should be
	// near-exact: step h should match the value one full season earlier.
	const perDay = 288
	for h := 0; h < 12; h++ {
		want := values[len(values)-perDay+h]
		if diff := math.Abs(res.Point[h] - want); diff > 1e-6 {
			t.Fatalf("step %d: got %.4f want %.4f", h, res.Point[h], want)
		}
	}
	if _, ok := res.Quantiles[0.9]; !ok {
		t.Fatal("expected a 0.9 quantile series")
	}
}

func TestSeasonalNaive_QuantilesAreOrdered(t *testing.T) {
	sn := &SeasonalNaive{Season: 24 * time.Hour, Cycles: 2}
	// Add drift so residuals are non-degenerate and the quantiles separate.
	values := dailySine(4, 40, 100)
	for i := range values {
		values[i] += float64(i) * 0.02
	}

	res, err := sn.Forecast(context.Background(), Request{
		Series:    Series{ID: "t", Values: values, Resolution: 5 * time.Minute},
		Horizon:   6,
		Quantiles: []float64{0.1, 0.5, 0.9},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for h := 0; h < 6; h++ {
		lo, mid, hi := res.Quantiles[0.1][h], res.Quantiles[0.5][h], res.Quantiles[0.9][h]
		if !(lo <= mid && mid <= hi) {
			t.Fatalf("step %d: quantiles out of order: %.3f, %.3f, %.3f", h, lo, mid, hi)
		}
	}
}

func TestSeasonalNaive_NeverNegative(t *testing.T) {
	// Demand cannot be negative; a wide lower quantile must clamp, not invent
	// negative load that would then be converted into negative replicas.
	sn := &SeasonalNaive{Season: time.Hour, Cycles: 2}
	values := make([]float64, 60) // 1h of 1m data x ... enough for a 1h season
	for i := range values {
		if i%2 == 0 {
			values[i] = 0
		} else {
			values[i] = 100
		}
	}
	values = append(values, values...)

	res, err := sn.Forecast(context.Background(), Request{
		Series:    Series{ID: "t", Values: values, Resolution: time.Minute},
		Horizon:   4,
		Quantiles: []float64{0.05},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for h, v := range res.Quantiles[0.05] {
		if v < 0 {
			t.Fatalf("step %d produced negative demand %.3f", h, v)
		}
	}
}

func TestSeasonalNaive_InsufficientHistory(t *testing.T) {
	sn := &SeasonalNaive{Season: 168 * time.Hour, Cycles: 3}
	_, err := sn.Forecast(context.Background(), Request{
		Series:  Series{ID: "t", Values: dailySine(1, 10, 50), Resolution: 5 * time.Minute},
		Horizon: 6,
	})
	if err == nil {
		t.Fatal("expected an error for a series shorter than one season")
	}
}

func TestResult_AtClampsPastHorizon(t *testing.T) {
	r := &Result{
		Point:     []float64{1, 2, 3},
		Quantiles: map[float64][]float64{0.9: {10, 20, 30}},
	}
	p, qs, err := r.At(99)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != 3 || qs[0.9] != 30 {
		t.Fatalf("expected clamp to the last point, got %v / %v", p, qs)
	}
}

func TestResult_QuantileAtFallsBackToPoint(t *testing.T) {
	r := &Result{Point: []float64{7, 8, 9}}
	v, err := r.QuantileAt(0.9, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != 8 {
		t.Fatalf("expected the point forecast as fallback, got %v", v)
	}
}

func TestTimesFM_RoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req timesfmRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		// The client must have truncated to MaxContext.
		if got := len(req.Series[0].Values); got != 100 {
			t.Errorf("expected 100 values after truncation, got %d", got)
		}
		_ = json.NewEncoder(w).Encode(timesfmResponse{
			Model:     "google/timesfm-2.5-200m-pytorch",
			Revision:  "1d952420fba87f3c6dee4f240de0f1a0fbc790e3",
			LatencyMS: 42,
			Forecasts: []timesfmForecast{{
				ID:        "t",
				Point:     []float64{1, 2},
				Quantiles: map[string][]float64{"0.9": {3, 4}},
			}},
		})
	}))
	defer srv.Close()

	tf := &TimesFM{Endpoint: srv.URL, MaxContext: 100, MaxHorizon: 64, Client: srv.Client()}
	res, err := tf.Forecast(context.Background(), Request{
		Series:    Series{ID: "t", Values: make([]float64, 500), Resolution: 5 * time.Minute},
		Horizon:   2,
		Quantiles: []float64{0.9},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Model != "google/timesfm-2.5-200m-pytorch" {
		t.Fatalf("unexpected model %q", res.Model)
	}
	if res.Revision != "1d952420fba87f3c6dee4f240de0f1a0fbc790e3" {
		t.Fatalf("model revision did not round-trip: %q", res.Revision)
	}
	if res.Quantiles[0.9][1] != 4 {
		t.Fatalf("quantile did not round-trip: %v", res.Quantiles)
	}
	if res.Latency != 42*time.Millisecond {
		t.Fatalf("unexpected latency %s", res.Latency)
	}
}

func TestTimesFM_RejectsHorizonBeyondCompiledMax(t *testing.T) {
	// Quietly shortening the horizon would make the configured lead time a
	// lie, so this must fail loudly.
	tf := &TimesFM{Endpoint: "http://unused", MaxContext: 100, MaxHorizon: 8, Client: http.DefaultClient}
	_, err := tf.Forecast(context.Background(), Request{
		Series:  Series{ID: "t", Values: make([]float64, 100), Resolution: 5 * time.Minute},
		Horizon: 32,
	})
	if err == nil {
		t.Fatal("expected an error when horizon exceeds maxHorizon")
	}
}

func TestTimesFM_PropagatesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"model not loaded"}`))
	}))
	defer srv.Close()

	tf := &TimesFM{Endpoint: srv.URL, MaxContext: 100, MaxHorizon: 64, Client: srv.Client()}
	_, err := tf.Forecast(context.Background(), Request{
		Series:  Series{ID: "t", Values: make([]float64, 10), Resolution: 5 * time.Minute},
		Horizon: 2,
	})
	if err == nil {
		t.Fatal("expected the server error to propagate")
	}
}
