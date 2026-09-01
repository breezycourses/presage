package backtest

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/GrowlyX/presage/internal/forecast"
	"github.com/GrowlyX/presage/internal/policy"
)

// Static holds a fixed replica count: the "what we do today" baseline for
// anyone whose Deployment has a hard-coded replicas field.
type Static struct{ Replicas int32 }

func (s Static) Name() string { return fmt.Sprintf("static(%d)", s.Replicas) }

func (s Static) Decide(context.Context, []float64, int, int32) (int32, error) {
	return s.Replicas, nil
}

// Reactive is a conventional buffer autoscaler: size for present demand plus a
// buffer. This is the honest comparison point, because it is what presage
// replaces and what its own reactive floor computes internally.
type Reactive struct {
	PerReplica float64
	Buffer     policy.Amount
	Min, Max   int32
}

func (r Reactive) Name() string { return fmt.Sprintf("reactive(buffer=%s)", r.Buffer) }

func (r Reactive) Decide(_ context.Context, history []float64, now int, _ int32) (int32, error) {
	demand := history[now]
	base := int32(math.Ceil(demand / r.PerReplica))
	target := base + int32(math.Ceil(r.Buffer.Of(float64(base))))
	return clamp(target, r.Min, r.Max), nil
}

// Oracle provisions for demand exactly one lead time ahead, with a buffer. It
// cheats -- it reads the future -- and that is the point: it is the ceiling a
// perfect forecaster would reach, so it says how much of the gap between
// reactive and perfect a real forecast actually closed. Without it, "presage
// beat reactive by 12%" has no scale.
type Oracle struct {
	PerReplica float64
	LeadSteps  int
	Buffer     policy.Amount
	Min, Max   int32
	// Full is the complete series, including the future. Only Oracle gets it.
	Full []float64
}

func (o Oracle) Name() string { return "oracle (perfect foresight)" }

func (o Oracle) Decide(_ context.Context, _ []float64, now int, _ int32) (int32, error) {
	at := now + o.LeadSteps
	if at >= len(o.Full) {
		at = len(o.Full) - 1
	}
	base := int32(math.Ceil(o.Full[at] / o.PerReplica))
	target := base + int32(math.Ceil(o.Buffer.Of(float64(base))))
	return clamp(target, o.Min, o.Max), nil
}

// Predictive runs the real forecasting path and the real policy engine, so a
// backtest result reflects the code that would actually run rather than a
// reimplementation of it that could drift.
type Predictive struct {
	Label      string
	Backend    forecast.Backend
	Policy     policy.Config
	PerReplica float64
	Resolution time.Duration
	LeadSteps  int
	// Context caps how much history is handed to the backend, mirroring the
	// model's compiled context length.
	Context int

	TargetQuantile float64
	LowerQuantile  float64

	// scaleDownCandidateSince is carried between decisions the way the
	// controller carries it in status, so the stabilization window behaves in
	// the backtest as it does in the cluster.
	scaleDownCandidateSince *time.Time
	clock                   time.Time
}

func (p *Predictive) Name() string {
	if p.Label != "" {
		return p.Label
	}
	return fmt.Sprintf("predictive(%s)", p.Backend.Name())
}

func (p *Predictive) Decide(ctx context.Context, history []float64, now int, current int32) (int32, error) {
	// A synthetic clock advancing one resolution per step, so stabilization
	// windows measure simulated time rather than wall time.
	p.clock = p.clock.Add(p.Resolution)

	series := history
	if p.Context > 0 && len(series) > p.Context {
		series = series[len(series)-p.Context:]
	}

	horizon := p.LeadSteps
	if horizon < 1 {
		horizon = 1
	}

	result, err := p.Backend.Forecast(ctx, forecast.Request{
		Series:    forecast.Series{ID: "backtest", Values: series, Resolution: p.Resolution},
		Horizon:   horizon,
		Quantiles: []float64{p.LowerQuantile, p.TargetQuantile},
	})
	if err != nil {
		return 0, err
	}

	up, err := result.QuantileAt(p.TargetQuantile, p.LeadSteps)
	if err != nil {
		return 0, err
	}
	down, err := result.QuantileAt(p.LowerQuantile, p.LeadSteps)
	if err != nil {
		return 0, err
	}

	decision, err := policy.Evaluate(p.Policy, policy.Input{
		Now:                     p.clock,
		CurrentReplicas:         current,
		PerReplica:              p.PerReplica,
		ForecastUp:              up,
		ForecastDown:            down,
		CurrentDemand:           history[now],
		ScaleDownCandidateSince: p.scaleDownCandidateSince,
	})
	if err != nil {
		return 0, err
	}
	p.scaleDownCandidateSince = decision.ScaleDownCandidateSince
	return decision.Replicas, nil
}

func clamp(v, lo, hi int32) int32 {
	if v < lo {
		return lo
	}
	if hi > 0 && v > hi {
		return hi
	}
	return v
}
