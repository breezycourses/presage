package policy

import (
	"math"
	"testing"
	"time"
)

var epoch = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

// baseConfig is deliberately permissive so that each test can isolate the one
// rule it is exercising.
func baseConfig() Config {
	return Config{
		Headroom:         Amount{Value: 0},
		MinReplicas:      1,
		MaxReplicas:      1000,
		ScaleDownWindow:  0,
		MaxScaleUpRate:   Amount{Value: 100000},
		MaxScaleDownRate: Amount{Value: 100000},
		// Guard off by default; the tests that exercise it turn it on.
		ScaleDownMaxRelativeSpread: 0,
	}
}

func baseInput() Input {
	return Input{Now: epoch, CurrentReplicas: 10, PerReplica: 10}
}

// TestEvaluate_UncertaintyBuysCapacityNotInaction pins the central design
// decision. An earlier revision put a dead band between the lower and upper
// quantiles, which made a more uncertain forecast produce *less* movement --
// exactly backwards, and it silently suppressed scale-ups, defeating the lead
// time protection that justifies forecasting at all. Capacity now tracks the
// upper quantile alone, so widening uncertainty provisions more.
func TestEvaluate_UncertaintyBuysCapacityNotInaction(t *testing.T) {
	cfg := baseConfig()

	confident := baseInput()
	confident.ForecastDown, confident.ForecastUp = 100, 110

	uncertain := baseInput()
	uncertain.ForecastDown, uncertain.ForecastUp = 100, 180

	a, err := Evaluate(cfg, confident)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := Evaluate(cfg, uncertain)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Replicas != 11 {
		t.Fatalf("confident forecast: want 11, got %d (%s)", a.Replicas, a.Explain)
	}
	if b.Replicas != 18 {
		t.Fatalf("uncertain forecast: want 18, got %d (%s)", b.Replicas, b.Explain)
	}
	if b.Replicas <= a.Replicas {
		t.Fatalf("more uncertainty must provision more, got %d vs %d", b.Replicas, a.Replicas)
	}
}

