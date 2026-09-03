package policy

import (
	"errors"
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
	return Input{
		Now:             epoch,
		CurrentReplicas: 10,
		Signals:         []Signal{{Name: "demand", PerReplica: 10}},
	}
}

// down/up set the lower and upper forecast quantiles of the first signal.
func setForecast(in *Input, down, up float64) {
	in.Signals[0].ForecastDown = down
	in.Signals[0].ForecastUp = up
}

func setDemand(in *Input, v float64)     { in.Signals[0].CurrentDemand = v }
func setPerReplica(in *Input, v float64) { in.Signals[0].PerReplica = v }

// TestEvaluate_UncertaintyBuysCapacityNotInaction pins the central design
// decision. An earlier revision put a dead band between the lower and upper
// quantiles, which made a more uncertain forecast produce *less* movement --
// exactly backwards, and it silently suppressed scale-ups, defeating the lead
// time protection that justifies forecasting at all. Capacity now tracks the
// upper quantile alone, so widening uncertainty provisions more.
func TestEvaluate_UncertaintyBuysCapacityNotInaction(t *testing.T) {
	cfg := baseConfig()

	confident := baseInput()
	setForecast(&confident, 100, 110)

	uncertain := baseInput()
	setForecast(&uncertain, 100, 180)

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
	setForecast(&in, 37.5, 60)

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
	setForecast(&in, 55, 60)
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
	setForecast(&in, 50, 300)

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
	setForecast(&in, 90, 150)

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
	setForecast(&in, 90, 200)

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
	setForecast(&in, 55, 60) // target 6, well below current 10

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
	setForecast(&in, 55, 60)

	first, _ := Evaluate(cfg, in)
	if first.ScaleDownCandidateSince == nil {
		t.Fatal("expected candidate tracker to be set")
	}

	// Demand comes back before the window elapses; the timer must restart, not
	// resume, or a workload that dips repeatedly would eventually scale down
	// on the strength of intermittent dips.
	in.ScaleDownCandidateSince = first.ScaleDownCandidateSince
	in.Now = epoch.Add(10 * time.Minute)
	setForecast(&in, 100, 140)
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
	setDemand(&in, 200)      // 20 replicas' worth of demand, right now
	setForecast(&in, 10, 20) // forecast says demand is about to vanish

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
	setDemand(&in, 100) // 10 replicas + 25% = 13
	setForecast(&in, 0, 0)

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
	setDemand(&in, 200)
	setForecast(&in, 10, 20)

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
			setForecast(&in, tt.fDown, tt.fUp)

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
	setForecast(&in, 50, 100)

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
	setForecast(&in, 100, 100) // 100 * 1.2 / 10 = 12

	got, _ := Evaluate(cfg, in)
	if got.Replicas != 12 {
		t.Fatalf("expected 12 with 20%% headroom, got %d (%s)", got.Replicas, got.Explain)
	}
}

func TestEvaluate_QuantileCrossingIsCorrected(t *testing.T) {
	// A backend that emits crossed quantiles must not invert up/down handling.
	cfg := baseConfig()
	in := baseInput()
	// Deliberately crossed: up=50 is below down=150.
	setForecast(&in, 150, 50)

	got, err := Evaluate(cfg, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Replicas != 15 {
		t.Fatalf("expected the larger value to drive scale-up (15), got %d (%s)", got.Replicas, got.Explain)
	}
	// Crossing must be visible in the decision so callers can surface it.
	if len(got.CrossingSignals) != 1 || got.CrossingSignals[0] != "demand" {
		t.Fatalf("expected crossing signal [demand], got %v", got.CrossingSignals)
	}
}

func TestEvaluate_NoCrossingWhenQuantilesAreOrdered(t *testing.T) {
	cfg := baseConfig()
	in := baseInput()
	setForecast(&in, 50, 100)

	got, err := Evaluate(cfg, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.CrossingSignals) != 0 {
		t.Fatalf("expected no crossing signals, got %v", got.CrossingSignals)
	}
}

func TestEvaluate_NegativeForecastClampedToZero(t *testing.T) {
	cfg := baseConfig()
	cfg.MinReplicas = 0
	in := baseInput()
	in.CurrentReplicas = 5
	setForecast(&in, -50, -10)

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
		{"zero capacity", nil, func(i Input) Input { i.Signals[0].PerReplica = 0; return i }, ErrInvalidCapacity},
		{"NaN capacity", nil, func(i Input) Input { i.Signals[0].PerReplica = math.NaN(); return i }, ErrInvalidCapacity},
		{"NaN forecast", nil, func(i Input) Input { i.Signals[0].ForecastUp = math.NaN(); return i }, ErrInvalidForecast},
		{"Inf forecast", nil, func(i Input) Input { i.Signals[0].ForecastDown = math.Inf(1); return i }, ErrInvalidForecast},
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
			if _, err := Evaluate(cfg, in); !errors.Is(err, tt.want) {
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
					setDemand(&in, demand)
					setForecast(&in, fDown, fUp)

					got, err := Evaluate(cfg, in)
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
					wantFloor := int32(math.Ceil(demand/in.Signals[0].PerReplica)) + 2
					if got.Replicas < wantFloor {
						t.Fatalf("floor violated: demand=%v up=%v down=%v current=%d -> %d, want >= %d (%s)",
							demand, fUp, fDown, current, got.Replicas, wantFloor, got.Explain)
					}
				}
			}
		}
	}
}

