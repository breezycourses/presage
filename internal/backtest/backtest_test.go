package backtest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/GrowlyX/presage/internal/policy"
)

const res = time.Minute

func opts(series []float64, mut ...func(*Options)) Options {
	o := Options{
		Series:          series,
		Resolution:      res,
		PerReplica:      10,
		LeadTime:        5 * res,
		Warmup:          0,
		EvalEvery:       1,
		InitialReplicas: 1,
	}
	for _, m := range mut {
		m(&o)
	}
	return o
}

// recordingStrategy asks for a fixed count and remembers what it was shown.
type recordingStrategy struct {
	target   int32
	maxIndex int
	maxLen   int
	calls    int
}

func (r *recordingStrategy) Name() string { return "recording" }

func (r *recordingStrategy) Decide(_ context.Context, history []float64, now int, _ int32) (int32, error) {
	r.calls++
	if now > r.maxIndex {
		r.maxIndex = now
	}
	if len(history) > r.maxLen {
		r.maxLen = len(history)
	}
	return r.target, nil
}

// TestStrategiesCannotSeeTheFuture is the property that makes a backtest
// meaningful at all. A strategy is handed a prefix, never the whole series, so
// lookahead is impossible rather than merely discouraged.
func TestStrategiesCannotSeeTheFuture(t *testing.T) {
	series := make([]float64, 50)
	rec := &recordingStrategy{target: 3}

	if _, err := Run(context.Background(), opts(series), rec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.maxLen > len(series) {
		t.Fatalf("strategy saw %d points from a %d-point series", rec.maxLen, len(series))
	}
	// At step `now` the prefix must be exactly now+1 long: the present is
	// included, the future is not.
	if rec.maxLen != rec.maxIndex+1 {
		t.Fatalf("prefix length %d does not match index %d; a strategy could see ahead",
			rec.maxLen, rec.maxIndex)
	}
}

// TestScaleUpTakesLeadTime is the reason the harness exists. Without modelling
// lead time a reactive strategy looks flawless, and the backtest would report
// that forecasting is pointless.
func TestScaleUpTakesLeadTime(t *testing.T) {
	// Flat at 10 (1 replica's worth), then a step to 100 (10 replicas' worth).
	series := make([]float64, 40)
	for i := range series {
		if i < 20 {
			series[i] = 10
		} else {
			series[i] = 100
		}
	}

	reactive := Reactive{PerReplica: 10, Buffer: policy.Amount{Value: 0}, Min: 1, Max: 100}
	score, err := Run(context.Background(), opts(series), reactive)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The step lands at index 20; the decision made there is effective at 25.
	// So exactly 5 steps are under-provisioned.
	if score.UnmetSteps != 5 {
		t.Fatalf("expected 5 under-provisioned steps from a 5-step lead time, got %d", score.UnmetSteps)
	}
}

// TestScaleDownIsImmediate: releasing a replica is fast. Modelling it as slow
// would flatter every strategy's cost number equally but unrealistically.
func TestScaleDownIsImmediate(t *testing.T) {
	series := make([]float64, 30)
	for i := range series {
		if i < 10 {
			series[i] = 100
		} else {
			series[i] = 10
		}
	}

	reactive := Reactive{PerReplica: 10, Buffer: policy.Amount{Value: 0}, Min: 1, Max: 100}
	o := opts(series, func(o *Options) { o.InitialReplicas = 10 })
	score, err := Run(context.Background(), reactiveOpts(o), reactive)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// It should be back at 1 replica almost immediately after demand drops,
	// so the average sits well below the starting 10.
	if score.AvgReplicas() > 5 {
		t.Fatalf("scale-down looks delayed: average %.2f replicas", score.AvgReplicas())
	}
}

func reactiveOpts(o Options) Options { return o }

// TestOracleBeatsReactiveOnARamp: perfect foresight should be strictly better
// on a rising signal, because that is exactly the situation lead time hurts.
// If this ever fails, the simulation is not modelling lead time correctly and
// every other result from the harness is suspect.
func TestOracleBeatsReactiveOnARamp(t *testing.T) {
	series := make([]float64, 120)
	for i := range series {
		series[i] = 10 + float64(i)*2 // steady ramp
	}
	// Started with enough capacity to cover the first lead-time window. Even
	// perfect foresight cannot beat lead time from a cold start: at step 0 its
	// first cohort cannot land before step 10, so a scaler that begins
	// under-provisioned is short no matter how good its forecast is. That is a
	// real property, not a simulation artefact -- it is why `minReplicas`
	// exists.
	o := opts(series, func(o *Options) { o.LeadTime = 10 * res; o.InitialReplicas = 3 })

	reactive := Reactive{PerReplica: 10, Buffer: policy.Amount{Value: 0}, Min: 1, Max: 200}
	oracle := Oracle{PerReplica: 10, LeadSteps: 10, Buffer: policy.Amount{Value: 0}, Min: 1, Max: 200, Full: series}

	scores, err := RunAll(context.Background(), o, reactive, oracle)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r, orc := scores[0], scores[1]

	if orc.UnmetDemand >= r.UnmetDemand {
		t.Fatalf("oracle (%.1f unmet) should beat reactive (%.1f) on a ramp",
			orc.UnmetDemand, r.UnmetDemand)
	}
	if orc.UnmetSteps != 0 {
		t.Fatalf("perfect foresight should never be short on a smooth ramp, got %d steps", orc.UnmetSteps)
	}
}

func TestWarmupIsExcludedFromScoring(t *testing.T) {
	series := make([]float64, 100)
	for i := range series {
		series[i] = 50
	}
	o := opts(series, func(o *Options) { o.Warmup = 40 })

	score, err := Run(context.Background(), o, Static{Replicas: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if score.Steps != 60 {
		t.Fatalf("expected 60 scored steps after a 40-step warmup, got %d", score.Steps)
	}
}

func TestEvalEveryControlsDecisionCadence(t *testing.T) {
	series := make([]float64, 100)
	rec := &recordingStrategy{target: 2}
	o := opts(series, func(o *Options) { o.EvalEvery = 10 })

	if _, err := Run(context.Background(), o, rec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.calls != 10 {
		t.Fatalf("expected 10 decisions at evalEvery=10 over 100 steps, got %d", rec.calls)
	}
}

type failingStrategy struct {
	after int
	calls int
}

func (f *failingStrategy) Name() string { return "failing" }
func (f *failingStrategy) Decide(context.Context, []float64, int, int32) (int32, error) {
	f.calls++
	if f.calls > f.after {
		return 0, errors.New("backend down")
	}
	return 7, nil
}

// TestFailedDecisionsHoldThePreviousCount mirrors the controller: it would
// rather leave a workload alone than resize it from bad data.
func TestFailedDecisionsHoldThePreviousCount(t *testing.T) {
	series := make([]float64, 60)
	for i := range series {
		series[i] = 50
	}
	o := opts(series, func(o *Options) { o.InitialReplicas = 7; o.LeadTime = 0 })

	score, err := Run(context.Background(), o, &failingStrategy{after: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if score.Errors == 0 {
		t.Fatal("expected failed decisions to be counted")
	}
	if score.AvgReplicas() != 7 {
		t.Fatalf("expected the count to hold at 7, got %.2f", score.AvgReplicas())
	}
}

func TestInvalidOptions(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Options)
	}{
		{"empty series", func(o *Options) { o.Series = nil }},
		{"zero resolution", func(o *Options) { o.Resolution = 0 }},
		{"zero capacity", func(o *Options) { o.PerReplica = 0 }},
		{"warmup consumes the series", func(o *Options) { o.Warmup = len(o.Series) }},
		{"zero evalEvery", func(o *Options) { o.EvalEvery = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Run(context.Background(), opts(make([]float64, 10), tt.mut), Static{Replicas: 1}); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestQuantile(t *testing.T) {
	xs := []float64{0, 0, 0, 0, 10}
	if got := quantile(xs, 0.95); math.Abs(got-8) > 1e-9 {
		t.Fatalf("p95 = %v, want 8", got)
	}
	if got := quantile(nil, 0.5); got != 0 {
		t.Fatalf("empty p50 = %v, want 0", got)
	}
}

func TestReportMentionsEveryStrategy(t *testing.T) {
	series := make([]float64, 60)
	for i := range series {
		series[i] = 50 + 20*math.Sin(float64(i)/6)
	}
	o := opts(series)
	scores, err := RunAll(context.Background(), o,
		Static{Replicas: 8},
		Reactive{PerReplica: 10, Buffer: policy.Amount{Value: 1}, Min: 1, Max: 50},
		Oracle{PerReplica: 10, LeadSteps: 5, Buffer: policy.Amount{Value: 1}, Min: 1, Max: 50, Full: series},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	report := Report(scores, o, nil)
	for _, s := range scores {
		if !contains(report, s.Strategy) {
			t.Fatalf("report omits %q:\n%s", s.Strategy, report)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

var _ = fmt.Sprintf