// TestEvaluate_UncertaintyGuardBlocksScaleDown covers the second quantile's
// only job: releasing capacity is the expensive direction to be wrong in, so
// it is gated on the forecast being confident.
func TestEvaluate_UncertaintyGuardBlocksScaleDown(t *testing.T) {
	cfg := baseConfig()
	cfg.ScaleDownMaxRelativeSpread = 0.25

	in := baseInput()
	// Target says 6 replicas (down from 10), but the p50->p90 spread is
	// 60% -- far too wide to justify giving capacity back.
	in.ForecastDown, in.ForecastUp = 37.5, 60

	got, err := Evaluate(cfg, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Replicas != 10 {
		t.Fatalf("expected hold at 10, got %d (%s)", got.Replicas, got.Explain)
	}
	if got.Constraint != ConstraintForecastUncertain {
		t.Fatalf("expected ForecastUncertainty constraint, got %q", got.Constraint)
	}
	if got.ScaleDownCandidateSince == nil {
		t.Fatal("a blocked scale-down is still a pending scale-down; tracker must be set")
	}

	// Same target, but a confident forecast: the scale-down goes through.
	in.ForecastDown, in.ForecastUp = 55, 60
	got, err = Evaluate(cfg, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Replicas != 6 {
		t.Fatalf("expected scale-down to 6 on a confident forecast, got %d (%s)", got.Replicas, got.Explain)
	}
}

// TestEvaluate_UncertaintyGuardNeverBlocksScaleUp: the guard is asymmetric on
// purpose. An uncertain forecast must never delay adding capacity.
func TestEvaluate_UncertaintyGuardNeverBlocksScaleUp(t *testing.T) {
	cfg := baseConfig()
	cfg.ScaleDownMaxRelativeSpread = 0.01 // effectively always "uncertain"

	in := baseInput()
	in.ForecastDown, in.ForecastUp = 50, 300

	got, err := Evaluate(cfg, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Replicas != 30 {
		t.Fatalf("expected scale-up to 30 regardless of spread, got %d (%s)", got.Replicas, got.Explain)
	}
}

func TestEvaluate_ScalesUpOnUpperQuantile(t *testing.T) {
	cfg := baseConfig()
	in := baseInput()
	// p90 -> 15 replicas, above current 10.
	in.ForecastDown, in.ForecastUp = 90, 150

	got, err := Evaluate(cfg, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Replicas != 15 {
		t.Fatalf("expected 15, got %d (%s)", got.Replicas, got.Explain)
	}
	if got.Predictive != 15 {
		t.Fatalf("expected predictive 15, got %d", got.Predictive)
	}
}

func TestEvaluate_ScaleUpIsNeverDelayed(t *testing.T) {
	// A long scale-down window must not slow a scale-up down.
	cfg := baseConfig()
	cfg.ScaleDownWindow = time.Hour
	in := baseInput()
	in.ForecastDown, in.ForecastUp = 90, 200

	got, err := Evaluate(cfg, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Replicas != 20 {
		t.Fatalf("scale-up was delayed: got %d (%s)", got.Replicas, got.Explain)
	}
}

func TestEvaluate_ScaleDownHeldUntilWindowElapses(t *testing.T) {
	cfg := baseConfig()
	cfg.ScaleDownWindow = 15 * time.Minute
	in := baseInput()
	in.ForecastDown, in.ForecastUp = 55, 60 // target 6, well below current 10

	// First evaluation: becomes a candidate, holds.
	first, err := Evaluate(cfg, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first.Replicas != 10 {
		t.Fatalf("expected hold at 10 on first evaluation, got %d", first.Replicas)
	}
	if first.Constraint != ConstraintScaleDownWindow {
		t.Fatalf("expected ScaleDownWindow constraint, got %q", first.Constraint)
	}
	if first.ScaleDownCandidateSince == nil {
		t.Fatal("expected candidate tracker to be set")
	}

	// Still inside the window.
	in.ScaleDownCandidateSince = first.ScaleDownCandidateSince
	in.Now = epoch.Add(14 * time.Minute)
	mid, _ := Evaluate(cfg, in)
	if mid.Replicas != 10 {
		t.Fatalf("expected still held at 14m, got %d", mid.Replicas)
	}

	// Window elapsed.
	in.Now = epoch.Add(16 * time.Minute)
	after, _ := Evaluate(cfg, in)
	if after.Replicas != 6 {
		t.Fatalf("expected scale-down to 6 after window, got %d (%s)", after.Replicas, after.Explain)
	}
}

func TestEvaluate_ScaleDownCandidateResetsWhenDemandRecovers(t *testing.T) {
	cfg := baseConfig()
	cfg.ScaleDownWindow = 15 * time.Minute
	in := baseInput()
	in.ForecastDown, in.ForecastUp = 55, 60

	first, _ := Evaluate(cfg, in)
	if first.ScaleDownCandidateSince == nil {
		t.Fatal("expected candidate tracker to be set")
	}

	// Demand comes back before the window elapses; the timer must restart, not
	// resume, or a workload that dips repeatedly would eventually scale down
	// on the strength of intermittent dips.
	in.ScaleDownCandidateSince = first.ScaleDownCandidateSince
	in.Now = epoch.Add(10 * time.Minute)
	in.ForecastDown, in.ForecastUp = 100, 140
	recovered, _ := Evaluate(cfg, in)
	if recovered.ScaleDownCandidateSince != nil {
		t.Fatalf("expected candidate tracker to clear, got %v", recovered.ScaleDownCandidateSince)
	}
}

func TestEvaluate_ReactiveFloorPreventsUnderProvisioning(t *testing.T) {
	// The safety property that makes presage a strict improvement over the
	// reactive policy it replaces: a badly wrong low forecast cannot starve
	// the workload.
	cfg := baseConfig()
	cfg.ReactiveFloor = &ReactiveFloor{Buffer: Amount{Value: 2}}
	in := baseInput()
	in.CurrentDemand = 200                  // 20 replicas' worth of demand, right now
	in.ForecastDown, in.ForecastUp = 10, 20 // forecast says demand is about to vanish

	got, err := Evaluate(cfg, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Replicas != 22 {
		t.Fatalf("expected reactive floor 20+2=22, got %d (%s)", got.Replicas, got.Explain)
	}
	if got.Constraint != ConstraintReactiveFloor {
		t.Fatalf("expected ReactiveFloor constraint, got %q", got.Constraint)
	}
	if got.Predictive != 2 {
		t.Fatalf("expected the (bad) predictive value to still be reported, got %d", got.Predictive)
	}
}

func TestEvaluate_ReactiveFloorPercentBuffer(t *testing.T) {
	cfg := baseConfig()
	cfg.ReactiveFloor = &ReactiveFloor{Buffer: Amount{Value: 25, Percent: true}}
	in := baseInput()
	in.CurrentDemand = 100 // 10 replicas + 25% = 13
	in.ForecastDown, in.ForecastUp = 0, 0

	got, _ := Evaluate(cfg, in)
	if got.Replicas != 13 {
		t.Fatalf("expected 13, got %d (%s)", got.Replicas, got.Explain)
	}
}

func TestEvaluate_MaxReplicasOverridesReactiveFloor(t *testing.T) {
	// Documented consequence: MaxReplicas is a hard bound and wins over the
	// safety floor. Operators must size it above real peak demand.
	cfg := baseConfig()
	cfg.MaxReplicas = 15
	cfg.ReactiveFloor = &ReactiveFloor{Buffer: Amount{Value: 2}}
	in := baseInput()
	in.CurrentDemand = 200
	in.ForecastDown, in.ForecastUp = 10, 20

	got, _ := Evaluate(cfg, in)
	if got.Replicas != 15 {
		t.Fatalf("expected clamp to 15, got %d", got.Replicas)
	}
	if got.Constraint != ConstraintMaxReplicas {
		t.Fatalf("expected MaxReplicas constraint, got %q", got.Constraint)
	}
}

func TestEvaluate_RateLimits(t *testing.T) {
	tests := []struct {
		name       string
		up, down   Amount
		current    int32
		fUp, fDown float64
		want       int32
		constraint Constraint
	}{
		{"scale up capped absolutely", Amount{Value: 3}, Amount{Value: 100}, 10, 500, 500, 13, ConstraintMaxScaleUpRate},
		{"scale up capped by percent", Amount{Value: 50, Percent: true}, Amount{Value: 100}, 10, 500, 500, 15, ConstraintMaxScaleUpRate},
		{"scale down capped by percent", Amount{Value: 100}, Amount{Value: 20, Percent: true}, 10, 10, 10, 8, ConstraintMaxScaleDownRate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.MaxScaleUpRate, cfg.MaxScaleDownRate = tt.up, tt.down
			in := baseInput()
			in.CurrentReplicas = tt.current
			in.ForecastUp, in.ForecastDown = tt.fUp, tt.fDown

			got, err := Evaluate(cfg, in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Replicas != tt.want {
				t.Fatalf("want %d, got %d (%s)", tt.want, got.Replicas, got.Explain)
			}
			if got.Constraint != tt.constraint {
				t.Fatalf("want constraint %q, got %q", tt.constraint, got.Constraint)
			}
		})
	}
}

func TestEvaluate_PercentRateDoesNotDeadlockAtZero(t *testing.T) {
	// current=0 with a percentage rate resolves to a zero-replica step. Without
	// the floor-of-one this workload could never leave zero.
	cfg := baseConfig()
	cfg.MinReplicas = 0
	cfg.MaxScaleUpRate = Amount{Value: 100, Percent: true}
	in := baseInput()
	in.CurrentReplicas = 0
	in.ForecastDown, in.ForecastUp = 50, 100

	got, err := Evaluate(cfg, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Replicas != 1 {
		t.Fatalf("expected to escape zero with 1 replica, got %d (%s)", got.Replicas, got.Explain)
	}
}

func TestEvaluate_Headroom(t *testing.T) {
	cfg := baseConfig()
	cfg.Headroom = Amount{Value: 20, Percent: true}
	in := baseInput()
	in.ForecastDown, in.ForecastUp = 100, 100 // 100 * 1.2 / 10 = 12

	got, _ := Evaluate(cfg, in)
	if got.Replicas != 12 {
		t.Fatalf("expected 12 with 20%% headroom, got %d (%s)", got.Replicas, got.Explain)
	}
}

func TestEvaluate_QuantileCrossingIsCorrected(t *testing.T) {
	// A backend that emits crossed quantiles must not invert up/down handling.
	cfg := baseConfig()
	in := baseInput()
	in.ForecastUp, in.ForecastDown = 50, 150 // crossed on purpose

	got, err := Evaluate(cfg, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Replicas != 15 {
		t.Fatalf("expected the larger value to drive scale-up (15), got %d (%s)", got.Replicas, got.Explain)
	}
}

func TestEvaluate_NegativeForecastClampedToZero(t *testing.T) {
	cfg := baseConfig()
	cfg.MinReplicas = 0
	in := baseInput()
	in.CurrentReplicas = 5
	in.ForecastUp, in.ForecastDown = -10, -50

	got, err := Evaluate(cfg, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Replicas != 0 {
		t.Fatalf("expected 0, got %d (%s)", got.Replicas, got.Explain)
	}
}

func TestEvaluate_InvalidInputs(t *testing.T) {
	tests := []struct {
		name string
		cfg  func(Config) Config
		in   func(Input) Input
		want error
	}{
		{"zero capacity", nil, func(i Input) Input { i.PerReplica = 0; return i }, ErrInvalidCapacity},
		{"NaN capacity", nil, func(i Input) Input { i.PerReplica = math.NaN(); return i }, ErrInvalidCapacity},
		{"NaN forecast", nil, func(i Input) Input { i.ForecastUp = math.NaN(); return i }, ErrInvalidForecast},
		{"Inf forecast", nil, func(i Input) Input { i.ForecastDown = math.Inf(1); return i }, ErrInvalidForecast},
		{"max below min", func(c Config) Config { c.MinReplicas, c.MaxReplicas = 10, 5; return c }, nil, ErrInvalidBounds},
		{"zero max", func(c Config) Config { c.MaxReplicas = 0; return c }, nil, ErrInvalidBounds},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, in := baseConfig(), baseInput()
			if tt.cfg != nil {
				cfg = tt.cfg(cfg)
			}
			if tt.in != nil {
				in = tt.in(in)
			}
			if _, err := Evaluate(cfg, in); err != tt.want {
				t.Fatalf("want %v, got %v", tt.want, err)
			}
		})
	}
}

// TestEvaluate_ReactiveFloorIsNeverUnderReactive is the headline safety
// property, checked across a wide sweep of forecasts including badly wrong
// ones: with the floor enabled and MaxReplicas out of the way, the
// recommendation is never below what a plain reactive buffer policy would
// have chosen.
//
// The rate limits are left permissive here on purpose. MaxScaleUpRate and
// MaxReplicas are hard constraints that can legitimately bind below the floor
// -- the floor removes *forecast error* as a cause of under-provisioning, not
// operator-configured limits.
func TestEvaluate_ReactiveFloorIsNeverUnderReactive(t *testing.T) {
	cfg := baseConfig()
	cfg.ReactiveFloor = &ReactiveFloor{Buffer: Amount{Value: 2}}
	cfg.ScaleDownWindow = 10 * time.Minute

	for _, demand := range []float64{0, 1, 37, 100, 250, 999} {
		for _, fUp := range []float64{0, 5, 50, 500, 5000} {
			for _, fDown := range []float64{0, 5, 50, 500, 5000} {
				for _, current := range []int32{0, 1, 10, 100} {
					in := baseInput()
					in.CurrentReplicas = current
					in.CurrentDemand = demand
					in.ForecastUp, in.ForecastDown = fUp, fDown

					got, err := Evaluate(cfg, in)
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
					wantFloor := int32(math.Ceil(demand/in.PerReplica)) + 2
					if got.Replicas < wantFloor {
						t.Fatalf("floor violated: demand=%v up=%v down=%v current=%d -> %d, want >= %d (%s)",
							demand, fUp, fDown, current, got.Replicas, wantFloor, got.Explain)
					}
				}
			}
		}
	}
}