// TestEvaluate_LargestSignalBinds: a workload must be big enough for every
// dimension, so the binding one is whichever needs the most. Averaging or
// summing instead would let a quiet dimension mask a busy one.
func TestEvaluate_LargestSignalBinds(t *testing.T) {
	cfg := baseConfig()
	in := baseInput()
	in.Signals = []Signal{
		{Name: "players", PerReplica: 10, ForecastUp: 60, ForecastDown: 60}, // 6 replicas
		{Name: "cpu", PerReplica: 0.5, ForecastUp: 7, ForecastDown: 7},      // 14 replicas
		{Name: "rooms", PerReplica: 4, ForecastUp: 20, ForecastDown: 20},    // 5 replicas
	}

	got, err := Evaluate(cfg, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Replicas != 14 {
		t.Fatalf("want 14 (the largest requirement), got %d (%s)", got.Replicas, got.Explain)
	}
	// "Which dimension is driving the size of this workload" is the first
	// question anyone asks, so the answer is reported rather than inferred.
	if got.BindingSignal != "cpu" {
		t.Fatalf("want binding signal %q, got %q", "cpu", got.BindingSignal)
	}
}

// TestEvaluate_ReactiveFloorTakesTheMaxAcrossSignals: the floor has to cover
// every dimension too, or a busy signal could be starved by a quiet one.
func TestEvaluate_ReactiveFloorTakesTheMaxAcrossSignals(t *testing.T) {
	cfg := baseConfig()
	cfg.ReactiveFloor = &ReactiveFloor{Buffer: Amount{Value: 1}}
	in := baseInput()
	in.Signals = []Signal{
		{Name: "quiet", PerReplica: 10, CurrentDemand: 10, ForecastUp: 0, ForecastDown: 0},
		{Name: "busy", PerReplica: 10, CurrentDemand: 250, ForecastUp: 0, ForecastDown: 0},
	}

	got, err := Evaluate(cfg, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Replicas != 26 { // ceil(250/10) + 1
		t.Fatalf("want 26 from the busy signal's floor, got %d (%s)", got.Replicas, got.Explain)
	}
	if got.Constraint != ConstraintReactiveFloor {
		t.Fatalf("want ReactiveFloor, got %q", got.Constraint)
	}
}

// TestEvaluate_UncertaintyGuardUsesTheBindingSignal: the confidence check must
// read the forecast that actually set the target, not an unrelated one.
func TestEvaluate_UncertaintyGuardUsesTheBindingSignal(t *testing.T) {
	cfg := baseConfig()
	cfg.ScaleDownMaxRelativeSpread = 0.25
	in := baseInput()
	in.Signals = []Signal{
		// Binding (6 replicas), and very uncertain.
		{Name: "binding", PerReplica: 10, ForecastUp: 60, ForecastDown: 20},
		// Smaller and confident; must not be what the guard measures.
		{Name: "other", PerReplica: 10, ForecastUp: 20, ForecastDown: 19.5},
	}

	got, err := Evaluate(cfg, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Constraint != ConstraintForecastUncertain {
		t.Fatalf("the guard should have read the binding signal's spread, got %q (%s)",
			got.Constraint, got.Explain)
	}
	if got.Replicas != 10 {
		t.Fatalf("want a held scale-down at 10, got %d", got.Replicas)
	}
}

func TestEvaluate_NoSignals(t *testing.T) {
	in := baseInput()
	in.Signals = nil
	if _, err := Evaluate(baseConfig(), in); !errors.Is(err, ErrNoSignals) {
		t.Fatalf("want ErrNoSignals, got %v", err)
	}
}

func TestEvaluate_BadSignalNamesItself(t *testing.T) {
	in := baseInput()
	in.Signals = []Signal{
		{Name: "good", PerReplica: 10, ForecastUp: 50, ForecastDown: 50},
		{Name: "broken", PerReplica: 0, ForecastUp: 50, ForecastDown: 50},
	}
	_, err := Evaluate(baseConfig(), in)
	if err == nil || !errors.Is(err, ErrInvalidCapacity) {
		t.Fatalf("want ErrInvalidCapacity, got %v", err)
	}
	if !contains(err.Error(), "broken") {
		t.Fatalf("the error should name the offending signal, got %q", err)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
